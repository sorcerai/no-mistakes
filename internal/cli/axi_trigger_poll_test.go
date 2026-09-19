package cli

import (
	"context"
	"strings"
	"testing"
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
