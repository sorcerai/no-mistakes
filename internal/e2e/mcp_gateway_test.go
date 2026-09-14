//go:build e2e

package e2e

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

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// mcpIntent is distinctive so the journey can prove the caller's own words
// reached the pipeline rather than being inferred from a transcript.
const mcpIntent = "expose the gateway behind the agent surface without direct forge writes"

// mcpScenario parks the review step on one ask-user finding - the case the
// gateway must refuse to answer on its own - and lets every other step pass.
func mcpScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review needs a human decision"
    structured:
      findings:
        - id: "mcp-1"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "two behaviours are defensible here; a maintainer must choose"
          action: ask-user
      summary: "found 1 issue"
      risk_level: medium
      risk_rationale: "behaviour choice needs a human"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: mcp gateway"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write mcp scenario: %v", err)
	}
	return path
}

// allowMCPRoot appends the gateway's repository allowlist to the harness's
// global config. It is written after setup because the allowed root is a
// worktree path that does not exist until the test creates it.
func allowMCPRoot(t *testing.T, h *Harness, root string) {
	t.Helper()
	path := filepath.Join(h.NMHome, "config.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	updated := string(existing) + fmt.Sprintf("mcp:\n  allowed_repo_roots:\n    - %s\n", root)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
}

// startMCPSession launches the real `no-mistakes mcp serve --stdio` binary as a
// subprocess and speaks MCP to it, which is what an agent surface does.
func startMCPSession(t *testing.T, h *Harness, dir string) *sdk.ClientSession {
	t.Helper()
	cmd := exec.Command(h.NMBin, "mcp", "serve", "--stdio")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "e2e-agent-surface", Version: "test"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to mcp gateway: %v", err)
	}
	return session
}

// mcpCall issues one tool call and decodes the normalized receipt.
func mcpCall(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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
	t.Logf("%s receipt: %s", name, text.Text)
	return receipt
}

