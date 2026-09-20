// Package mcp is the No-Mistakes MCP gateway: a thin, deterministic boundary
// that lets an external agent surface hand repository mutation work to
// No-Mistakes instead of writing to a forge directly.
//
// It is an orchestration adapter, not a second workflow engine. Every tool maps
// to one AXI control operation through internal/axiapi, normalizes the result
// to a stable machine receipt, and leaves every state transition to
// No-Mistakes. Nothing here publishes to a forge, merges a pull request, pushes
// to a default branch, or answers a gate the pipeline referred to a human.
package mcp

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/axiapi"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Operations, as they appear in a receipt.
const (
	OpStatus  = "status"
	OpRun     = "run"
	OpRespond = "respond"
	OpLogs    = "logs"
	OpSync    = "sync"
	OpDoctor  = "doctor"
)

// Receipt states. The terminal ones are AXI's own outcome vocabulary, carried
// through unchanged: an overridden or skipped run must never be reported with
// the same word as a genuinely clean one.
const (
	StateIdle                 = "idle"
	StateRunning              = "running"
	StateAwaitingDecision     = "awaiting_decision"
	StateChecksPassed         = "checks-passed"
	StatePassed               = "passed"
	StatePassedWithSkips      = "passed-with-skips"
	StatePassedWithOverride   = "passed-with-override"
	StateFailed               = "failed"
	StateCancelled            = "cancelled"
	StateCIMonitorInterrupted = "ci-monitor-interrupted"
)

// Finding is one review, test, or lint finding as the gateway reports it. The
// action classification is the pipeline's own and is never rewritten here.
type Finding struct {
	ID          string `json:"id,omitempty"`
	Severity    string `json:"severity,omitempty"`
	Action      string `json:"action,omitempty"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Description string `json:"description"`
}

// AutomaticSkip is a validation step the pipeline skipped for itself.
type AutomaticSkip struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// NextAction tells the caller what it may do next, and with which arguments.
type NextAction struct {
	Code    string   `json:"code"`
	Allowed []string `json:"allowed,omitempty"`
}

// ErrorBody is a typed, actionable failure.
type ErrorBody struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// Receipt is the normalized response every tool returns.
type Receipt struct {
	OK        bool   `json:"ok"`
	Operation string `json:"operation"`
	RepoPath  string `json:"repo_path,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Branch    string `json:"branch,omitempty"`
	// HeadSHA is the full commit, never abbreviated: an abbreviated SHA cannot
	// be compared against a forge, which makes it useless as evidence.
	HeadSHA string `json:"head_sha,omitempty"`
	State   string `json:"state,omitempty"`
	Step    string `json:"step,omitempty"`
	PRURL   string `json:"pr_url,omitempty"`
	// CI is "checks-passed" only when AXI itself reports established CI
	// readiness. Every other state leaves it null; no outcome is ever promoted
	// to a CI verdict it did not earn.
	CI *string `json:"ci"`
	// RequiresUserDecision is true when the gate holds an ask-user finding or a
	// protected-path refusal. Such a gate is never the gateway's to answer.
	RequiresUserDecision bool            `json:"requires_user_decision"`
	Findings             []Finding       `json:"findings,omitempty"`
	NextAction           *NextAction     `json:"next_action,omitempty"`
	AutomaticSkips       []AutomaticSkip `json:"automatic_skips,omitempty"`
	Warnings             []string        `json:"warnings,omitempty"`
	// RunError is the pipeline's own failure message, when it recorded one.
	RunError string `json:"run_error,omitempty"`
	// Extra carries operation-specific payloads (logs, sync state, doctor).
	Extra map[string]any `json:"data,omitempty"`
	Error *ErrorBody     `json:"error,omitempty"`
}

// NewReceipt normalizes an AXI run snapshot. It derives nothing about the
// outcome itself: the state word, the automatic skips, and CI readiness all
// come from internal/axiapi, which is the same source the `no-mistakes axi`
// command reports from.
func NewReceipt(operation, repoPath string, state *axiapi.RunState) *Receipt {
	r := &Receipt{OK: true, Operation: operation, RepoPath: repoPath}
	if state == nil {
		r.State = StateIdle
		r.NextAction = &NextAction{Code: "run"}
		return r
	}
	r.RunID = state.RunID
	r.Branch = state.Branch
	r.HeadSHA = state.HeadSHA
	r.PRURL = state.PRURL
	r.RunError = state.RunError
	for _, skip := range state.AutomaticSkips {
		r.AutomaticSkips = append(r.AutomaticSkips, AutomaticSkip{Step: skip.Step, Reason: skip.Reason})
	}

	switch {
	case state.RunID == "":
		r.State = StateIdle
		r.NextAction = &NextAction{Code: "run"}
	case state.Terminal:
		r.State = state.Outcome
		if state.CIOverrideReason != "" {
			r.Warnings = append(r.Warnings, "A human approved past a live CI failure: "+state.CIOverrideReason)
		}
	case state.Gate != nil:
		r.State = StateAwaitingDecision
		r.Step = state.Gate.Step
		r.RequiresUserDecision = axiapi.RequiresUserDecision(state.Gate)
		for _, f := range state.Gate.Findings {
			r.Findings = append(r.Findings, Finding{
				ID: f.ID, Severity: f.Severity, Action: f.ActionOrDefault(),
				File: f.File, Line: f.Line, Description: f.Description,
			})
		}
		r.NextAction = &NextAction{
			Code:    "respond",
			Allowed: []string{string(types.ActionApprove), string(types.ActionFix), string(types.ActionSkip)},
		}
		if r.RequiresUserDecision {
			r.Warnings = append(r.Warnings, "This gate holds findings the pipeline referred to a human; return them for a decision rather than answering them.")
		}
	case state.CIReady:
		r.State = StateChecksPassed
		checks := StateChecksPassed
		r.CI = &checks
		// There is no merge tool. CI readiness is where the gateway stops and a
		// human decides.
		r.NextAction = &NextAction{Code: "await_human_merge"}
	default:
		r.State = StateRunning
		r.NextAction = &NextAction{Code: "status"}
	}

	if state.TestOverrideReason != "" {
		r.Warnings = append(r.Warnings, "A human approved a Test exception: "+state.TestOverrideReason)
	}

	if len(r.AutomaticSkips) > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"%d validation step(s) did not run; this outcome establishes neither CI readiness nor a code failure. See automatic_skips.",
			len(r.AutomaticSkips)))
	}
	if state.WaitElapsed && !state.Terminal && !state.CIReady && state.Gate == nil {
		// A bounded hold ending is not a pipeline failure. The run is still in
		// flight, so the receipt says so and points at the reattach.
		r.Warnings = append(r.Warnings, "The bounded wait elapsed while the run was still in flight; this is not a failure. Reattach to keep driving it.")
		r.NextAction = &NextAction{Code: "reattach"}
	}
	return r
}

// NewErrorReceipt renders a typed refusal.
func NewErrorReceipt(operation, repoPath string, policyErr *PolicyError) *Receipt {
	return &Receipt{
		OK:        false,
		Operation: operation,
		RepoPath:  repoPath,
		Error:     &ErrorBody{Code: policyErr.Code, Message: policyErr.Message, Remediation: policyErr.Remediation},
	}
}
