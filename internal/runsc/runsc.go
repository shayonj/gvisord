// Copyright 2026 Shayon Mukherjee
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package runsc wraps the runsc CLI for sentry lifecycle management.
package runsc

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Runtime captures the runsc operations gvisord needs at the runtime boundary.
// It is intentionally small so higher-level packages can stub it in tests.
type Runtime interface {
	Create(rootDir, bundleDir, id string) error
	Start(rootDir, id string) error
	Restore(rootDir, bundleDir, checkpointDir, id string) error
	Kill(rootDir, id, signal string) error
	Wait(rootDir, id string) (int, error)
	Reset(rootDir, id string) error
	Delete(rootDir, id string) error
	State(rootDir, id string) (*ContainerState, error)
	Exec(rootDir, id string, args []string) ([]byte, error)
	PrepareBundle(bundleDir, rootfs, checkpointDir string) error
	CleanFilestores(rootfs string)
	KillProcess(pid int)
	WarmSentry() bool
}

// Client wraps the runsc binary.
type Client struct {
	binPath    string
	extraFlags []string
	warmSentry bool
	log        *slog.Logger
}

var _ Runtime = (*Client)(nil)

func NewClient(binPath string, extraFlags []string, log *slog.Logger) *Client {
	warm := false
	filtered := make([]string, 0, len(extraFlags))
	for _, f := range extraFlags {
		if f == "--warm-sentry" {
			warm = true
		} else {
			filtered = append(filtered, f)
		}
	}
	return &Client{binPath: binPath, extraFlags: filtered, warmSentry: warm, log: log}
}

// WarmSentry returns whether the client was configured with warm sentry support.
func (c *Client) WarmSentry() bool {
	return c.warmSentry
}

// run executes a runsc command and returns combined output.
// Stderr is captured to a temp file (not a pipe) to avoid blocking
// when forked child processes (e.g. the sentry) inherit the FDs.
func (c *Client) run(rootDir string, args ...string) ([]byte, error) {
	cmdArgs := append(c.extraFlags, "--root="+rootDir)
	cmdArgs = append(cmdArgs, args...)

	c.log.Debug("runsc exec", "args", args)

	stderrFile, err := os.CreateTemp("", "gvisord-runsc-*")
	if err != nil {
		return nil, fmt.Errorf("creating stderr tempfile: %w", err)
	}
	defer func() {
		stderrFile.Close()
		os.Remove(stderrFile.Name())
	}()

	stdoutFile, err := os.CreateTemp("", "gvisord-runsc-out-*")
	if err != nil {
		return nil, fmt.Errorf("creating stdout tempfile: %w", err)
	}
	defer func() {
		stdoutFile.Close()
		os.Remove(stdoutFile.Name())
	}()

	cmd := exec.Command(c.binPath, cmdArgs...)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	if err := cmd.Run(); err != nil {
		if _, seekErr := stderrFile.Seek(0, 0); seekErr != nil {
			return nil, fmt.Errorf("rewinding stderr tempfile: %w", seekErr)
		}
		stderrBytes, _ := io.ReadAll(stderrFile)
		return nil, fmt.Errorf("runsc %s: %w\n%s", strings.Join(args, " "), err, stderrBytes)
	}

	if _, err := stdoutFile.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("rewinding stdout tempfile: %w", err)
	}
	out, _ := io.ReadAll(stdoutFile)
	return out, nil
}

// Create creates a container. If warm sentry is enabled, the sentry process
// persists across restore/reset cycles. Otherwise, a standard container is
// created that must be started with Start.
func (c *Client) Create(rootDir, bundleDir, id string) error {
	args := []string{}
	if c.warmSentry {
		args = append(args, "--warm-sentry")
	}
	args = append(args, "create", "--bundle="+bundleDir, id)
	_, err := c.run(rootDir, args...)
	return err
}

// Start starts a created container (standard mode, no checkpoint).
func (c *Client) Start(rootDir, id string) error {
	_, err := c.run(rootDir, "start", id)
	return err
}

// Restore loads a checkpoint into a container. With warm sentry, this restores
// into the existing sentry process. Without warm sentry, this creates a new
// container from the checkpoint.
func (c *Client) Restore(rootDir, bundleDir, checkpointDir, id string) error {
	args := []string{}
	if c.warmSentry {
		args = append(args, "--warm-sentry")
	}
	args = append(args, "restore", "--detach",
		"--image-path="+checkpointDir,
		"--bundle="+bundleDir,
		id)
	_, err := c.run(rootDir, args...)
	return err
}

// Kill sends a signal to the container.
func (c *Client) Kill(rootDir, id, signal string) error {
	_, err := c.run(rootDir, "kill", id, signal)
	return err
}

// Wait waits for the container init process to exit.
func (c *Client) Wait(rootDir, id string) (int, error) {
	out, err := c.run(rootDir, "wait", id)
	if err != nil {
		return -1, err
	}
	var result struct {
		ExitStatus int `json:"exitStatus"`
	}
	if jsonErr := json.Unmarshal(out, &result); jsonErr != nil {
		return -1, fmt.Errorf("parsing wait result: %w", jsonErr)
	}
	return result.ExitStatus, nil
}

