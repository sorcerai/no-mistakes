package axiapi

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{string(types.RunRunning), false},
		{string(types.RunCompleted), true},
		{string(types.RunFailed), true},
		{string(types.RunCancelled), true},
	} {
		if got := TerminalStatus(tc.status); got != tc.want {
			t.Errorf("TerminalStatus(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// TestAutomaticSkips pins the classification of a skip the pipeline decided for
// itself: only publication and verification skip automatically, and only with a
// recorded reason. An explicitly requested skip carries no reason and must not
// be reported as automatic.
func TestAutomaticSkips(t *testing.T) {
	steps := []StepState{
		{Name: string(types.StepReview), Status: string(types.StepStatusSkipped), SkipReason: ""},
		{Name: string(types.StepPR), Status: string(types.StepStatusSkipped), SkipReason: "no forge configured"},
		{Name: string(types.StepCI), Status: string(types.StepStatusSkipped), SkipReason: "repository declares no CI"},
		{Name: string(types.StepLint), Status: string(types.StepStatusCompleted)},
	}
	want := []AutomaticSkip{
		{Step: string(types.StepPR), Reason: "no forge configured"},
		{Step: string(types.StepCI), Reason: "repository declares no CI"},
	}
	if got := AutomaticSkips(steps); !reflect.DeepEqual(got, want) {
		t.Errorf("AutomaticSkips = %#v, want %#v", got, want)
	}
}

func TestOutcome(t *testing.T) {
	skips := []AutomaticSkip{{Step: string(types.StepCI), Reason: "repository declares no CI"}}
	for _, tc := range []struct {
		name     string
		status   string
		override string
		skips    []AutomaticSkip
		want     string
	}{
		{"completed", string(types.RunCompleted), "", nil, "passed"},
		{"failed", string(types.RunFailed), "", nil, "failed"},
		{"cancelled", string(types.RunCancelled), "", nil, "cancelled"},
		{"ci monitor interrupted", string(types.RunCIMonitorInterrupted), "", nil, "ci-monitor-interrupted"},
		{"override outranks skips", string(types.RunCompleted), "live checks still failing", skips, "passed-with-override"},
		{"automatic skips", string(types.RunCompleted), "", skips, "passed-with-skips"},
		{"failure keeps its word", string(types.RunFailed), "live checks still failing", skips, "failed"},
	} {
		if got := Outcome(tc.status, tc.override, tc.skips, ""); got != tc.want {
			t.Errorf("%s: Outcome = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCIReadyToMerge pins that CI readiness is only reported while the CI step
// is actively monitoring and the daemon persisted readiness. Nothing else -
// a completed run, a missing CI step, or an unready monitor - may read as
// checks-passed.
func TestCIReadyToMerge(t *testing.T) {
	monitoring := []StepState{{Name: string(types.StepCI), Status: string(types.StepStatusRunning)}}
	if !CIReadyToMerge(monitoring, true, false) {
		t.Error("monitoring CI step with persisted readiness should be merge-ready")
	}
	if CIReadyToMerge(monitoring, false, false) {
		t.Error("monitoring CI step without persisted readiness must not be merge-ready")
	}
	done := []StepState{{Name: string(types.StepCI), Status: string(types.StepStatusCompleted)}}
	if CIReadyToMerge(done, true, false) {
		t.Error("a completed CI step is not an open checks-passed handoff")
	}
	if CIReadyToMerge([]StepState{{Name: string(types.StepPush), Status: string(types.StepStatusRunning)}}, true, false) {
		t.Error("a run without a CI step must not be merge-ready")
	}
}

// TestRequiresUserDecision pins what the pipeline refuses to decide for itself:
// an ask-user finding, or a protected-path refusal. Nothing else needs a human.
func TestRequiresUserDecision(t *testing.T) {
	if RequiresUserDecision(nil) {
		t.Error("no gate cannot require a decision")
	}
	mechanical := &Gate{Findings: []types.Finding{
		{ID: "a", Action: types.ActionAutoFix, Description: "x"},
		{ID: "b", Action: types.ActionNoOp, Description: "y"},
	}}
	if RequiresUserDecision(mechanical) {
		t.Error("a gate with only mechanical and informational findings is the agent's to answer")
	}
	askUser := &Gate{Findings: []types.Finding{
		{ID: "a", Action: types.ActionAutoFix, Description: "x"},
		{ID: "b", Action: types.ActionAskUser, Description: "y"},
	}}
	if !RequiresUserDecision(askUser) {
		t.Error("one ask-user finding makes the whole gate a human's")
	}
	if !RequiresUserDecision(&Gate{ProtectedPathRefusal: true}) {
		t.Error("a protected-path refusal needs an explicit decision even with no findings")
	}
	// A finding with no action spelled out defaults to the pipeline's own
	// default rather than being treated as a silent ask-user.
	defaulted := &Gate{Findings: []types.Finding{{ID: "a", Description: "x"}}}
	if RequiresUserDecision(defaulted) != (types.Finding{ID: "a"}.ActionOrDefault() == types.ActionAskUser) {
		t.Error("an unspecified action must follow the shared default, not a local guess")
	}
}
