package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/axiapi"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func decode(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

const fullSHA = "0123456789abcdef0123456789abcdef01234567"

func TestReceiptForIdleRepository(t *testing.T) {
	got := decode(t, NewReceipt(OpStatus, "/repos/x", &axiapi.RunState{Branch: "feature/x"}))
	if got["state"] != StateIdle {
		t.Errorf("state = %v, want %v", got["state"], StateIdle)
	}
	if got["ok"] != true {
		t.Errorf("ok = %v", got["ok"])
	}
	next, _ := got["next_action"].(map[string]any)
	if next == nil || next["code"] != "run" {
		t.Errorf("next_action = %v, want a run action", got["next_action"])
	}
}

// TestReceiptGatePreservesFindingActions pins that the three finding action
// classes survive verbatim and that an ask-user finding raises the decision
// flag the safety contract is built on.
func TestReceiptGatePreservesFindingActions(t *testing.T) {
	state := &axiapi.RunState{
		RunID: "01ABC", Branch: "feature/x", HeadSHA: fullSHA, Status: string(types.RunRunning),
		Gate: &axiapi.Gate{
			Step: "review", Status: string(types.StepStatusAwaitingApproval),
			Findings: []types.Finding{
				{ID: "r1", Severity: "error", Action: types.ActionAskUser, File: "a.go", Line: 12, Description: "decide"},
				{ID: "r2", Severity: "warning", Action: types.ActionAutoFix, File: "b.go", Description: "mechanical"},
				{ID: "r3", Severity: "info", Action: types.ActionNoOp, Description: "noted"},
			},
		},
	}
	got := decode(t, NewReceipt(OpRun, "/repos/x", state))
	if got["state"] != StateAwaitingDecision {
		t.Errorf("state = %v, want %v", got["state"], StateAwaitingDecision)
	}
	if got["requires_user_decision"] != true {
		t.Error("an ask-user finding must require an external decision")
	}
	findings, _ := got["findings"].([]any)
	if len(findings) != 3 {
		t.Fatalf("findings = %d, want 3", len(findings))
	}
	wantActions := []string{types.ActionAskUser, types.ActionAutoFix, types.ActionNoOp}
	for i, f := range findings {
		item := f.(map[string]any)
		if item["action"] != wantActions[i] {
			t.Errorf("finding %d action = %v, want %v", i, item["action"], wantActions[i])
		}
	}
	next, _ := got["next_action"].(map[string]any)
	if next == nil || next["code"] != "respond" {
		t.Fatalf("next_action = %v", got["next_action"])
	}
	allowed, _ := next["allowed"].([]any)
	if len(allowed) != 3 {
		t.Errorf("allowed = %v, want approve/fix/skip", allowed)
	}
}

// TestReceiptGateWithoutAskUserDoesNotRequireDecision keeps the safety flag
// meaningful: a purely mechanical gate is the agent's to answer.
func TestReceiptGateWithoutAskUserDoesNotRequireDecision(t *testing.T) {
	state := &axiapi.RunState{
		RunID: "01ABC", Status: string(types.RunRunning),
		Gate: &axiapi.Gate{Step: "lint", Status: string(types.StepStatusAwaitingApproval),
			Findings: []types.Finding{{ID: "l1", Action: types.ActionAutoFix, Description: "format"}}},
	}
	got := decode(t, NewReceipt(OpRun, "/repos/x", state))
	if got["requires_user_decision"] != false {
		t.Error("a gate with no ask-user finding must not require an external decision")
	}
}

// TestReceiptProtectedPathRefusalRequiresDecision pins that a refusal the
// pipeline declined to resolve is never the gateway's to answer either.
func TestReceiptProtectedPathRefusalRequiresDecision(t *testing.T) {
	state := &axiapi.RunState{
		RunID: "01ABC", Status: string(types.RunRunning),
		Gate: &axiapi.Gate{Step: "review", Status: string(types.StepStatusAwaitingApproval), ProtectedPathRefusal: true},
	}
	got := decode(t, NewReceipt(OpRun, "/repos/x", state))
	if got["requires_user_decision"] != true {
		t.Error("a protected-path refusal must require an external decision")
	}
}

// TestReceiptChecksPassedCarriesPRAndFullHead is acceptance criterion 7's
// receipt: the terminal handoff a caller reports back must be verifiable.
func TestReceiptChecksPassedCarriesPRAndFullHead(t *testing.T) {
	state := &axiapi.RunState{
		RunID: "01ABC", Branch: "feature/x", HeadSHA: fullSHA, Status: string(types.RunRunning),
		PRURL: "https://example.test/o/r/pull/1", CIReady: true,
	}
	got := decode(t, NewReceipt(OpRun, "/repos/x", state))
	if got["state"] != StateChecksPassed {
		t.Errorf("state = %v, want %v", got["state"], StateChecksPassed)
	}
	if got["ci"] != StateChecksPassed {
		t.Errorf("ci = %v, want checks-passed", got["ci"])
	}
	if got["pr_url"] != "https://example.test/o/r/pull/1" {
		t.Errorf("pr_url = %v", got["pr_url"])
	}
	if got["head_sha"] != fullSHA {
		t.Errorf("head_sha = %v, want the full SHA", got["head_sha"])
	}
}

