package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func writeEvidence(t *testing.T, filename string, data any) {
	t.Helper()
	dir := os.Getenv("NM_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("mkdir evidence dir: %v", err)
		return
	}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Logf("marshal evidence: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, filename), payload, 0o644); err != nil {
		t.Logf("write evidence %s: %v", filename, err)
	}
}

// These journeys prove the gateway's two central contracts - an ask-user gate
// is never resolved or lost by the gateway, and a receipt carries the PR URL and
// full head SHA while sync mutates only when AXI authorizes it - without
// inspecting the process table and without a daemon. Every observation is
// durable no-mistakes state, a git ref, or receipt content from a real
// `no-mistakes mcp serve --stdio` process. The daemon's side of a live gate
// (its executor keeps waiting while no gateway is connected) needs a running
// daemon, whose nested-gate check inspects process ancestry on every mutating
// call; that half stays with the e2e TestMCPGatewayJourney.

// startGatewayProcess launches the real `no-mistakes mcp serve --stdio` command
// as a separate process (this test binary re-executed through the
// NM_HOOK_HELPER entry point) and connects to it as an agent surface would.
func startGatewayProcess(t *testing.T) *sdk.ClientSession {
	t.Helper()
	cmd := exec.Command(os.Args[0], "mcp", "serve", "--stdio")
	cmd.Env = append(os.Environ(), "NM_HOOK_HELPER=1")
	cmd.Stderr = os.Stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "durable-state-agent-surface", Version: "test"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to mcp gateway: %v", err)
	}
	return session
}

// gatewayCall issues one tool call and decodes the normalized receipt.
func gatewayCall(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s returned no content", name)
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("%s content = %T, want text", name, res.Content[0])
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(text.Text), &receipt); err != nil {
		t.Fatalf("%s content is not a JSON receipt: %v\n%s", name, err, text.Text)
	}
	return receipt
}