// TestMCPGatewayJourney drives a complete no-mistakes delivery through the MCP
// gateway the way an external agent surface would: discover the tools, inspect
// status, start a run, receive a gate, be refused an ask-user decision, survive
// the gateway process exiting mid-run, then finish with an explicit human
// decision and receive a receipt carrying the created PR URL and full head SHA.
//
// No MCP tool writes to the forge: publication happens inside no-mistakes.
func TestMCPGatewayJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: mcpScenario(t)})
	ctx := context.Background()
	branch := "feature/mcp-gateway"

	parentURL := "https://github.com/example/no-mistakes.git"
	forkURL := "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set GitHub origin: %v\n%s", err, out)
	}
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(filepath.Dir(h.AgentLog), "gh-mcp-gateway.log"))
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	head := h.CommitChange(branch, "feature.txt", "gateway\n", "add gateway feature")
	worktree := h.AddWorktree(branch)
	allowMCPRoot(t, h, filepath.Dir(worktree))

	session := startMCPSession(t, h, h.WorkDir)
	defer session.Close()

	// Discovery: exactly the v1 tools, and nothing that writes to a forge.
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	tools := map[string]bool{}
	for _, tool := range list.Tools {
		tools[tool.Name] = true
	}
	for _, want := range []string{"nomistakes_status", "nomistakes_run", "nomistakes_respond", "nomistakes_logs", "nomistakes_sync", "nomistakes_doctor"} {
		if !tools[want] {
			t.Fatalf("missing tool %s; have %v", want, tools)
		}
	}
	if len(tools) != 6 {
		t.Fatalf("tools = %v, want exactly the six v1 tools", tools)
	}

	// A repository outside the configured roots is refused, by path, before
	// anything happens.
	refused := mcpCall(t, session, "nomistakes_doctor", map[string]any{"repo_path": h.WorkDir})
	if refused["ok"] != false {
		t.Fatalf("the working clone is outside the allowed root and must be refused: %#v", refused)
	}

	doctor := mcpCall(t, session, "nomistakes_doctor", map[string]any{"repo_path": worktree})
	if doctor["ok"] != true {
		t.Fatalf("doctor: %#v", doctor)
	}
	if data, _ := doctor["data"].(map[string]any); data == nil || data["repo_initialized"] != true {
		t.Fatalf("doctor data: %#v", doctor["data"])
	}

	idle := mcpCall(t, session, "nomistakes_status", map[string]any{"repo_path": worktree})
	if idle["state"] != "idle" {
		t.Fatalf("status before any run = %#v", idle)
	}

	gate := mcpCall(t, session, "nomistakes_run", map[string]any{
		"repo_path": worktree, "intent": mcpIntent, "wait_seconds": 150,
	})
	if gate["state"] != "awaiting_decision" {
		t.Fatalf("run did not park at a gate: %#v", gate)
	}
	if gate["requires_user_decision"] != true {
		t.Fatalf("an ask-user gate must require an external decision: %#v", gate)
	}
	runID, _ := gate["run_id"].(string)
	if runID == "" {
		t.Fatalf("gate receipt carries no run id: %#v", gate)
	}
	findings, _ := gate["findings"].([]any)
	if len(findings) != 1 || findings[0].(map[string]any)["action"] != types.ActionAskUser {
		t.Fatalf("gate findings = %#v", gate["findings"])
	}

	// The intent reached the pipeline verbatim rather than being inferred.
	assertIntentReachedAgent(t, h, mcpIntent)

	// The gateway refuses to answer what the pipeline referred to a human, and
	// the run is left exactly where it was.
	denied := mcpCall(t, session, "nomistakes_respond", map[string]any{
		"repo_path": worktree, "action": "approve",
	})
	if denied["ok"] != false {
		t.Fatalf("an ask-user gate was progressed without a decision: %#v", denied)
	}
	if errObj, _ := denied["error"].(map[string]any); errObj == nil || errObj["code"] != "user_decision_required" {
		t.Fatalf("refusal = %#v", denied)
	}
	still := h.RunInfo(runID)
	if still == nil || still.Status != types.RunRunning {
		t.Fatalf("the refused response changed the run: %#v", still)
	}

	// Logs are readable while parked.
	logs := mcpCall(t, session, "nomistakes_logs", map[string]any{
		"repo_path": worktree, "step": "review",
	})
	if logs["ok"] != true {
		t.Fatalf("logs: %#v", logs)
	}

	// The gateway holds no run state: stopping it leaves the run parked and
	// alive, and a fresh gateway picks it straight back up.
	if err := session.Close(); err != nil {
		t.Fatalf("close gateway session: %v", err)
	}
	survived := h.RunInfo(runID)
	if survived == nil || survived.Status != types.RunRunning {
		t.Fatalf("stopping the gateway ended the run: %#v", survived)
	}

	resumed := startMCPSession(t, h, h.WorkDir)
	defer resumed.Close()
	reattached := mcpCall(t, resumed, "nomistakes_status", map[string]any{"repo_path": worktree})
	if reattached["run_id"] != runID || reattached["state"] != "awaiting_decision" {
		t.Fatalf("a fresh gateway did not find the parked run: %#v", reattached)
	}

	// With the decision a human actually made, the run finishes.
	done := mcpCall(t, resumed, "nomistakes_respond", map[string]any{
		"repo_path": worktree, "action": "approve",
		"user_decision": "The maintainer reviewed mcp-1 and accepted the current behaviour.",
		"wait_seconds":  180,
	})
	if done["ok"] != true {
		t.Fatalf("respond with a decision: %#v", done)
	}

	completed := h.WaitForRun(branch, 180*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run status = %q, error = %v", completed.Status, deref(completed.Error))
	}
	if completed.PRURL == nil || !strings.HasPrefix(*completed.PRURL, "https://github.com/example/no-mistakes/pull/") {
		t.Fatalf("no-mistakes did not create a PR: %v", completed.PRURL)
	}

	final := mcpCall(t, resumed, "nomistakes_status", map[string]any{"repo_path": worktree, "run_id": runID})
	if final["pr_url"] != *completed.PRURL {
		t.Fatalf("receipt pr_url = %v, want %q", final["pr_url"], *completed.PRURL)
	}
	gotHead, _ := final["head_sha"].(string)
	if gotHead != completed.HeadSHA {
		t.Fatalf("receipt head_sha = %q, want the run head %q", gotHead, completed.HeadSHA)
	}
	if len(gotHead) != 40 {
		t.Fatalf("receipt head_sha %q is abbreviated; a machine receipt must carry the full commit", gotHead)
	}
	if gotHead == head {
		t.Logf("head unchanged by the pipeline: %s", gotHead)
	}
	if state, _ := final["state"].(string); !strings.HasPrefix(state, "passed") {
		t.Fatalf("final state = %q, want a passed outcome", state)
	}
}

// assertIntentReachedAgent proves the caller's own intent text was handed to
// the pipeline agent rather than inferred.
func assertIntentReachedAgent(t *testing.T, h *Harness, intent string) {
	t.Helper()
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, intent) {
			return
		}
	}
	t.Fatalf("intent %q never reached a pipeline agent prompt", intent)
}
