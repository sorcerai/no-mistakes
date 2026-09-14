package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/axiapi"
	"github.com/kunchenguid/no-mistakes/internal/types"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect runs the gateway and a client over in-memory transports, which is a
// real MCP session: initialize, list-tools, and call all go through the
// protocol rather than around it.
func connect(t *testing.T, svc *Service) *sdk.ClientSession {
	t.Helper()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	server := NewServer(svc, "test")
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestInitializeAndListTools(t *testing.T) {
	svc, _ := newService(t, &fakeAXI{})
	session := connect(t, svc)

	if info := session.InitializeResult(); info == nil || info.ServerInfo == nil || info.ServerInfo.Name == "" {
		t.Fatalf("initialize result = %#v", info)
	}
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
	}
	sort.Strings(names)
	want := []string{
		"nomistakes_doctor", "nomistakes_logs", "nomistakes_respond",
		"nomistakes_run", "nomistakes_status", "nomistakes_sync",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want exactly %v", names, want)
	}
}

// TestNoMergeOrAbortToolExists pins two v1 non-goals structurally: the gateway
// cannot merge a pull request and cannot abort a run, because no such tool is
// registered for a client to call.
func TestNoMergeOrAbortToolExists(t *testing.T) {
	svc, _ := newService(t, &fakeAXI{})
	session := connect(t, svc)
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		lower := strings.ToLower(tool.Name)
		if strings.Contains(lower, "merge") || strings.Contains(lower, "abort") || strings.Contains(lower, "push") {
			t.Errorf("v1 must not expose %q", tool.Name)
		}
	}
}

// TestSchemaDeclaresAndEnforcesRequiredFields proves each tool declares the
// fields it cannot work without, and that a call missing one is rejected before
// it reaches the service.
func TestSchemaDeclaresAndEnforcesRequiredFields(t *testing.T) {
	axi := &fakeAXI{}
	svc, _ := newService(t, axi)
	session := connect(t, svc)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantRequired := map[string][]string{
		"nomistakes_status":  {"repo_path"},
		"nomistakes_run":     {"repo_path", "intent"},
		"nomistakes_respond": {"repo_path", "action"},
		"nomistakes_logs":    {"repo_path", "step"},
		"nomistakes_sync":    {"repo_path"},
		"nomistakes_doctor":  {"repo_path"},
	}
	seen := map[string]bool{}
	for _, tool := range list.Tools {
		seen[tool.Name] = true
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		got := append([]string(nil), schema.Required...)
		sort.Strings(got)
		want := append([]string(nil), wantRequired[tool.Name]...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s required = %v, want %v", tool.Name, got, want)
		}
	}
	for name := range wantRequired {
		if !seen[name] {
			t.Errorf("%s was not registered", name)
		}
	}

	for tool := range wantRequired {
		res, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: map[string]any{}})
		if err == nil && !res.IsError {
			t.Errorf("%s accepted a call with no arguments", tool)
		}
	}
	if len(axi.runCalls)+len(axi.respondCalls)+len(axi.syncCalls) != 0 {
		t.Error("a schema-invalid call still reached AXI")
	}
}

func callReceipt(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) (*sdk.CallToolResult, map[string]any) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
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
	return res, receipt
}

// TestCallToolReturnsTheNormalizedReceipt proves the receipt survives the
// protocol boundary intact, full head SHA included.
func TestCallToolReturnsTheNormalizedReceipt(t *testing.T) {
	axi := &fakeAXI{status: &axiapi.RunState{
		RunID: "01ABC", Branch: "feature/x", HeadSHA: fullSHA, Status: string(types.RunRunning),
	}}
	svc, repo := newService(t, axi)
	session := connect(t, svc)

	_, receipt := callReceipt(t, session, "nomistakes_status", map[string]any{"repo_path": repo})
	if receipt["ok"] != true || receipt["head_sha"] != fullSHA || receipt["state"] != StateRunning {
		t.Fatalf("receipt = %#v", receipt)
	}
}

// TestCallToolMarksARefusalAsAnError keeps a refused call visible to the model
// instead of reading as a successful one.
func TestCallToolMarksARefusalAsAnError(t *testing.T) {
	svc, _ := newService(t, &fakeAXI{})
	session := connect(t, svc)
	outside := mkGitRepo(t, filepath.Join(t.TempDir(), "elsewhere"))

	res, receipt := callReceipt(t, session, "nomistakes_status", map[string]any{"repo_path": outside})
	if !res.IsError {
		t.Error("a refused call must be marked as an error")
	}
	errObj, _ := receipt["error"].(map[string]any)
	if errObj == nil || errObj["code"] != CodeRepoNotAllowed {
		t.Fatalf("receipt = %#v", receipt)
	}
}

// TestAskUserGateCannotProgressThroughTheProtocol is the safety contract
// exercised end to end through a real MCP client: the refusal is what comes
// back, and nothing reached the daemon.
func TestAskUserGateCannotProgressThroughTheProtocol(t *testing.T) {
	axi := &fakeAXI{status: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Gate: &axiapi.Gate{
		Step: "review", Findings: []types.Finding{{ID: "r1", Action: types.ActionAskUser, Description: "decide"}},
	}}}
	svc, repo := newService(t, axi)
	session := connect(t, svc)

	_, receipt := callReceipt(t, session, "nomistakes_respond", map[string]any{
		"repo_path": repo, "action": "approve",
	})
	if receipt["requires_user_decision"] != true {
		t.Errorf("receipt = %#v", receipt)
	}
	errObj, _ := receipt["error"].(map[string]any)
	if errObj == nil || errObj["code"] != CodeUserDecisionRequired {
		t.Fatalf("receipt = %#v", receipt)
	}
	if len(axi.respondCalls) != 0 {
		t.Fatal("an ask-user gate reached the daemon through the protocol")
	}
}

// TestClosingTheSessionDoesNotAbortTheRun pins that the gateway holds no run
// state: stopping it leaves an active run running. The service is stateless by
// construction, so a closed session cannot have cancelled anything.
func TestClosingTheSessionDoesNotAbortTheRun(t *testing.T) {
	axi := &fakeAXI{
		run:    &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Status: string(types.RunRunning)},
		status: &axiapi.RunState{RunID: "01ABC", HeadSHA: fullSHA, Status: string(types.RunRunning)},
	}
	svc, repo := newService(t, axi)
	session := connect(t, svc)
	callReceipt(t, session, "nomistakes_run", map[string]any{"repo_path": repo, "intent": "goal"})
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	// A second, independent session observes the same live run.
	next := connect(t, svc)
	_, receipt := callReceipt(t, next, "nomistakes_status", map[string]any{"repo_path": repo})
	if receipt["state"] != StateRunning || receipt["run_id"] != "01ABC" {
		t.Fatalf("the run did not survive the closed session: %#v", receipt)
	}
}

// TestRunToolExposesNoAutoApproveInput pins that no client can ask the gateway
// to resolve gates for it.
func TestRunToolExposesNoAutoApproveInput(t *testing.T) {
	svc, _ := newService(t, &fakeAXI{})
	session := connect(t, svc)
	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"\"yes\"", "auto_approve", "auto_fix", "auto_yes"} {
			if strings.Contains(string(schema), banned) {
				t.Errorf("%s exposes %s", tool.Name, banned)
			}
		}
	}
}
