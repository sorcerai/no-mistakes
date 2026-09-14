package mcp

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/axiapi"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestBoundedWaitClampsBeforeDurationOverflow(t *testing.T) {
	if got := boundedWait(math.MaxInt); got != maxWait {
		t.Fatalf("boundedWait(MaxInt) = %s, want %s", got, maxWait)
	}
}

// fakeAXI records what the gateway asked AXI to do, so a test can prove a
// refusal never reached the pipeline.
type fakeAXI struct {
	status      *axiapi.RunState
	run         *axiapi.RunState
	respond     *axiapi.RunState
	logs        *axiapi.StepLog
	sync        *axiapi.SyncState
	syncApplied *axiapi.SyncState
	doctor      *axiapi.DoctorReport
	gate        gatecontext.Result

	err error

	runCalls     []axiapi.RunRequest
	respondCalls []axiapi.RespondRequest
	syncCalls    []axiapi.SyncRequest
}

func (f *fakeAXI) Status(_ context.Context, repoPath, runID string) (*axiapi.RunState, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.status, nil
}

func (f *fakeAXI) Run(_ context.Context, req axiapi.RunRequest) (*axiapi.RunState, error) {
	f.runCalls = append(f.runCalls, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.run, nil
}

func (f *fakeAXI) Respond(_ context.Context, req axiapi.RespondRequest) (*axiapi.RunState, error) {
	f.respondCalls = append(f.respondCalls, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.respond, nil
}

func (f *fakeAXI) Logs(_ context.Context, req axiapi.LogsRequest) (*axiapi.StepLog, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.logs, nil
}

func (f *fakeAXI) Sync(_ context.Context, req axiapi.SyncRequest) (*axiapi.SyncState, error) {
	f.syncCalls = append(f.syncCalls, req)
	if f.err != nil {
		return nil, f.err
	}
	if (req.Apply || req.Recover) && f.syncApplied != nil {
		return f.syncApplied, nil
	}
	return f.sync, nil
}

func (f *fakeAXI) Doctor(_ context.Context, repoPath string) (*axiapi.DoctorReport, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.doctor, nil
}

func (f *fakeAXI) GateContext(_ context.Context, repoPath string) (gatecontext.Result, error) {
	return f.gate, nil
}

func newService(t *testing.T, axi *fakeAXI) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	repo := mkGitRepo(t, filepath.Join(root, "project"))
	policy, err := NewRepositoryPolicy([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{AXI: axi, Policy: policy}, repo
}

func TestStatusReturnsNormalizedReceipt(t *testing.T) {
	axi := &fakeAXI{status: &axiapi.RunState{RunID: "01ABC", Branch: "feature/x", HeadSHA: fullSHA, Status: string(types.RunRunning)}}
	svc, repo := newService(t, axi)
	got := svc.Status(context.Background(), StatusInput{RepoPath: repo})
	if !got.OK || got.RunID != "01ABC" || got.HeadSHA != fullSHA {
		t.Fatalf("receipt = %#v", got)
	}
}

// TestEveryToolRefusesAnUnallowedRepository is the allowlist applied at the
// tool boundary: no operation, read or write, reaches AXI for a path outside
// the configured roots.
func TestEveryToolRefusesAnUnallowedRepository(t *testing.T) {
	axi := &fakeAXI{}
	svc, _ := newService(t, axi)
	outside := mkGitRepo(t, filepath.Join(t.TempDir(), "elsewhere"))

	receipts := map[string]*Receipt{
		"status":  svc.Status(context.Background(), StatusInput{RepoPath: outside}),
		"run":     svc.Run(context.Background(), RunInput{RepoPath: outside, Intent: "goal"}),
		"respond": svc.Respond(context.Background(), RespondInput{RepoPath: outside, Action: "approve"}),
		"logs":    svc.Logs(context.Background(), LogsInput{RepoPath: outside, Step: "review"}),
		"sync":    svc.Sync(context.Background(), SyncInput{RepoPath: outside}),
		"doctor":  svc.Doctor(context.Background(), DoctorInput{RepoPath: outside}),
	}
	for name, got := range receipts {
		if got.OK {
			t.Errorf("%s: accepted a repository outside every configured root", name)
		}
		if got.Error == nil || got.Error.Code != CodeRepoNotAllowed {
			t.Errorf("%s: error = %#v, want %s", name, got.Error, CodeRepoNotAllowed)
		}
	}
	if len(axi.runCalls)+len(axi.respondCalls)+len(axi.syncCalls) != 0 {
		t.Error("a refused repository still reached AXI")
	}
}

// TestRunRequiresIntent pins that the pipeline is never started without the
// goal the change exists to serve.
func TestRunRequiresIntent(t *testing.T) {
	axi := &fakeAXI{}
	svc, repo := newService(t, axi)
	got := svc.Run(context.Background(), RunInput{RepoPath: repo})
	if got.OK || got.Error.Code != CodeIntentRequired {
		t.Fatalf("receipt = %#v", got)
	}
	if len(axi.runCalls) != 0 {
		t.Error("a run without intent still reached AXI")
	}
}

// TestMutatingToolsRefuseNestedGateContext pins invariant 10: a caller running
// inside an active validation step must return its phase, not drive the
// pipeline. The refusal is typed and mutates nothing.
func TestMutatingToolsRefuseNestedGateContext(t *testing.T) {
	axi := &fakeAXI{gate: gatecontext.Result{Nested: true, RunID: "01XYZ", Phase: types.StepReview}}
	svc, repo := newService(t, axi)

	for name, got := range map[string]*Receipt{
		"run":     svc.Run(context.Background(), RunInput{RepoPath: repo, Intent: "goal"}),
		"respond": svc.Respond(context.Background(), RespondInput{RepoPath: repo, Action: "approve"}),
		"sync":    svc.Sync(context.Background(), SyncInput{RepoPath: repo, Apply: true}),
	} {
		if got.OK || got.Error == nil || got.Error.Code != CodeNestedGateContext {
			t.Errorf("%s: receipt = %#v, want a nested-gate refusal", name, got)
		}
	}
	if len(axi.runCalls)+len(axi.respondCalls)+len(axi.syncCalls) != 0 {
		t.Error("a nested caller still mutated through AXI")
	}
	// Reads stay available so the nested caller can still observe.
	axi.status = &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA}
	if got := svc.Status(context.Background(), StatusInput{RepoPath: repo}); !got.OK {
		t.Errorf("status must stay available to a nested caller: %#v", got)
	}
}

// TestRespondRefusesAskUserWithoutAnExplicitDecision is the safety contract:
// a finding the pipeline referred to a human cannot be progressed by the agent
// driving the gateway, and the refusal never reaches the daemon.
func TestRespondRefusesAskUserWithoutAnExplicitDecision(t *testing.T) {
	gate := &axiapi.Gate{
		Step: "review", Status: string(types.StepStatusAwaitingApproval),
		Findings: []types.Finding{
			{ID: "r1", Action: types.ActionAskUser, Description: "which behaviour is correct?"},
			{ID: "r2", Action: types.ActionAutoFix, Description: "mechanical"},
		},
	}
	for _, action := range []string{"approve", "fix", "skip"} {
		axi := &fakeAXI{status: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Gate: gate}}
		svc, repo := newService(t, axi)
		got := svc.Respond(context.Background(), RespondInput{RepoPath: repo, Action: action})
		if got.OK {
			t.Fatalf("%s: an ask-user gate was progressed without a decision", action)
		}
		if !got.RequiresUserDecision {
			t.Errorf("%s: receipt must report requires_user_decision", action)
		}
		if got.Error == nil || got.Error.Code != CodeUserDecisionRequired {
			t.Errorf("%s: error = %#v", action, got.Error)
		}
		if len(got.Findings) != 2 {
			t.Errorf("%s: the refusal must return the findings to decide on, got %d", action, len(got.Findings))
		}
		if len(axi.respondCalls) != 0 {
			t.Fatalf("%s: the response still reached the daemon", action)
		}
	}
}

// TestRespondProceedsWithAnExplicitUserDecision pins the other half: a decision
// a human actually made is forwarded verbatim.
func TestRespondProceedsWithAnExplicitUserDecision(t *testing.T) {
	axi := &fakeAXI{
		status: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Gate: &axiapi.Gate{
			Step: "review", Findings: []types.Finding{{ID: "r1", Action: types.ActionAskUser, Description: "decide"}},
		}},
		respond: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Status: string(types.RunRunning)},
	}
	svc, repo := newService(t, axi)
	got := svc.Respond(context.Background(), RespondInput{
		RepoPath: repo, Action: "fix", FindingIDs: []string{"r1"},
		UserDecision: "The maintainer chose the second behaviour.",
	})
	if !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
	if len(axi.respondCalls) != 1 {
		t.Fatalf("respond calls = %d, want 1", len(axi.respondCalls))
	}
	if axi.respondCalls[0].Action != types.ActionFix {
		t.Errorf("action = %q", axi.respondCalls[0].Action)
	}
}

