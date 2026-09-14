// Package axiapi is the typed AXI application boundary: the run state, gate,
// and outcome vocabulary AXI reports, expressed as Go values instead of
// terminal output.
//
// It exists so a second consumer - today the MCP gateway in internal/mcp - can
// drive the same pipeline as `no-mistakes axi` without reparsing that command's
// human- and agent-facing rendering, and without re-deriving the outcome
// semantics that rendering encodes. internal/cli delegates to the decision
// functions here so there is exactly one answer to "did this run pass", "did it
// skip validation the operator did not ask it to skip", and "are its checks
// green"; a second implementation that drifted would let one surface report a
// clean pass for a run the other reports as overridden or skipped.
//
// Everything here is scoped by an explicit repository directory. The package
// never changes the process working directory: the MCP server is long-lived and
// serves concurrent calls, so a global chdir would race.
package axiapi

import (
	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepState is the part of a pipeline step the outcome decisions read.
type StepState struct {
	Name       string
	Status     string
	SkipReason string
}

// AutomaticSkip is a validation step the pipeline skipped on its own, with the
// reason it recorded.
type AutomaticSkip struct {
	Step   string
	Reason string
}

// TerminalStatus reports whether a run has reached a final state.
func TerminalStatus(status string) bool {
	return types.RunStatus(status).Terminal()
}

// AutomaticSkips lists the steps the pipeline skipped for itself. Only
// publication and external verification skip automatically, and only with a
// recorded reason; an explicitly requested skip carries no automatic cause and
// is not reported here.
func AutomaticSkips(steps []StepState) []AutomaticSkip {
	var skips []AutomaticSkip
	for _, s := range steps {
		if s.Status == string(types.StepStatusSkipped) && s.SkipReason != "" &&
			(s.Name == string(types.StepPR) || s.Name == string(types.StepCI)) {
			skips = append(skips, AutomaticSkip{Step: s.Name, Reason: s.SkipReason})
		}
	}
	return skips
}

// Outcome maps a terminal run status onto the agent-facing outcome word,
// qualifying a completed run whose external checks were overridden by a human
// or whose publication/verification skipped automatically. Both qualifications
// exist so a deliberate override and a silently unpublished run cannot read
// identically to a genuinely green one.
func Outcome(status, overrideReason string, skips []AutomaticSkip) string {
	word := outcomeWord(status)
	if word != "passed" {
		return word
	}
	if overrideReason != "" {
		return "passed-with-override"
	}
	if len(skips) > 0 {
		return "passed-with-skips"
	}
	return word
}

func outcomeWord(status string) string {
	switch types.RunStatus(status) {
	case types.RunCompleted:
		return "passed"
	case types.RunFailed:
		return "failed"
	case types.RunCancelled:
		return "cancelled"
	case types.RunCIMonitorInterrupted:
		return "ci-monitor-interrupted"
	default:
		return status
	}
}

// CIReadyToMerge reports whether the CI step is actively monitoring and the
// daemon has persisted checks-passed readiness. It is the only source of
// "checks-passed": no other run state may be promoted to it.
func CIReadyToMerge(steps []StepState, ciReady, ciReadyNoCI bool) bool {
	activity := cimonitor.FromAuthoritative(ciReady, ciReadyNoCI, nil)
	for _, s := range steps {
		if s.Name == string(types.StepCI) {
			return s.Status == string(types.StepStatusRunning) && activity.Ready
		}
	}
	return false
}
