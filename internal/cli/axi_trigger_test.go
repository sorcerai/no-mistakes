package cli

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const triggerFixtureBranch = "feature/probe"

// triggerFixture drives the real triggerRun against a real Git worktree, a
// real bare gate remote, a real SQLite database and a real IPC server. The
// IPC handlers stand in for the daemon: they read and write the database the
// way the daemon would, and each one can inject a single error. A push to the
// bare gate runs no hook, so a run "created by the push" is simulated by
// inserting a terminal row at the first active-run poll.
type triggerFixture struct {
	t       *testing.T
	d       *db.DB
	env     *axiEnv
	gateDir string
	head    string
	oldRun  *db.Run

	// Injection knobs, set before calling trigger.
	failHeadRead    int32 // 1-based get_runs_for_head call that fails; 0 never
	failActiveRead  int32 // 1-based get_active_run call that fails; 0 never
	freshOnFirstGet bool  // insert a fast-terminal run at the first active poll
	onActiveRead    func()

	headReads   atomic.Int32
	activeReads atomic.Int32
	reruns      atomic.Int32
	freshRunID  atomic.Value
}

// newTriggerFixture builds a feature branch whose head H already carries one
// old failed run. With gateHasHead the gate already holds H, so the push is a
// no-op; otherwise the push delivers a new head.
func newTriggerFixture(t *testing.T, gateHasHead bool) *triggerFixture {
	t.Helper()
	dir := t.TempDir()
	cliGit(t, dir, "init", "-b", "main")
	cliGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	base := cliGit(t, dir, "rev-parse", "HEAD")

	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	root, err := git.FindGitRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(root, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, dir, "clone", "--bare", dir, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, dir, "remote", "add", gate.RemoteName, gateDir)

	cliGit(t, dir, "checkout", "-b", triggerFixtureBranch)
	cliGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "feature work")
	head := cliGit(t, dir, "rev-parse", "HEAD")
	if gateHasHead {
		cliGit(t, dir, "push", gate.RemoteName, "refs/heads/"+triggerFixtureBranch)
	}
	chdir(t, dir)

	oldRun, err := d.InsertRun(repo.ID, triggerFixtureBranch, head, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(oldRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	f := &triggerFixture{t: t, d: d, gateDir: gateDir, head: head, oldRun: oldRun}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGetRunsForHead, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		if f.headReads.Add(1) == f.failHeadRead {
			return nil, errors.New("injected head read failure")
		}
		var params ipc.GetRunsForHeadParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		runs, err := d.GetRunsByRepoHead(params.RepoID, params.Branch, params.HeadSHA)
		if err != nil {
			return nil, err
		}
		infos := make([]ipc.RunInfo, 0, len(runs))
		for _, run := range runs {
			infos = append(infos, ipc.RunInfo{ID: run.ID, RepoID: run.RepoID, Branch: run.Branch, HeadSHA: run.HeadSHA, Status: run.Status})
		}
		return &ipc.GetRunsResult{Runs: infos}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		n := f.activeReads.Add(1)
		if n == 1 && f.freshOnFirstGet {
			fresh := f.insertRun(types.RunFailed)
			f.freshRunID.Store(fresh.ID)
		}
		if f.onActiveRead != nil {
			f.onActiveRead()
		}
		if n == f.failActiveRead {
			return nil, errors.New("injected active read failure")
		}
		return &ipc.GetActiveRunResult{}, nil
	})
	srv.Handle(ipc.MethodRerun, func(context.Context, json.RawMessage) (interface{}, error) {
		f.reruns.Add(1)
		return &ipc.RerunResult{RunID: f.insertRun(types.RunPending).ID}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() { srv.Close(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if alive, _ := daemon.IsRunning(p); alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test IPC server did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	f.env = &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig(), client: client}
	return f
}

func (f *triggerFixture) insertRun(status types.RunStatus) *db.Run {
	run, err := f.d.InsertRun(f.env.repo.ID, triggerFixtureBranch, f.head, "")
	if err != nil {
		f.t.Error(err)
		return &db.Run{}
	}
	if err := f.d.UpdateRunStatus(run.ID, status); err != nil {
		f.t.Error(err)
	}
	return run
}

func (f *triggerFixture) trigger(ctx context.Context) (string, error) {
	return triggerRun(ctx, f.env, triggerFixtureBranch, f.head, nil, "probe launch", "")
}

func (f *triggerFixture) runCount() int {
	f.t.Helper()
	runs, err := f.d.GetRunsByRepo(f.env.repo.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(runs)
}

// gateHead returns the gate's branch tip, or "" when the gate lacks the branch.
func (f *triggerFixture) gateHead() string {
	out, err := git.Run(context.Background(), f.gateDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+triggerFixtureBranch)
	if err != nil {
		return ""
	}
	return out
}

func triggerTestContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A successful baseline lets the poll attach to a run the push created even
// when that run is already terminal before it was ever seen active.
func TestTriggerRunAttachesFreshTerminalRunWithoutRerun(t *testing.T) {
	f := newTriggerFixture(t, false)
	f.freshOnFirstGet = true

	runID, err := f.trigger(triggerTestContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if fresh, _ := f.freshRunID.Load().(string); runID == "" || runID != fresh {
		t.Fatalf("run ID = %q, want fresh terminal run %q", runID, fresh)
	}
	if f.reruns.Load() != 0 || f.runCount() != 2 {
		t.Fatalf("reruns = %d, rows = %d; want 0 reruns and old+fresh rows", f.reruns.Load(), f.runCount())
	}
	if got := f.gateHead(); got != f.head {
		t.Fatalf("gate head = %q, want pushed head %q", got, f.head)
	}
}

// A no-op push whose head only has an old terminal run is a fresh validation
// request: the baseline excludes the old run and the fallback reruns once.
func TestTriggerRunNoOpPushWithOldTerminalRunReruns(t *testing.T) {
	f := newTriggerFixture(t, true)

	runID, err := f.trigger(triggerTestContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" || runID == f.oldRun.ID {
		t.Fatalf("run ID = %q, want a new rerun, never old run %q", runID, f.oldRun.ID)
	}
	if f.reruns.Load() != 1 || f.runCount() != 2 {
		t.Fatalf("reruns = %d, rows = %d; want exactly one rerun", f.reruns.Load(), f.runCount())
	}
}