// TestRespondProtectedPathRefusalAlsoNeedsADecision keeps the pipeline's own
// refusal from being answered by the gateway.
func TestRespondProtectedPathRefusalAlsoNeedsADecision(t *testing.T) {
	axi := &fakeAXI{status: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA,
		Gate: &axiapi.Gate{Step: "review", ProtectedPathRefusal: true}}}
	svc, repo := newService(t, axi)
	got := svc.Respond(context.Background(), RespondInput{RepoPath: repo, Action: "approve"})
	if got.OK || got.Error.Code != CodeUserDecisionRequired {
		t.Fatalf("receipt = %#v", got)
	}
	if len(axi.respondCalls) != 0 {
		t.Error("a protected-path refusal was answered by the gateway")
	}
}

func TestRespondRejectsAnUnknownAction(t *testing.T) {
	axi := &fakeAXI{status: &axiapi.RunState{RunID: "01ABC", Gate: &axiapi.Gate{Step: "review"}}}
	svc, repo := newService(t, axi)
	got := svc.Respond(context.Background(), RespondInput{RepoPath: repo, Action: "abort"})
	if got.OK || got.Error.Code != CodeInvalidAction {
		t.Fatalf("receipt = %#v", got)
	}
	if len(axi.respondCalls) != 0 {
		t.Error("an unsupported action reached the daemon")
	}
}

