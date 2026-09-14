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
		if got := Outcome(tc.status, tc.override, tc.skips); got != tc.want {
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
