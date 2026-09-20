package axiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestWaitForTriggeredRunPropagatesIPCError(t *testing.T) {
	dir, err := os.MkdirTemp("", "ax-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ipc.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("daemon unavailable")
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()
	t.Cleanup(func() {
		srv.Close()
		<-errCh
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err := ipc.Dial(sock)
		if err == nil {
			client.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = waitForTriggeredRun(context.Background(), client, "repo", "branch", "head", nil)
	if err == nil {
		t.Fatal("waitForTriggeredRun returned nil error")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "daemon unavailable") {
		t.Fatalf("error = %q, want daemon error", got)
	}
}

func TestTriggerWaitFailurePreservesPushAndPollErrors(t *testing.T) {
	pushErr := errors.New("rejected by gate")
	waitErr := errors.New("daemon unavailable")

	err := triggerWaitFailure("feature/probe", pushErr, waitErr)
	if !strings.Contains(err.Error(), `push "feature/probe" to gate: rejected by gate`) {
		t.Fatalf("error = %q, want push failure", err)
	}
	if !strings.Contains(err.Error(), "wait for triggered run: daemon unavailable") {
		t.Fatalf("error = %q, want poll failure", err)
	}
	if !errors.Is(err, waitErr) {
		t.Fatalf("error = %v, want wrapped poll failure", err)
	}
}

func TestWaitForTriggeredRunFindsNewRunAfterPriorRun(t *testing.T) {
	dir, err := os.MkdirTemp("", "ax-trigger-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ipc.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{}, nil
	})
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{Runs: []ipc.RunInfo{
			{ID: "prior", HeadSHA: "head"},
			{ID: "new", HeadSHA: "head"},
		}}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()
	t.Cleanup(func() {
		srv.Close()
		<-errCh
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err := ipc.Dial(sock)
		if err == nil {
			client.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	run, err := waitForTriggeredRun(context.Background(), client, "repo", "branch", "head", map[string]struct{}{"prior": {}})
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.ID != "new" {
		t.Fatalf("run = %#v, want new run", run)
	}
}

func TestWaitForTriggeredRunReturnsCallerDeadline(t *testing.T) {
	// A deadline already in the past cancels synchronously inside
	// WithDeadline, so ctx.Err() is set before waitForTriggeredRun runs.
	// WithTimeout(time.Nanosecond) only sets it once its timer fires, which a
	// short sleep does not guarantee on a coarse-granularity platform
	// (Windows defaults to ~15.6ms). When the timer had not fired, the loop's
	// ctx.Err() guard saw nil and reached the IPC call, dereferencing the nil
	// client below and panicking - a flake, not a product fault.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	// The nil client is the assertion: caller expiry must be returned before
	// any IPC call is attempted.
	_, err := waitForTriggeredRun(ctx, nil, "repo", "branch", "head", nil)
	if !errors.Is(err, context.DeadlineExceeded) && !ipc.IsCallTimeout(err) {
		t.Fatalf("error = %v, want caller deadline", err)
	}
}

func TestWaitForTriggeredRunBoundsIPCReplyByCallerDeadline(t *testing.T) {
	dir, err := os.MkdirTemp("", "ax-slow-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ipc.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		time.Sleep(time.Second)
		return &ipc.GetActiveRunResult{}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()
	t.Cleanup(func() {
		srv.Close()
		<-errCh
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err := ipc.Dial(sock)
		if err == nil {
			client.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = waitForTriggeredRun(ctx, client, "repo", "branch", "head", nil)
	if !errors.Is(err, context.DeadlineExceeded) && !ipc.IsCallTimeout(err) {
		t.Fatalf("error = %v, want caller deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("wait took %s, exceeded caller deadline", elapsed)
	}
}

func TestCallIPCBoundsReplyByCallerDeadline(t *testing.T) {
	dir, err := os.MkdirTemp("", "ax-callipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ipc.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		time.Sleep(time.Second)
		return &ipc.GateContextResult{}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()
	t.Cleanup(func() {
		srv.Close()
		<-errCh
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		client, err := ipc.Dial(sock)
		if err == nil {
			client.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	var result ipc.GateContextResult
	err = callIPC(ctx, client, ipc.MethodGateContext, &ipc.GateContextParams{}, &result)
	if !errors.Is(err, context.DeadlineExceeded) && !ipc.IsCallTimeout(err) {
		t.Fatalf("error = %v, want caller deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("call took %s, exceeded caller deadline", elapsed)
	}
}
