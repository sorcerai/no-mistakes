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
	dir, err := os.MkdirTemp("/private/tmp", "ax-")
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

func TestWaitForTriggeredRunReturnsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	_, err := waitForTriggeredRun(ctx, nil, "repo", "branch", "head", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want caller deadline", err)
	}
}
