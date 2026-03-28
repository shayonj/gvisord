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

// Package e2e contains end-to-end tests for the gvisord daemon.
//
// These tests exercise the full daemon lifecycle using the fake runtime:
// config → Manager → Pool → API → client round-trip → lease → complete.
//
// To run against real runsc on a Linux host, set GVISORD_E2E_RUNSC=1 and
// ensure runsc, rootfs, and checkpoint are available. The fake runtime tests
// validate the entire control plane without requiring gVisor.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shayonj/gvisord/internal/api"
	"github.com/shayonj/gvisord/internal/config"
	"github.com/shayonj/gvisord/internal/pool"
	"github.com/shayonj/gvisord/internal/runsc"
)

// --- fake runtime for e2e ---

type fakeRuntime struct {
	mu       sync.Mutex
	statePID int
	nextPID  int
}

func (f *fakeRuntime) Create(rootDir, bundleDir, id string) error { return nil }
func (f *fakeRuntime) Start(rootDir, id string) error             { return nil }
func (f *fakeRuntime) Restore(rootDir, bundleDir, ckpt, id string) error {
	return nil
}
func (f *fakeRuntime) Kill(rootDir, id, signal string) error   { return nil }
func (f *fakeRuntime) Wait(rootDir, id string) (int, error)    { return 0, nil }
func (f *fakeRuntime) Reset(rootDir, id string) error          { return nil }
func (f *fakeRuntime) Delete(rootDir, id string) error         { return nil }
func (f *fakeRuntime) PrepareBundle(bd, rf, cd string) error {
	if err := os.MkdirAll(bd, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bd, "config.json"), []byte(`{"ociVersion":"1.0","mounts":[],"linux":{"namespaces":[]}}`), 0644)
}
func (f *fakeRuntime) Exec(rootDir, id string, args []string) ([]byte, error) {
	return []byte("exec-output"), nil
}
func (f *fakeRuntime) WarmSentry() bool  { return false }
func (f *fakeRuntime) CleanFilestores(rootfs string) {}
func (f *fakeRuntime) KillProcess(pid int)                     {}
func (f *fakeRuntime) State(rootDir, id string) (*runsc.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPID++
	return &runsc.ContainerState{ID: id, PID: f.nextPID, Status: "created"}, nil
}

// --- helpers ---

// shortSocketPath creates a socket path short enough for Unix domain sockets
// (max 108 bytes on most systems). macOS TempDir paths are very long, so
// we create a short symlink under /tmp.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "gvs-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	t.Cleanup(func() { os.Remove(path) })
	return path
}

type apiClient struct {
	sockPath string
}