// Reset resets a warm sentry for the next restore cycle. Only valid when
// warm sentry mode is enabled; returns an error from runsc otherwise.
func (c *Client) Reset(rootDir, id string) error {
	_, err := c.run(rootDir, "reset", id)
	return err
}

// Exec runs a command inside a running container and returns its stdout.
func (c *Client) Exec(rootDir, id string, args []string) ([]byte, error) {
	cmdArgs := []string{"exec", id}
	cmdArgs = append(cmdArgs, args...)
	return c.run(rootDir, cmdArgs...)
}

// Delete deletes a container.
func (c *Client) Delete(rootDir, id string) error {
	_, err := c.run(rootDir, "delete", id)
	return err
}

// State returns the container state.
func (c *Client) State(rootDir, id string) (*ContainerState, error) {
	out, err := c.run(rootDir, "state", id)
	if err != nil {
		return nil, err
	}
	var state ContainerState
	if err := json.Unmarshal(out, &state); err != nil {
		return nil, fmt.Errorf("parsing state: %w", err)
	}
	return &state, nil
}

func (c *Client) PrepareBundle(bundleDir, rootfs, checkpointDir string) error {
	return PrepareBundle(bundleDir, rootfs, checkpointDir)
}

func (c *Client) CleanFilestores(rootfs string) {
	CleanFilestores(rootfs)
}

func (c *Client) KillProcess(pid int) {
	KillProcess(pid)
}

type ContainerState struct {
	ID     string `json:"id"`
	PID    int    `json:"pid"`
	Status string `json:"status"`
}

// KillProcess sends SIGKILL to a process by PID. Best-effort.
func KillProcess(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGKILL)
	_, _ = proc.Wait()
}

// PrepareBundle creates an OCI bundle directory with a symlinked rootfs.
func PrepareBundle(bundleDir, rootfs, checkpointDir string) error {
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		return err
	}
	rootfsLink := filepath.Join(bundleDir, "rootfs")
	os.Remove(rootfsLink)
	if err := os.Symlink(rootfs, rootfsLink); err != nil {
		return fmt.Errorf("symlinking rootfs: %w", err)
	}

	configSrc := filepath.Join(checkpointDir, "config.json")
	configDst := filepath.Join(bundleDir, "config.json")
	data, err := os.ReadFile(configSrc)
	if err != nil {
		return fmt.Errorf("reading checkpoint config.json: %w", err)
	}
	if err := os.WriteFile(configDst, data, 0644); err != nil {
		return fmt.Errorf("writing bundle config.json: %w", err)
	}
	return nil
}

// InjectBundleMounts reads the OCI config.json from bundleDir, adds bind
// mount entries for the harness binary and cache directory, and writes
// the modified config back. Mounts are read-only.
func InjectBundleMounts(bundleDir, harnessPath, cacheDir string) error {
	configPath := filepath.Join(bundleDir, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading bundle config.json: %w", err)
	}

	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("parsing bundle config.json: %w", err)
	}

	mounts, _ := spec["mounts"].([]any)

	if harnessPath != "" {
		mounts = append(mounts, map[string]any{
			"destination": "/harness/gvisord-exec",
			"type":        "bind",
			"source":      harnessPath,
			"options":     []any{"rbind", "ro"},
		})
	}

	if cacheDir != "" {
		mounts = append(mounts, map[string]any{
			"destination": "/cache",
			"type":        "bind",
			"source":      cacheDir,
			"options":     []any{"rbind", "ro"},
		})
	}

	spec["mounts"] = mounts

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling modified config.json: %w", err)
	}
	return os.WriteFile(configPath, out, 0644)
}

// InjectNetNS reads the OCI config.json from bundleDir and adds the
// network namespace path to linux.namespaces. If a network namespace
// entry already exists, its path is updated.
func InjectNetNS(bundleDir, netnsPath string) error {
	if netnsPath == "" {
		return nil
	}

	configPath := filepath.Join(bundleDir, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading bundle config.json: %w", err)
	}

	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("parsing bundle config.json: %w", err)
	}

	linux, _ := spec["linux"].(map[string]any)
	if linux == nil {
		linux = map[string]any{}
		spec["linux"] = linux
	}

	namespaces, _ := linux["namespaces"].([]any)

	found := false
	for i, ns := range namespaces {
		nsMap, ok := ns.(map[string]any)
		if ok && nsMap["type"] == "network" {
			nsMap["path"] = netnsPath
			namespaces[i] = nsMap
			found = true
			break
		}
	}
	if !found {
		namespaces = append(namespaces, map[string]any{
			"type": "network",
			"path": netnsPath,
		})
	}
	linux["namespaces"] = namespaces

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling modified config.json: %w", err)
	}
	return os.WriteFile(configPath, out, 0644)
}

// CleanFilestores removes .gvisor* filestore files from the rootfs root
// directory. These files are created by runsc at the top level of the
// rootfs, so a targeted readdir is sufficient (no recursive walk needed).
func CleanFilestores(rootfs string) {
	entries, err := os.ReadDir(rootfs)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gvisor") {
			os.Remove(filepath.Join(rootfs, e.Name()))
		}
	}
}