func receiptErrorCode(receipt map[string]any) string {
	errObj, _ := receipt["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func receiptData(receipt map[string]any) map[string]any {
	data, _ := receipt["data"].(map[string]any)
	return data
}

// allowGatewayRoot writes the operator's gateway allowlist to the test home.
func allowGatewayRoot(t *testing.T, root string) {
	t.Helper()
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("mcp:\n  allowed_repo_roots:\n    - "+root+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gateSnapshot is everything that would change if the gate were resolved.
type gateSnapshot struct {
	RunStatus  types.RunStatus
	StepStatus types.StepStatus
	Findings   string
	Rounds     int
}

func readGateSnapshot(t *testing.T, runID string) gateSnapshot {
	t.Helper()
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	run, err := d.GetRun(runID)
	if err != nil || run == nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	steps, err := d.GetStepsByRun(runID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps for %s = %d, %v", runID, len(steps), err)
	}
	rounds, err := d.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	snap := gateSnapshot{RunStatus: run.Status, StepStatus: steps[0].Status, Rounds: len(rounds)}
	if steps[0].FindingsJSON != nil {
		snap.Findings = *steps[0].FindingsJSON
	}
	return snap
}

// TestMCPGatewayAskUserGateSurvivesGatewayExit parks a run at a review gate
// holding an ask-user finding, exactly as the executor records one, then
// proves a gateway process refuses every way of answering it without a human
// decision, exits, and a fresh gateway process finds the same gate unchanged.
func TestMCPGatewayAskUserGateSurvivesGatewayExit(t *testing.T) {
	f := newCLISyncFixture(t)
	gateBranch := "feature/gate"
	worktree := filepath.Join(filepath.Dir(f.local), "gate-worktree")
	cliGit(t, f.local, "worktree", "add", "-b", gateBranch, worktree, f.old)
	allowGatewayRoot(t, filepath.Dir(f.local))

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	completed, err := d.GetRun(f.runID)
	if err != nil || completed == nil {
		t.Fatalf("fixture run: %v", err)
	}
	run, err := d.InsertRun(completed.RepoID, gateBranch, f.old, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	review, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"gate-1","severity":"warning","file":"file.txt","line":1,` +
		`"description":"two behaviours are defensible here; a maintainer must choose","action":"ask-user"}],` +
		`"summary":"found 1 issue"}`
	if err := d.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(review.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	before := readGateSnapshot(t, run.ID)

	assertGate := func(t *testing.T, receipt map[string]any) {
		t.Helper()
		if receipt["run_id"] != run.ID || receipt["state"] != "awaiting_decision" || receipt["requires_user_decision"] != true {
			t.Fatalf("gate receipt = %#v", receipt)
		}
		items, _ := receipt["findings"].([]any)
		if len(items) != 1 {
			t.Fatalf("gate findings = %#v", receipt["findings"])
		}
		finding, _ := items[0].(map[string]any)
		if finding["id"] != "gate-1" || finding["action"] != types.ActionAskUser {
			t.Fatalf("gate finding = %#v", finding)
		}
	}

	first := startGatewayProcess(t)
	initialStatus := gatewayCall(t, first, "nomistakes_status", map[string]any{"repo_path": worktree})
	assertGate(t, initialStatus)

	// Every action, and a decision that is only whitespace, is refused - with
	// the findings a human has to see, and without the gate moving.
	refusals := []map[string]any{
		{"repo_path": worktree, "action": "approve"},
		{"repo_path": worktree, "action": "fix", "finding_ids": []string{"gate-1"}},
		{"repo_path": worktree, "action": "skip"},
		{"repo_path": worktree, "action": "approve", "user_decision": "   \n\t"},
	}
	var recordedRefusals []map[string]any
	for _, args := range refusals {
		denied := gatewayCall(t, first, "nomistakes_respond", args)
		if denied["ok"] != false || receiptErrorCode(denied) != "user_decision_required" {
			t.Fatalf("respond %v was not refused for a missing decision: %#v", args, denied)
		}
		assertGate(t, denied)
		recordedRefusals = append(recordedRefusals, map[string]any{
			"input": args, "receipt": denied, "code": receiptErrorCode(denied),
		})
	}
	if got := readGateSnapshot(t, run.ID); got != before {
		t.Fatalf("refused responses changed the gate:\nbefore %+v\nafter  %+v", before, got)
	}

	// The gateway process exits. The gate is a durable no-mistakes record, not
	// gateway state, so it is still exactly where it was.
	if err := first.Close(); err != nil {
		t.Fatalf("gateway exit: %v", err)
	}
	afterExit := readGateSnapshot(t, run.ID)
	if afterExit != before {
		t.Fatalf("gateway exit changed the gate:\nbefore %+v\nafter  %+v", before, afterExit)
	}

	// A fresh gateway process reattaches by branch alone and finds the same
	// gate, still refusing to answer it.
	second := startGatewayProcess(t)
	defer second.Close()
	reattachedStatus := gatewayCall(t, second, "nomistakes_status", map[string]any{"repo_path": worktree})
	assertGate(t, reattachedStatus)
	denied := gatewayCall(t, second, "nomistakes_respond", map[string]any{"repo_path": worktree, "run_id": run.ID, "action": "approve"})
	if receiptErrorCode(denied) != "user_decision_required" {
		t.Fatalf("reattached gateway did not refuse: %#v", denied)
	}
	finalSnap := readGateSnapshot(t, run.ID)
	if finalSnap != before {
		t.Fatalf("reattached gateway changed the gate:\nbefore %+v\nafter  %+v", before, finalSnap)
	}

	// No refusal reached for the daemon: none was ever started for this home.
	if _, err := os.Stat(p.Socket()); !os.IsNotExist(err) {
		t.Fatalf("a daemon socket exists after refused responses (stat err %v); a refusal must not contact the daemon", err)
	}

	writeEvidence(t, "scenario1-ask-user-gate-survival.json", map[string]any{
		"scenario":                         "Ask-user gate survives gateway process exit and reattach without resolution or loss",
		"worktree":                         worktree,
		"gate_branch":                      gateBranch,
		"run_id":                           run.ID,
		"gate_snapshot_initial":            before,
		"initial_status_receipt":           initialStatus,
		"unauthorized_respond_refusals":    recordedRefusals,
		"gate_snapshot_after_gateway_exit": afterExit,
		"reattached_status_receipt":        reattachedStatus,
		"reattached_refused_receipt":       denied,
		"gate_snapshot_final":              finalSnap,
		"daemon_contacted":                 false,
	})
}

// TestMCPGatewayReceiptAndAuthorizedSync reads a completed, pushed run as the
// pipeline records it, and proves the receipt carries its PR URL and full head
// SHA, and that sync mutates only when AXI's own next action authorizes exactly
// that mutation - every other request leaves HEAD and every ref untouched.
func TestMCPGatewayReceiptAndAuthorizedSync(t *testing.T) {
	f := newCLISyncFixture(t)
	allowGatewayRoot(t, filepath.Dir(f.local))
	const prURL = "https://github.com/example/no-mistakes/pull/7"
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRURL(f.runID, prURL); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	session := startGatewayProcess(t)
	defer session.Close()

	var statusReceipts []map[string]any
	for _, args := range []map[string]any{
		{"repo_path": f.local, "run_id": f.runID},
		{"repo_path": f.local},
	} {
		receipt := gatewayCall(t, session, "nomistakes_status", args)
		if receipt["run_id"] != f.runID || receipt["pr_url"] != prURL {
			t.Fatalf("status %v receipt = %#v", args, receipt)
		}
		head, _ := receipt["head_sha"].(string)
		if head != f.pushed || len(head) != 40 {
			t.Fatalf("receipt head_sha = %q, want the full pipeline head %q", head, f.pushed)
		}
		// Nothing reported CI green, so the receipt must not claim it.
		if receipt["ci"] != nil {
			t.Fatalf("receipt claims ci %v for a run with no verified checks", receipt["ci"])
		}
		statusReceipts = append(statusReceipts, receipt)
	}
	writeEvidence(t, "scenario2-receipt-verification.json", map[string]any{
		"scenario":          "Receipts return PR URL, full 40-character head SHA, and null CI when unverified",
		"repo_path":         f.local,
		"run_id":            f.runID,
		"expected_pr_url":   prURL,
		"expected_head_sha": f.pushed,
		"head_sha_len":      len(f.pushed),
		"status_receipts":   statusReceipts,
	})

	refs := func() string {
		return cliGit(t, f.local, "for-each-ref", "--format=%(refname) %(objectname)") + "\nHEAD " + cliGit(t, f.local, "rev-parse", "HEAD")
	}
	inspected := gatewayCall(t, session, "nomistakes_sync", map[string]any{"repo_path": f.local})
	next, _ := receiptData(inspected)["next_action"].(map[string]any)
	if inspected["ok"] != true || next["code"] != "sync" {
		t.Fatalf("sync inspection = %#v", inspected)
	}
	untouched := refs()

	var syncRefusals []map[string]any
	refused := func(args map[string]any, wantCode string) {
		t.Helper()
		receipt := gatewayCall(t, session, "nomistakes_sync", args)
		if receipt["ok"] != false || receiptErrorCode(receipt) != wantCode {
			t.Fatalf("sync %v = %#v, want refusal %s", args, receipt, wantCode)
		}
		if got := refs(); got != untouched {
			t.Fatalf("refused sync %v changed refs:\nbefore %s\nafter  %s", args, untouched, got)
		}
		syncRefusals = append(syncRefusals, map[string]any{
			"args": args, "want_code": wantCode, "receipt": receipt,
		})
	}
	// Recovery is not what AXI authorized, and asserting both is not a way in.
	refused(map[string]any{"repo_path": f.local, "recover": true}, "sync_not_authorized")
	refused(map[string]any{"repo_path": f.local, "apply": true, "recover": true}, "invalid_action")

	// A dirty worktree changes AXI's next action, so the same apply that is
	// authorized a moment later is refused now.
	scratch := filepath.Join(f.local, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused(map[string]any{"repo_path": f.local, "apply": true}, "sync_not_authorized")
	if err := os.Remove(scratch); err != nil {
		t.Fatal(err)
	}

	applied := gatewayCall(t, session, "nomistakes_sync", map[string]any{"repo_path": f.local, "apply": true})
	if applied["ok"] != true || receiptData(applied)["changed"] != true {
		t.Fatalf("authorized sync = %#v", applied)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("HEAD after authorized sync = %s, want %s", got, f.pushed)
	}

	// Synchronized, AXI authorizes nothing further, so a repeat is refused.
	untouched = refs()
	refused(map[string]any{"repo_path": f.local, "apply": true}, "sync_not_authorized")

	writeEvidence(t, "scenario3-guarded-sync.json", map[string]any{
		"scenario":           "Guarded branch sync authorizes only AXI-prescribed mutations and rejects unauthorized/dirty states",
		"repo_path":          f.local,
		"inspected_receipt":  inspected,
		"refused_operations": syncRefusals,
		"applied_receipt":    applied,
		"final_head":         cliGit(t, f.local, "rev-parse", "HEAD"),
		"expected_head":      f.pushed,
	})
}

// TestMCPGatewayRepositoryAllowlistAndTraversal proves repository allowlisting
// and path canonicalization fail closed for outside, non-git, traversal, and
// unconfigured targets.
func TestMCPGatewayRepositoryAllowlistAndTraversal(t *testing.T) {
	root := t.TempDir()
	allowedRepo := filepath.Join(root, "allowed-repo")
	cliGit(t, root, "init", "-b", "main", allowedRepo)

	outsideRepo := filepath.Join(root, "outside-repo")
	cliGit(t, root, "init", "-b", "main", outsideRepo)

	nonGitDir := filepath.Join(root, "empty-dir")
	if err := os.MkdirAll(nonGitDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Configure roots to allow both allowedRepo and nonGitDir
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	configContent := fmt.Sprintf("mcp:\n  allowed_repo_roots:\n    - %s\n    - %s\n", allowedRepo, nonGitDir)
	if err := os.WriteFile(p.ConfigFile(), []byte(configContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session := startGatewayProcess(t)
	defer session.Close()

	// 1. Allowed repo succeeds allowlist check
	allowedReceipt := gatewayCall(t, session, "nomistakes_status", map[string]any{"repo_path": allowedRepo})
	if receiptErrorCode(allowedReceipt) == "repo_not_allowed" {
		t.Fatalf("allowed repo was refused: %#v", allowedReceipt)
	}

	// 2. Outside repo fails with repo_not_allowed
	outsideReceipt := gatewayCall(t, session, "nomistakes_status", map[string]any{"repo_path": outsideRepo})
	if outsideReceipt["ok"] != false || receiptErrorCode(outsideReceipt) != "repo_not_allowed" {
		t.Fatalf("outside repo %s was not refused: %#v", outsideRepo, outsideReceipt)
	}

	// 3. Traversal path fails with repo_not_allowed
	traversalPath := filepath.Join(allowedRepo, "..", "outside-repo")
	traversalReceipt := gatewayCall(t, session, "nomistakes_status", map[string]any{"repo_path": traversalPath})
	if traversalReceipt["ok"] != false || receiptErrorCode(traversalReceipt) != "repo_not_allowed" {
		t.Fatalf("traversal path was not refused: %#v", traversalReceipt)
	}

	// 4. Non-git directory fails closed with not_a_git_repository
	nonGitReceipt := gatewayCall(t, session, "nomistakes_status", map[string]any{"repo_path": nonGitDir})
	if nonGitReceipt["ok"] != false || receiptErrorCode(nonGitReceipt) != "not_a_git_repository" {
		t.Fatalf("non-git repo was not refused: %#v", nonGitReceipt)
	}

	writeEvidence(t, "scenario4-repo-allowlist-traversal.json", map[string]any{
		"scenario":          "Repository allowlisting and fail-closed path canonicalization",
		"allowed_repo":      allowedRepo,
		"allowed_receipt":   allowedReceipt,
		"outside_repo":      outsideRepo,
		"outside_receipt":   outsideReceipt,
		"traversal_path":    traversalPath,
		"traversal_receipt": traversalReceipt,
		"non_git_path":      nonGitDir,
		"non_git_receipt":   nonGitReceipt,
	})
}

// TestMCPGatewayToolSurfaceContract proves the tool listing exposes exactly the
// 6 v1 tools and strictly omits forbidden operations (merge, push, abort).
func TestMCPGatewayToolSurfaceContract(t *testing.T) {
	session := startGatewayProcess(t)
	defer session.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var toolNames []string
	var forbidden []string
	toolDetails := make(map[string]any)
	for _, tool := range list.Tools {
		toolNames = append(toolNames, tool.Name)
		toolDetails[tool.Name] = map[string]any{
			"description":  tool.Description,
			"annotations":  tool.Annotations,
			"input_schema": tool.InputSchema,
		}
		lower := strings.ToLower(tool.Name)
		if strings.Contains(lower, "merge") || strings.Contains(lower, "push") || strings.Contains(lower, "abort") {
			forbidden = append(forbidden, tool.Name)
		}
	}

	if len(forbidden) > 0 {
		t.Fatalf("found forbidden tools: %v", forbidden)
	}

	writeEvidence(t, "scenario6-tools-list-contract.json", map[string]any{
		"scenario":        "MCP gateway tool catalog exposes exactly v1 tools and omits forbidden operations",
		"tool_count":      len(toolNames),
		"tool_names":      toolNames,
		"tool_details":    toolDetails,
		"forbidden_tools": forbidden,
	})
}