// TestReceiptPassedWithSkipsCannotMasqueradeAsCIReady pins invariants 11-13:
// skipped validation is exposed, and a skipped outcome never reports CI.
func TestReceiptPassedWithSkipsCannotMasqueradeAsCIReady(t *testing.T) {
	state := &axiapi.RunState{
		RunID: "01ABC", HeadSHA: fullSHA, Status: string(types.RunCompleted), Terminal: true,
		Outcome:        "passed-with-skips",
		AutomaticSkips: []axiapi.AutomaticSkip{{Step: "ci", Reason: "repository declares no CI"}},
	}
	got := decode(t, NewReceipt(OpRun, "/repos/x", state))
	if got["state"] != StatePassedWithSkips {
		t.Errorf("state = %v, want %v", got["state"], StatePassedWithSkips)
	}
	if got["ci"] != nil {
		t.Errorf("ci = %v, want null for a run whose checks never ran", got["ci"])
	}
	skips, _ := got["automatic_skips"].([]any)
	if len(skips) != 1 {
		t.Fatalf("automatic_skips = %v, want the skipped CI step", got["automatic_skips"])
	}
	if skips[0].(map[string]any)["step"] != "ci" {
		t.Errorf("skip = %v", skips[0])
	}
	warnings, _ := got["warnings"].([]any)
	if len(warnings) == 0 {
		t.Error("a skipped validation step must be warned about, not just listed")
	}
}

// TestReceiptDistinguishesEveryTerminalOutcome pins that the outcome vocabulary
// AXI can produce reaches the receipt intact - collapsing an override or an
// interruption onto a plain pass would report a run as clean that was not.
func TestReceiptDistinguishesEveryTerminalOutcome(t *testing.T) {
	for _, outcome := range []string{
		StatePassed, StatePassedWithSkips, StatePassedWithOverride,
		StateFailed, StateCancelled, StateCIMonitorInterrupted,
	} {
		state := &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Terminal: true, Outcome: outcome}
		got := decode(t, NewReceipt(OpRun, "/repos/x", state))
		if got["state"] != outcome {
			t.Errorf("state = %v, want %v", got["state"], outcome)
		}
		if got["ci"] != nil {
			t.Errorf("%s: ci = %v, want null", outcome, got["ci"])
		}
	}
}

// TestReceiptWaitElapsedIsNotAFailure pins that a bounded hold ending reports a
// run that is still in flight, with the reattach action - never a terminal
// state AXI did not report.
func TestReceiptWaitElapsedIsNotAFailure(t *testing.T) {
	state := &axiapi.RunState{RunID: "01ABC", Branch: "feature/x", HeadSHA: fullSHA,
		Status: string(types.RunRunning), WaitElapsed: true}
	got := decode(t, NewReceipt(OpRun, "/repos/x", state))
	if got["state"] != StateRunning {
		t.Errorf("state = %v, want %v", got["state"], StateRunning)
	}
	if got["ok"] != true {
		t.Error("an elapsed wait is not a failed call")
	}
	warnings, _ := got["warnings"].([]any)
	if len(warnings) == 0 || !strings.Contains(strings.ToLower(warnings[0].(string)), "reattach") {
		t.Errorf("warnings = %v, want reattach guidance", got["warnings"])
	}
	next, _ := got["next_action"].(map[string]any)
	if next == nil || next["code"] != "reattach" {
		t.Errorf("next_action = %v, want reattach", got["next_action"])
	}
}

func TestReceiptWaitElapsedPreservesSettledState(t *testing.T) {
	for name, tc := range map[string]struct {
		state               *axiapi.RunState
		wantState, wantNext string
	}{
		"terminal": {state: &axiapi.RunState{RunID: "01ABC", Terminal: true, Outcome: StatePassed, WaitElapsed: true}, wantState: StatePassed},
		"ci ready": {state: &axiapi.RunState{RunID: "01ABC", CIReady: true, WaitElapsed: true}, wantState: StateChecksPassed, wantNext: "await_human_merge"},
		"gate":     {state: &axiapi.RunState{RunID: "01ABC", Gate: &axiapi.Gate{Step: "test"}, WaitElapsed: true}, wantState: StateAwaitingDecision, wantNext: "respond"},
	} {
		t.Run(name, func(t *testing.T) {
			got := decode(t, NewReceipt(OpRun, "/repos/x", tc.state))
			if got["state"] != tc.wantState {
				t.Fatalf("state = %v, want %q", got["state"], tc.wantState)
			}
			next, _ := got["next_action"].(map[string]any)
			if tc.wantNext == "" {
				if next != nil {
					t.Fatalf("next_action = %v, want none", got["next_action"])
				}
			} else if next == nil || next["code"] != tc.wantNext {
				t.Fatalf("next_action = %v, want %q", got["next_action"], tc.wantNext)
			}
		})
	}
}

func TestErrorReceiptIsTypedAndActionable(t *testing.T) {
	got := decode(t, NewErrorReceipt(OpRun, "/repos/x", &PolicyError{
		Code: CodeRepoNotAllowed, Message: "outside", Remediation: "add the root",
	}))
	if got["ok"] != false {
		t.Error("an error receipt must not report ok")
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("missing error object: %v", got)
	}
	if errObj["code"] != CodeRepoNotAllowed || errObj["remediation"] != "add the root" {
		t.Errorf("error = %v", errObj)
	}
}

// TestReceiptHeadSHAIsNeverAbbreviated guards the one field a caller cannot
// re-derive: an abbreviated SHA cannot be compared against a forge.
func TestReceiptHeadSHAIsNeverAbbreviated(t *testing.T) {
	state := &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Status: string(types.RunRunning)}
	got := decode(t, NewReceipt(OpStatus, "/repos/x", state))
	if got["head_sha"] != fullSHA {
		t.Fatalf("head_sha = %v, want %q", got["head_sha"], fullSHA)
	}
}