func TestRespondRefusesWhenNoGateIsOpen(t *testing.T) {
	axi := &fakeAXI{status: &axiapi.RunState{RunID: "01ABC", Status: string(types.RunRunning)}}
	svc, repo := newService(t, axi)
	got := svc.Respond(context.Background(), RespondInput{RepoPath: repo, Action: "approve"})
	if got.OK || got.Error.Code != CodeNoGate {
		t.Fatalf("receipt = %#v", got)
	}
	if len(axi.respondCalls) != 0 {
		t.Error("a response was sent with no gate open")
	}
}

// TestSyncRefusesUnlessAXIAuthorizesIt pins that sync is not a speculative
// repair: the gateway performs it only when AXI's own next action asks for it.
func TestSyncRefusesUnlessAXIAuthorizesIt(t *testing.T) {
	axi := &fakeAXI{sync: &axiapi.SyncState{State: "synchronized", NextActionCode: "run_pipeline"}}
	svc, repo := newService(t, axi)
	got := svc.Sync(context.Background(), SyncInput{RepoPath: repo, Apply: true})
	if got.OK || got.Error == nil || got.Error.Code != CodeSyncNotAuthorized {
		t.Fatalf("receipt = %#v", got)
	}
	for _, call := range axi.syncCalls {
		if call.Apply || call.Recover {
			t.Fatal("an unauthorized sync mutated the branch")
		}
	}
}

func TestSyncAppliesWhenAXIAsksForIt(t *testing.T) {
	axi := &fakeAXI{
		sync:        &axiapi.SyncState{State: "behind", NextActionCode: "sync", NextActionCommand: "no-mistakes axi sync"},
		syncApplied: &axiapi.SyncState{State: "synchronized", Changed: true},
	}
	svc, repo := newService(t, axi)
	got := svc.Sync(context.Background(), SyncInput{RepoPath: repo, Apply: true})
	if !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
	applied := false
	for _, call := range axi.syncCalls {
		applied = applied || call.Apply
	}
	if !applied {
		t.Fatal("an authorized sync was not applied")
	}
}

// TestSyncRecoverRequiresTheRecoveryNextAction pins that custody recovery is
// only offered when AXI offers it, and that a plain sync authorization is not
// enough to discard unpublished commits.
func TestSyncRecoverRequiresTheRecoveryNextAction(t *testing.T) {
	axi := &fakeAXI{sync: &axiapi.SyncState{State: "behind", NextActionCode: "sync"}}
	svc, repo := newService(t, axi)
	got := svc.Sync(context.Background(), SyncInput{RepoPath: repo, Recover: true, KeepLocal: true})
	if got.OK || got.Error.Code != CodeSyncNotAuthorized {
		t.Fatalf("receipt = %#v", got)
	}

	axi2 := &fakeAXI{
		sync:        &axiapi.SyncState{State: "pipeline_owned", NextActionCode: "recover_custody"},
		syncApplied: &axiapi.SyncState{State: "user_owned", Recovered: true},
	}
	svc2, repo2 := newService(t, axi2)
	if got := svc2.Sync(context.Background(), SyncInput{RepoPath: repo2, Recover: true, KeepLocal: true}); !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
}

