package ceph

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// execTimeout bounds every external command this tool runs (cephadm shell
// invocations, smartctl, lsblk, journalctl, ...): a hung command must fail
// the current run rather than stall the operator indefinitely.
const execTimeout = 30 * time.Second

// ExecOutput runs name with args under a 30s timeout and returns stdout.
// Non-timeout failures return whatever output was collected so far alongside
// the original error (including *exec.ExitError, so callers can still inspect
// Stderr) - smartctl and shell actions legitimately exit non-zero with usable
// stdout; a timeout is reported explicitly with no output.
func ExecOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s timed out after %s", name, execTimeout)
		}
		return out, err
	}
	return out, nil
}

// execRun is ExecOutput's counterpart for commands whose output isn't
// needed - the exit status is the signal.
func execRun(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	err := exec.CommandContext(ctx, name, args...).Run()
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out after %s", name, execTimeout)
	}
	return err
}