func (c *apiClient) call(method string, params any) (json.RawMessage, error) {
	conn, err := net.Dial("unix", c.sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	type request struct {
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}
	if err := json.NewEncoder(conn).Encode(request{Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	var resp struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  string          `json:"error,omitempty"`
		Code   string          `json:"code,omitempty"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s: %s", resp.Code, resp.Error)
	}
	return resp.Result, nil
}

// --- E2E tests ---

// TestFullDaemonLifecycle tests the complete daemon flow:
// start → health → execute → complete → status → drain
func TestFullDaemonLifecycle(t *testing.T) {
	root := t.TempDir()
	sockPath := shortSocketPath(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket:       sockPath,
			Workdir:      filepath.Join(root, "sentries"),
			LeaseTimeout: 5 * time.Minute,
		},
		Runsc: config.RunscConfig{Path: "/usr/local/bin/runsc"},
		Limits: config.DefaultLimits,
		Templates: map[string]config.Template{
			"python": {
				Rootfs:     filepath.Join(root, "rootfs"),
				Checkpoint: filepath.Join(root, "ckpt"),
				Pools: []config.PoolConfig{
					{ResourceClass: "small", PoolSize: 2, PreRestore: true},
				},
			},
		},
		ResourceClasses: []config.ResourceClassConfig{
			{Name: "small", CPUMillis: 500, MemoryMB: 512},
		},
	}

	// Build manager with fake runtime
	rt := &fakeRuntime{statePID: 100}
	mgr := pool.NewManagerForTest(cfg, log, rt, pool.ManagerOpts{})
	if err := mgr.Start(); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	defer mgr.Shutdown()

	// Start API server
	srv := api.New(mgr, cfg, log)
	go func() { _ = srv.Serve(sockPath) }()

	// Wait for socket
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client := &apiClient{sockPath: sockPath}

	// 1. Health check
	t.Run("health", func(t *testing.T) {
		result, err := client.call("health", nil)
		if err != nil {
			t.Fatal(err)
		}
		var health map[string]any
		if err := json.Unmarshal(result, &health); err != nil {
			t.Fatal(err)
		}
		if health["healthy"] != true {
			t.Errorf("healthy = %v, want true", health["healthy"])
		}
	})

	// 2. Execute → get lease
	var leaseID string
	t.Run("execute", func(t *testing.T) {
		result, err := client.call("execute", map[string]string{"workload": "python/small"})
		if err != nil {
			t.Fatal(err)
		}
		var execResult pool.ExecuteResult
		if err := json.Unmarshal(result, &execResult); err != nil {
			t.Fatal(err)
		}
		if execResult.Workload != "python/small" {
			t.Errorf("workload = %q", execResult.Workload)
		}
		if execResult.LeaseID == "" {
			t.Fatal("expected lease_id")
		}
		if execResult.PID == 0 {
			t.Error("expected non-zero PID")
		}
		leaseID = execResult.LeaseID
	})

	// 3. Status shows active lease
	t.Run("status_with_lease", func(t *testing.T) {
		result, err := client.call("status", nil)
		if err != nil {
			t.Fatal(err)
		}
		var status pool.ManagerStatus
		if err := json.Unmarshal(result, &status); err != nil {
			t.Fatal(err)
		}
		if status.ActiveLeases != 1 {
			t.Errorf("active_leases = %d, want 1", status.ActiveLeases)
		}
		if len(status.Pools) != 1 {
			t.Fatalf("pools = %d, want 1", len(status.Pools))
		}
		ps := status.Pools[0]
		if ps.TotalExecutions != 1 {
			t.Errorf("total_executions = %d, want 1", ps.TotalExecutions)
		}
	})

	// 4. Complete the lease
	t.Run("complete", func(t *testing.T) {
		_, err := client.call("complete", map[string]string{"lease_id": leaseID})
		if err != nil {
			t.Fatal(err)
		}
	})

	// 5. Double complete fails
	t.Run("double_complete", func(t *testing.T) {
		_, err := client.call("complete", map[string]string{"lease_id": leaseID})
		if err == nil {
			t.Fatal("expected error on double complete")
		}
	})

	// 6. Execute unknown workload fails
	t.Run("unknown_workload", func(t *testing.T) {
		_, err := client.call("execute", map[string]string{"workload": "ruby"})
		if err == nil {
			t.Fatal("expected error for unknown workload")
		}
	})

	// 7. Unknown method fails
	t.Run("unknown_method", func(t *testing.T) {
		_, err := client.call("reboot", nil)
		if err == nil {
			t.Fatal("expected error for unknown method")
		}
	})

	// 8. Drain
	t.Run("drain", func(t *testing.T) {
		_, err := client.call("drain", nil)
		if err != nil {
			t.Fatal(err)
		}
	})
}

// TestConcurrentExecuteAndComplete tests multiple callers competing
// for a limited pool, then completing leases.
func TestConcurrentExecuteAndComplete(t *testing.T) {
	root := t.TempDir()
	sockPath := shortSocketPath(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	poolSize := 3
	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket:       sockPath,
			Workdir:      filepath.Join(root, "sentries"),
			LeaseTimeout: 5 * time.Minute,
		},
		Runsc:  config.RunscConfig{Path: "/usr/local/bin/runsc"},
		Limits: config.DefaultLimits,
		Templates: map[string]config.Template{
			"node": {
				Rootfs:     filepath.Join(root, "rootfs"),
				Checkpoint: filepath.Join(root, "ckpt"),
				Pools: []config.PoolConfig{
					{ResourceClass: "medium", PoolSize: poolSize, PreRestore: true, MaxPending: 10},
				},
			},
		},
		ResourceClasses: []config.ResourceClassConfig{
			{Name: "medium", CPUMillis: 1000, MemoryMB: 1024},
		},
	}

	rt := &fakeRuntime{}
	mgr := pool.NewManagerForTest(cfg, log, rt, pool.ManagerOpts{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown()

	srv := api.New(mgr, cfg, log)
	go func() { _ = srv.Serve(sockPath) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client := &apiClient{sockPath: sockPath}
	numClients := poolSize

	var wg sync.WaitGroup
	results := make(chan string, numClients)
	errors := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := client.call("execute", map[string]string{"workload": "node/medium"})
			if err != nil {
				errors <- err
				return
			}
			var res pool.ExecuteResult
			if err := json.Unmarshal(raw, &res); err != nil {
				errors <- err
				return
			}
			results <- res.LeaseID
		}()
	}
	wg.Wait()
	close(results)
	close(errors)

	for err := range errors {
		t.Errorf("execute error: %v", err)
	}

	// Complete all leases
	for leaseID := range results {
		if _, err := client.call("complete", map[string]string{"lease_id": leaseID}); err != nil {
			t.Errorf("complete(%s): %v", leaseID, err)
		}
	}

	// Verify status is clean
	raw, err := client.call("status", nil)
	if err != nil {
		t.Fatal(err)
	}
	var status pool.ManagerStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status.ActiveLeases != 0 {
		t.Errorf("active_leases = %d, want 0 after completing all", status.ActiveLeases)
	}
	if len(status.Pools) != 1 || status.Pools[0].TotalExecutions != int64(numClients) {
		t.Errorf("total_executions = %d, want %d", status.Pools[0].TotalExecutions, numClients)
	}

	srv.Stop()
}

// TestExecuteWithCheckpointOverride tests the checkpoint override flow.
func TestExecuteWithCheckpointOverride(t *testing.T) {
	root := t.TempDir()
	sockPath := shortSocketPath(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket:       sockPath,
			Workdir:      filepath.Join(root, "sentries"),
			LeaseTimeout: 5 * time.Minute,
		},
		Runsc:  config.RunscConfig{Path: "/usr/local/bin/runsc"},
		Limits: config.DefaultLimits,
		Templates: map[string]config.Template{
			"python": {
				Rootfs:     filepath.Join(root, "rootfs"),
				Checkpoint: filepath.Join(root, "ckpt-default"),
				Pools: []config.PoolConfig{
					{ResourceClass: "small", PoolSize: 1, PreRestore: false},
				},
			},
		},
		ResourceClasses: []config.ResourceClassConfig{
			{Name: "small", CPUMillis: 500, MemoryMB: 512},
		},
	}

	rt := &fakeRuntime{}
	mgr := pool.NewManagerForTest(cfg, log, rt, pool.ManagerOpts{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown()

	srv := api.New(mgr, cfg, log)
	go func() { _ = srv.Serve(sockPath) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client := &apiClient{sockPath: sockPath}
	raw, err := client.call("execute", map[string]string{
		"workload":   "python/small",
		"checkpoint": "/custom/ckpt",
	})
	if err != nil {
		t.Fatal(err)
	}

	var res pool.ExecuteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.Checkpoint != "/custom/ckpt" {
		t.Errorf("checkpoint = %q, want /custom/ckpt", res.Checkpoint)
	}

	_, _ = client.call("complete", map[string]string{"lease_id": res.LeaseID})
	srv.Stop()
}

// TestBadRequest tests malformed requests.
func TestBadRequest(t *testing.T) {
	root := t.TempDir()
	sockPath := shortSocketPath(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket:       sockPath,
			Workdir:      filepath.Join(root, "sentries"),
			LeaseTimeout: 5 * time.Minute,
		},
		Runsc:  config.RunscConfig{Path: "/usr/local/bin/runsc"},
		Limits: config.DefaultLimits,
		Templates: map[string]config.Template{
			"python": {
				Rootfs:     filepath.Join(root, "rootfs"),
				Checkpoint: filepath.Join(root, "ckpt"),
				Pools: []config.PoolConfig{
					{ResourceClass: "small", PoolSize: 1, PreRestore: true},
				},
			},
		},
		ResourceClasses: []config.ResourceClassConfig{
			{Name: "small", CPUMillis: 500, MemoryMB: 512},
		},
	}

	rt := &fakeRuntime{}
	mgr := pool.NewManagerForTest(cfg, log, rt, pool.ManagerOpts{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown()

	srv := api.New(mgr, cfg, log)
	go func() { _ = srv.Serve(sockPath) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client := &apiClient{sockPath: sockPath}

	// Execute without workload
	_, err := client.call("execute", map[string]string{})
	if err == nil {
		t.Error("expected error for missing workload")
	}

	// Complete without lease_id
	_, err = client.call("complete", map[string]string{})
	if err == nil {
		t.Error("expected error for missing lease_id")
	}

	// Run without workload
	_, err = client.call("run", map[string]string{"script": "print(1)"})
	if err == nil {
		t.Error("expected error for run without workload")
	}

	// Run without script or command
	_, err = client.call("run", map[string]string{"workload": "python/small"})
	if err == nil {
		t.Error("expected error for run without script")
	}

	srv.Stop()
}

// TestRunMethodIntegration tests the run method which does the full
// acquire-harness-complete cycle internally. Since we use a fake runtime
// and no real harness is listening, the run call will fail at the HTTP
// step. We verify the error is from the harness call, not from pool
// acquisition.
func TestRunMethodPoolAcquire(t *testing.T) {
	root := t.TempDir()
	sockPath := shortSocketPath(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			Socket:       sockPath,
			Workdir:      filepath.Join(root, "sentries"),
			LeaseTimeout: 5 * time.Minute,
		},
		Runsc:  config.RunscConfig{Path: "/usr/local/bin/runsc"},
		Limits: config.DefaultLimits,
		Templates: map[string]config.Template{
			"python": {
				Rootfs:     filepath.Join(root, "rootfs"),
				Checkpoint: filepath.Join(root, "ckpt"),
				Pools: []config.PoolConfig{
					{ResourceClass: "small", PoolSize: 1, PreRestore: true},
				},
			},
		},
		ResourceClasses: []config.ResourceClassConfig{
			{Name: "small", CPUMillis: 500, MemoryMB: 512},
		},
	}

	rt := &fakeRuntime{}
	mgr := pool.NewManagerForTest(cfg, log, rt, pool.ManagerOpts{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown()

	srv := api.New(mgr, cfg, log)
	go func() { _ = srv.Serve(sockPath) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	client := &apiClient{sockPath: sockPath}

	// No CNI, so sentry has no IP. The run method falls back to
	// runsc exec via the fake runtime, which returns "exec-output".
	raw, err := client.call("run", map[string]any{
		"workload": "python/small",
		"script":   "print(1)",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var runResult pool.RunResult
	if err := json.Unmarshal(raw, &runResult); err != nil {
		t.Fatal(err)
	}
	if runResult.Stdout != "exec-output" {
		t.Errorf("stdout = %q, want exec-output", runResult.Stdout)
	}
	if runResult.SentryID == "" {
		t.Error("expected sentry_id in run result")
	}

	result, err := client.call("health", nil)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var health map[string]any
	if err := json.Unmarshal(result, &health); err != nil {
		t.Fatal(err)
	}

	srv.Stop()
}
