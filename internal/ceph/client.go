package ceph

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// Client wraps Ceph CLI commands.
type Client struct{}

// NewClient creates a new Ceph client.
func NewClient() *Client {
	return &Client{}
}

// Run executes a command (argv-style) and returns parsed JSON.
func (c *Client) Run(args []string) (map[string]interface{}, error) {
	stdout, err := ExecOutput(args[0], args[1:]...)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("ceph command stderr: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("ceph command failed: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}
	return result, nil
}

// All cluster reads below go through `cephadm shell` because OSD nodes (where
// analyze runs) have no client.admin keyring in /etc/ceph: a bare `ceph tell`
// fails to authenticate with "[errno 13] RADOS permission denied". cephadm
// shell mounts the keyring from its own state dir - the same access method the
// operator's rules use. cephadm logs go to stderr, so Run() still sees clean
// JSON on stdout.

// OSDHistoricOps fetches historic operations from an OSD.
func (c *Client) OSDHistoricOps(osdID int) (map[string]interface{}, error) {
	return c.Run([]string{"cephadm", "shell", "ceph", "tell", fmt.Sprintf("osd.%d", osdID), "dump_historic_ops", "--format", "json"})
}

// OSDPerfDump fetches performance dump from an OSD.
func (c *Client) OSDPerfDump(osdID int) (map[string]interface{}, error) {
	return c.Run([]string{"cephadm", "shell", "ceph", "tell", fmt.Sprintf("osd.%d", osdID), "perf", "dump", "--format", "json"})
}

// OSDOpsInFlight fetches in-flight operations from an OSD.
func (c *Client) OSDOpsInFlight(osdID int) (map[string]interface{}, error) {
	return c.Run([]string{"cephadm", "shell", "ceph", "tell", fmt.Sprintf("osd.%d", osdID), "dump_ops_in_flight", "--format", "json"})
}

// OSDCounterDump fetches counter dump from an OSD.
func (c *Client) OSDCounterDump(osdID int) (map[string]interface{}, error) {
	return c.Run([]string{"cephadm", "shell", "ceph", "tell", fmt.Sprintf("osd.%d", osdID), "counter", "dump", "--format", "json"})
}

// OSDLs lists all OSD IDs.
func (c *Client) OSDLs() ([]int, error) {
	stdout, err := ExecOutput("cephadm", "shell", "ceph", "osd", "ls", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("failed to list OSDs: %w", err)
	}

	var ids []int
	if err := json.Unmarshal(stdout, &ids); err != nil {
		return nil, fmt.Errorf("failed to parse OSD list: %w", err)
	}
	return ids, nil
}

// OSDOkToStop checks if an OSD is safe to stop.
func (c *Client) OSDOkToStop(osdID string) (bool, error) {
	_, err := ExecOutput("cephadm", "shell", "ceph", "osd", "ok-to-stop", osdID)
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil // Not safe to stop
		}
		return false, fmt.Errorf("failed to check ok-to-stop: %w", err)
	}
	return true, nil
}

// OSDStopped checks if an OSD service is stopped. A non-zero exit from
// `cephadm unit status` means the unit is inactive/stopped; any other
// failure (e.g. the cephadm binary missing entirely) is an error - treating
// it as "stopped" would silently blind every rule that ends in an OSD stop.
func (c *Client) OSDStopped(osdID string) (bool, error) {
	err := execRun("cephadm", "unit", "status", "--name", fmt.Sprintf("osd.%s", osdID))
	if err == nil {
		return false, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return true, nil
	}
	return false, fmt.Errorf("failed to check unit status: %w", err)
}

// ShellCommand executes a shell command (used by operator actions).
func (c *Client) ShellCommand(command string) (bool, string, string) {
	stdout, err := ExecOutput("sh", "-c", command)
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return false, string(stdout), string(exitErr.Stderr)
		}
		return false, "", err.Error()
	}
	return true, string(stdout), ""
}