// TestSyncCheckOnlyNeverMutates keeps the read path open in every state.
func TestSyncCheckOnlyNeverMutates(t *testing.T) {
	axi := &fakeAXI{sync: &axiapi.SyncState{State: "pipeline_owned", NextActionCode: "inspect_and_reconcile_manually"}}
	svc, repo := newService(t, axi)
	got := svc.Sync(context.Background(), SyncInput{RepoPath: repo})
	if !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
	for _, call := range axi.syncCalls {
		if call.Apply || call.Recover {
			t.Fatal("a check-only sync mutated the branch")
		}
	}
}

func TestLogsAreBounded(t *testing.T) {
	axi := &fakeAXI{logs: &axiapi.StepLog{RunID: "01ABC", Step: "review", Lines: []string{"a", "b"}, TotalLines: 400, Truncated: true}}
	svc, repo := newService(t, axi)
	got := svc.Logs(context.Background(), LogsInput{RepoPath: repo, Step: "review"})
	if !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
	if got.Extra["total_lines"] != 400 || got.Extra["truncated"] != true {
		t.Errorf("data = %#v", got.Extra)
	}
}

// TestLogsCapsAnUnboundedRequest keeps a caller from asking for an entire log.
func TestLogsCapsAnUnboundedRequest(t *testing.T) {
	axi := &fakeAXI{logs: &axiapi.StepLog{RunID: "01ABC", Step: "review"}}
	svc, repo := newService(t, axi)
	svc.Logs(context.Background(), LogsInput{RepoPath: repo, Step: "review", TailLines: 100000})
	// The service passes its own cap to AXI rather than the caller's number.
	if got := svc.logTail(100000); got > MaxLogTailLines {
		t.Errorf("tail = %d, want at most %d", got, MaxLogTailLines)
	}
	if got := svc.logTail(0); got != DefaultLogTailLines {
		t.Errorf("default tail = %d, want %d", got, DefaultLogTailLines)
	}
}

func TestDoctorReportsDiagnostics(t *testing.T) {
	axi := &fakeAXI{doctor: &axiapi.DoctorReport{
		RepoInitialized: true, DaemonRunning: true, DefaultBranch: "main",
		Agents: []axiapi.AgentCheck{{Name: "claude", Available: true}, {Name: "codex"}},
	}}
	svc, repo := newService(t, axi)
	got := svc.Doctor(context.Background(), DoctorInput{RepoPath: repo})
	if !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
	if got.Extra["repo_initialized"] != true || got.Extra["daemon_running"] != true {
		t.Errorf("data = %#v", got.Extra)
	}
}

// TestAXIErrorsBecomeTypedReceipts keeps a pipeline failure actionable rather
// than an opaque protocol error.
func TestAXIErrorsBecomeTypedReceipts(t *testing.T) {
	axi := &fakeAXI{err: axiapi.ErrRepoNotInitialized}
	svc, repo := newService(t, axi)
	got := svc.Status(context.Background(), StatusInput{RepoPath: repo})
	if got.OK || got.Error == nil || got.Error.Code != CodeRepoNotInitialized {
		t.Fatalf("receipt = %#v", got)
	}
	if got.Error.Remediation == "" {
		t.Error("an uninitialized repository must keep its no-mistakes remediation")
	}

	axi2 := &fakeAXI{err: &axiapi.DirtyWorktreeError{Untracked: []string{"scratch.txt"}}}
	svc2, repo2 := newService(t, axi2)
	got2 := svc2.Run(context.Background(), RunInput{RepoPath: repo2, Intent: "goal"})
	if got2.OK || got2.Error.Code != CodeDirtyWorktree {
		t.Fatalf("receipt = %#v", got2)
	}

	axi3 := &fakeAXI{err: errors.New("boom")}
	svc3, repo3 := newService(t, axi3)
	got3 := svc3.Status(context.Background(), StatusInput{RepoPath: repo3})
	if got3.OK || got3.Error.Code != CodeAXIError {
		t.Fatalf("receipt = %#v", got3)
	}
}

