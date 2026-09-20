package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/axiapi"
)

// The run-state reconciliation policy lives in internal/axiapi so the MCP
// gateway watches a run exactly the way `no-mistakes axi` does - one owner of
// the subscribe-before-read, reconnect, coalesce, and heartbeat rules. These
// aliases keep the CLI's own spelling.
type (
	runStateSource    = axiapi.RunStateSource
	ipcRunStateSource = axiapi.IPCRunStateSource
	runReconciler     = axiapi.RunReconciler
)

var driveGetRunTimeoutNS = &axiapi.DriveGetRunTimeoutNS

func newRunReconciler(source runStateSource, runID string) *runReconciler {
	return axiapi.NewRunReconciler(source, runID)
}
