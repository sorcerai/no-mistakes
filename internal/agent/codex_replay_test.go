package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCodexFallbackPreservesCompletedWork(t *testing.T) {
	defer withFastBackoff(t)()
	for _, tc := range []struct {
		name, event, failure string
		fallback             bool
	}{
		{"quota_before_work", "", "quota exhausted", true},
		{"quota_during_tool", `{"type":"item.started","item":{"id":"a","type":"command_execution"}}`, "quota exhausted", false},
		{"quota_after_tool", `{"type":"item.completed","item":{"id":"a","type":"command_execution"}}`, "quota exhausted", false},
		{"quota_after_answer", `{"type":"item.completed","item":{"type":"agent_message","text":"already answered"}}`, "quota exhausted", false},
		{"transient_after_tool", `{"type":"item.completed","item":{"id":"a","type":"file_change"}}`, "service_unavailable 503", false},
		{"quota_after_reasoning_only", `{"type":"item.completed","item":{"id":"a","type":"reasoning"}}`, "quota exhausted", true},
		{"quota_after_todo_only", `{"type":"item.updated","item":{"id":"a","type":"todo_list","items":[]}}`, "quota exhausted", true},
		{"quota_after_error_item", `{"type":"item.completed","item":{"id":"a","type":"error","message":"quota exhausted"}}`, "quota exhausted", true},
		{"quota_during_collaboration", `{"type":"item.started","item":{"id":"a","type":"collab_tool_call"}}`, "quota exhausted", false},
		{"quota_after_unknown_update", `{"type":"item.updated","item":{"id":"a","type":"future_tool_call"}}`, "quota exhausted", false},
		{"quota_after_malformed_record", `{"type":"item.started"`, "quota exhausted", false},
		{"quota_after_unknown_event", `{"type":"future_tool.started","command":"write file"}`, "quota exhausted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			errEvent := `{"type":"error","message":"` + tc.failure + `"}`
			first := writeFakeCodex(t, t.TempDir(), "#!/bin/sh\necho attempt >> attempts.txt\nprintf '%s\\n' '"+tc.event+"' '"+errEvent+"'\nexit 1\n", "@echo off\r\necho attempt >> attempts.txt\r\necho("+tc.event+"\r\necho("+errEvent+"\r\nexit /b 1\r\n")
			second := writeFakeCodex(t, t.TempDir(), "#!/bin/sh\necho replay > replay.txt\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"fallback answer\"}}'\n", "@echo off\r\necho replay > replay.txt\r\necho {\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"fallback answer\"}}\r\n")
			result, err := NewFallback([]Agent{&codexAgent{bin: first}, &codexAgent{bin: second}}).Run(context.Background(), RunOpts{Prompt: "perform the operation", CWD: dir})
			if tc.fallback {
				if err != nil || result == nil || result.Text != "fallback answer" {
					t.Fatalf("quota before work did not fall back: result=%+v err=%v", result, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("expected original failure without replay, got result=%+v err=%v", result, err)
				}
				if _, statErr := os.Stat(filepath.Join(dir, "replay.txt")); !os.IsNotExist(statErr) {
					t.Fatalf("fallback replayed completed work: %v", statErr)
				}
			}
			attempts, readErr := os.ReadFile(filepath.Join(dir, "attempts.txt"))
			if readErr != nil || strings.Count(string(attempts), "attempt") != 1 {
				t.Fatalf("original operation was retried: %q, %v", attempts, readErr)
			}
		})
	}
}

func TestFallbackCancellationDoesNotStartAnotherAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("codex exited: signal: killed")
	first := &fallbackTestAgent{name: "codex", run: func() (*Result, error) {
		cancel()
		return nil, failure
	}}
	second := &fallbackTestAgent{name: "pi", run: func() (*Result, error) {
		t.Fatal("started another agent after cancellation")
		return nil, nil
	}}
	if _, err := NewFallback([]Agent{first, second}).Run(ctx, RunOpts{}); err != failure {
		t.Fatalf("original cancellation failure lost: %v", err)
	}
}

func TestCodexStreamReadFailureRefusesReplay(t *testing.T) {
	cause := errors.New("event transport failed")
	var usage TokenUsage
	var lastMessage, codexErr, threadID string
	err := parseCodexEvents(context.Background(), iotest.ErrReader(cause), nil,
		&usage, &lastMessage, &codexErr, &threadID, newCodexMetricsAccumulator())
	if !errors.Is(err, cause) || !IsReplayUnsafeError(err) {
		t.Fatalf("indeterminate event stream must preserve its cause and refuse replay: %v", err)
	}
}
