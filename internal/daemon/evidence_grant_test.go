package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Capture the actual process arguments, not a manually constructed RunOpts:
// both daemon assembly paths must grant the run directory rather than its parent.
func writeEvidenceCapturingCodex(t *testing.T, configPath, evidenceRoot string) string {
	t.Helper()
	capture := filepath.Join(t.TempDir(), "argv.txt")
	bin := filepath.Join(t.TempDir(), "codex")
	const response = `{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuoteForTest(capture) + "\ncat >/dev/null\nprintf '%s\\n' '" + response + "'\n"
	if runtime.GOOS == "windows" {
		bin += ".cmd"
		script = "@echo off\r\ntype nul > \"" + capture + "\"\r\n:args\r\nif \"%~1\"==\"\" goto done\r\necho %~1>>\"" + capture + "\"\r\nshift\r\ngoto args\r\n:done\r\nmore > nul\r\necho " + response + "\r\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("agent: codex\nagent_path_override:\n  codex: %q\n", bin)
	if evidenceRoot != "" {
		cfg += fmt.Sprintf("test:\n  evidence:\n    local_root: %q\n", evidenceRoot)
	}
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return capture
}

func assertOnlyRunEvidenceGrant(t *testing.T, capture, expected string) {
	t.Helper()
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n")), "\n")
	var grants []string
	for i, arg := range args {
		if arg == "--add-dir" && i+1 < len(args) {
			grants = append(grants, args[i+1])
		}
	}
	if len(grants) != 1 || grants[0] != expected {
		t.Fatalf("Codex writable roots = %q, want only this run's %q", grants, expected)
	}
}

func TestFreshRunGrantsOnlyItsEvidenceDirectory(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&reviewRoleProbeStep{}}
	})
	root := filepath.Join(t.TempDir(), "configured evidence")
	capture := writeEvidenceCapturingCodex(t, p.ConfigFile(), root)
	_, head := setupTestGitRepo(t, p, d, "evidence-grant")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("evidence-grant"), Ref: "refs/heads/main",
		Old: strings.Repeat("0", 40), New: head,
	}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run failed: %+v", run)
	}
	assertOnlyRunEvidenceGrant(t, capture, p.RunEvidenceDir(root, run.ID))
}

func TestRecoveredRunGrantsOnlyItsEvidenceDirectory(t *testing.T) {
	m, run, _ := gatePinFixture(t, "", nil)
	capture := writeEvidenceCapturingCodex(t, m.paths.ConfigFile(), "")
	plan, err := m.prepareRecoveredRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.agent.Close()
	if _, err := plan.agent.Run(context.Background(), agent.RunOpts{Prompt: "gather evidence", CWD: plan.workDir}); err != nil {
		t.Fatal(err)
	}
	assertOnlyRunEvidenceGrant(t, capture, m.paths.RunEvidenceDir("", run.ID))
}
