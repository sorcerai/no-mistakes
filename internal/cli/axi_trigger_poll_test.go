package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

// A failed post-push poll is not evidence that no run exists, so it must not
// fall back to a rerun that duplicates (or, through the daemon's supersede
// path, cancels) the run the push created. A cancelled caller never reached
// the rerun, since its git status shares the context; that case only pins
// that the poll's own error is what surfaces.
func TestTriggerRunPollErrorDoesNotRerun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*triggerFixture, context.CancelFunc)
		want  string
	}{
		{"active_poll", func(f *triggerFixture, _ context.CancelFunc) { f.failActiveRead = 1 }, "injected active read failure"},
		{"head_poll", func(f *triggerFixture, _ context.CancelFunc) { f.failHeadRead = 2 }, "injected head read failure"},
		{"caller_cancelled", func(f *triggerFixture, cancel context.CancelFunc) {
			// No run appears, so cancellation is the only way out of the poll.
			f.freshOnFirstGet = false
			f.onActiveRead = cancel
		}, context.Canceled.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriggerFixture(t, false)
			f.freshOnFirstGet = true
			ctx, cancel := context.WithCancel(triggerTestContext(t))
			defer cancel()
			tc.setup(f, cancel)

			runID, err := f.trigger(ctx)
			// The wrap proves the poll error itself was returned. Without it a
			// cancelled caller also fails later, inside the rerun path's git
			// status, with the same cause text.
			if runID != "" || err == nil || !strings.Contains(err.Error(), "wait for triggered run") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run ID = %q, err = %v; want wait error containing %q", runID, err, tc.want)
			}
			if got := f.gateHead(); got != f.head {
				t.Fatalf("gate head = %q, want pushed head %q", got, f.head)
			}
			// The push reached the gate, so this failure carries the retry
			// guidance; the pre-push refusals must not.
			var waitErr *triggeredRunWaitError
			if !errors.As(err, &waitErr) {
				t.Fatalf("err = %v; want a triggeredRunWaitError after a successful push", err)
			}
			wantRows := 1
			if f.freshOnFirstGet {
				wantRows = 2
			}
			if f.reruns.Load() != 0 || f.runCount() != wantRows {
				t.Fatalf("reruns = %d, rows = %d; want 0 reruns and %d rows", f.reruns.Load(), f.runCount(), wantRows)
			}
		})
	}
}

// When the push fails and the poll then errors too, the push is the root
// cause and the poll never had a run to find. Reporting only the poll error
// would drop the cause for the symptom.
func TestTriggerRunFailedPushAndPollErrorKeepsPushError(t *testing.T) {
	f := newTriggerFixture(t, false)
	rejectGatePushes(t, f.gateDir)
	f.failActiveRead = 1

	runID, err := f.trigger(triggerTestContext(t))
	if runID != "" || err == nil {
		t.Fatalf("run ID = %q, err = %v; want a failure", runID, err)
	}
	for _, want := range []string{"push", "to gate", "wait for triggered run", "injected active read failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	// A push that never landed must not claim it reached the gate.
	var waitErr *triggeredRunWaitError
	if errors.As(err, &waitErr) {
		t.Fatalf("err = %v; a failed push must not carry the reached-the-gate guidance", err)
	}
	if f.reruns.Load() != 0 {
		t.Fatalf("reruns = %d, want 0", f.reruns.Load())
	}
}

// The retry guidance is true only when the push reached the gate. Attaching
// it to the pre-push refusals would tell the caller a push happened when it
// provably did not.
func TestEmitTriggerErrorHelpOnlyAfterSuccessfulPush(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantHelp bool
	}{
		{"poll_error_after_push", &triggeredRunWaitError{err: errors.New("injected active read failure")}, true},
		{"baseline_refusal", errors.New(`get prior runs for "feature/probe": injected head read failure`), false},
		{"failed_push", errors.New(`push "feature/probe" to gate: rejected; wait for triggered run: boom`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&buf)
			if err := emitTriggerError(cmd, tc.err); err == nil {
				t.Fatal("emitTriggerError returned no exit error")
			}
			var doc struct {
				Error string   `toon:"error"`
				Help  []string `toon:"help"`
			}
			if err := toon.Unmarshal(buf.Bytes(), &doc); err != nil {
				t.Fatalf("decode emitted error: %v\n%s", err, buf.String())
			}
			if doc.Error != tc.err.Error() {
				t.Fatalf("error = %q, want %q", doc.Error, tc.err.Error())
			}
			got := strings.Contains(strings.Join(doc.Help, "\n"), pushedRunRetryGuidance)
			if got != tc.wantHelp {
				t.Fatalf("retry guidance present = %v, want %v:\n%s", got, tc.wantHelp, buf.String())
			}
		})
	}
}

// rejectGatePushes makes every push to the bare gate fail, without breaking
// the reconciliation reads that run against it first.
func rejectGatePushes(t *testing.T, gateDir string) {
	t.Helper()
	hook := filepath.Join(gateDir, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'gate rejected the push' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