// TestRunNeverAutoApproves pins that no gateway input can turn on AXI's
// gate auto-resolution: the gateway always hands a gate back.
func TestRunNeverAutoApproves(t *testing.T) {
	axi := &fakeAXI{run: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Status: string(types.RunRunning),
		Gate: &axiapi.Gate{Step: "review", Findings: []types.Finding{{ID: "r1", Action: types.ActionAutoFix, Description: "x"}}}}}
	svc, repo := newService(t, axi)
	got := svc.Run(context.Background(), RunInput{RepoPath: repo, Intent: "goal"})
	if got.State != StateAwaitingDecision {
		t.Fatalf("state = %q, want the gate handed back", got.State)
	}
	if len(axi.runCalls) != 1 {
		t.Fatalf("run calls = %d", len(axi.runCalls))
	}
	if axi.runCalls[0].Intent != "goal" {
		t.Errorf("intent = %q, want the caller's own words", axi.runCalls[0].Intent)
	}
}

// TestSyncRejectsContradictoryRequests keeps a mutation the caller did not ask
// for from being silently chosen: apply and recover are different mutations
// with different authorizations, and keep_local only qualifies a recovery.
func TestSyncRejectsContradictoryRequests(t *testing.T) {
	for name, in := range map[string]SyncInput{
		"apply and recover":          {Apply: true, Recover: true},
		"keep_local without recover": {Apply: true, KeepLocal: true},
	} {
		axi := &fakeAXI{sync: &axiapi.SyncState{State: "behind", NextActionCode: "sync"}}
		svc, repo := newService(t, axi)
		in.RepoPath = repo
		got := svc.Sync(context.Background(), in)
		if got.OK || got.Error == nil || got.Error.Code != CodeInvalidAction {
			t.Errorf("%s: receipt = %#v", name, got)
		}
		if len(axi.syncCalls) != 0 {
			t.Errorf("%s: a contradictory request still reached AXI", name)
		}
	}
}

// TestRespondForwardsTheDecisionFlagToAXI pins that the boundary enforcing the
// ask-user refusal against the live gate is told whether a human decided.
func TestRespondForwardsTheDecisionFlagToAXI(t *testing.T) {
	gate := &axiapi.Gate{Step: "review", Findings: []types.Finding{{ID: "r1", Action: types.ActionAutoFix, Description: "x"}}}
	axi := &fakeAXI{
		status:  &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Gate: gate},
		respond: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Status: string(types.RunRunning)},
	}
	svc, repo := newService(t, axi)
	if got := svc.Respond(context.Background(), RespondInput{RepoPath: repo, Action: "approve"}); !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
	if axi.respondCalls[0].UserDecisionGiven {
		t.Error("no decision was supplied, but the flag was set")
	}

	svc2, repo2 := newService(t, axi)
	if got := svc2.Respond(context.Background(), RespondInput{RepoPath: repo2, Action: "approve", UserDecision: "the maintainer said yes"}); !got.OK {
		t.Fatalf("receipt = %#v", got)
	}
	if !axi.respondCalls[1].UserDecisionGiven {
		t.Error("a supplied decision did not reach AXI")
	}
}

// TestAXIUserDecisionRefusalStaysTyped pins that a refusal raised against the
// live gate - the run advanced into an ask-user gate after this layer read it -
// still reaches the caller as the decision-required error, not an opaque one.
func TestAXIUserDecisionRefusalStaysTyped(t *testing.T) {
	axi := &fakeAXI{
		status: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA,
			Gate: &axiapi.Gate{Step: "review", Findings: []types.Finding{{ID: "r1", Action: types.ActionAutoFix, Description: "x"}}}},
		err: axiapi.ErrUserDecisionRequired,
	}
	svc, repo := newService(t, axi)
	got := svc.Respond(context.Background(), RespondInput{RepoPath: repo, Action: "approve"})
	if got.OK || got.Error == nil || got.Error.Code != CodeUserDecisionRequired {
		t.Fatalf("receipt = %#v", got)
	}
}

// TestDefaultBranchRefusalIsTyped pins the "no direct default-branch push"
// non-goal as something a caller can branch on, not an opaque failure.
func TestDefaultBranchRefusalIsTyped(t *testing.T) {
	axi := &fakeAXI{err: axiapi.ErrDefaultBranch}
	svc, repo := newService(t, axi)
	got := svc.Run(context.Background(), RunInput{RepoPath: repo, Intent: "goal"})
	if got.OK || got.Error == nil || got.Error.Code != CodeDefaultBranch {
		t.Fatalf("receipt = %#v", got)
	}
	if got.Error.Remediation == "" {
		t.Error("the refusal must say what to do instead")
	}
}
