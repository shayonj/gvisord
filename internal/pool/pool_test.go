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

package pool

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shayonj/gvisord/internal/config"
	"github.com/shayonj/gvisord/internal/runsc"
)

// --- fake runtime ---

type fakeRuntime struct {
	mu                  sync.Mutex
	statePID            int
	restoreCheckpoints  []string
	prepareBundleCalls  int
	cleanFilestoresCall int
	createCalls         int
	deleteCalls         int
	killCalls           []string
	resetCalls          int
	waitCalls           int
	resetErr            error
	restoreErr          error
	warmSentry          bool
}

func (f *fakeRuntime) Create(rootDir, bundleDir, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	return nil
}
func (f *fakeRuntime) Start(rootDir, id string) error { return nil }

func (f *fakeRuntime) Restore(rootDir, bundleDir, checkpointDir, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreCheckpoints = append(f.restoreCheckpoints, checkpointDir)
	return f.restoreErr
}

func (f *fakeRuntime) Kill(rootDir, id, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killCalls = append(f.killCalls, signal)
	return nil
}

func (f *fakeRuntime) Wait(rootDir, id string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitCalls++
	return 0, nil
}

func (f *fakeRuntime) Reset(rootDir, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
	return f.resetErr
}

func (f *fakeRuntime) Delete(rootDir, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	return nil
}

func (f *fakeRuntime) State(rootDir, id string) (*runsc.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &runsc.ContainerState{ID: id, PID: f.statePID, Status: "created"}, nil
}

func (f *fakeRuntime) PrepareBundle(bundleDir, rootfs, checkpointDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareBundleCalls++
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"ociVersion":"1.0","mounts":[],"linux":{"namespaces":[]}}`), 0644)
}

func (f *fakeRuntime) CleanFilestores(rootfs string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanFilestoresCall++
}

func (f *fakeRuntime) Exec(rootDir, id string, args []string) ([]byte, error) {
	return []byte("exec-output"), nil
}
func (f *fakeRuntime) WarmSentry() bool { return f.warmSentry }
func (f *fakeRuntime) KillProcess(pid int) {}


// --- Sentry tests ---

func TestSentryHealthCheck(t *testing.T) {
	s := &Sentry{
		ID:        "test-sentry",
		PID:       1,
		Restores:  5,
		Created:   time.Now(),
		BaseRSSKB: 1000,
	}

	if err := s.HealthCheck(100, 10*time.Minute, 1<<20, 1024); err != nil {
		t.Errorf("expected health check to pass: %v", err)
	}

	if err := s.HealthCheck(3, 10*time.Minute, 1<<20, 1024); err == nil {
		t.Error("expected health check to fail on restore count")
	}

	s.Created = time.Now().Add(-20 * time.Minute)
	if err := s.HealthCheck(100, 10*time.Minute, 1<<20, 1024); err == nil {
		t.Error("expected health check to fail on age")
	}
}

func TestSentryInfo(t *testing.T) {
	s := &Sentry{
		ID:       "test-info",
		PID:      1,
		State:    StateReady,
		Restores: 3,
		Created:  time.Now().Add(-30 * time.Second),
		IP:       "10.88.0.5",
	}
	info := s.Info()
	if info.ID != "test-info" {
		t.Errorf("got ID=%q, want test-info", info.ID)
	}
	if info.State != "ready" {
		t.Errorf("got State=%q, want ready", info.State)
	}
	if info.Restores != 3 {
		t.Errorf("got Restores=%d, want 3", info.Restores)
	}
	if info.IP != "10.88.0.5" {
		t.Errorf("got IP=%q, want 10.88.0.5", info.IP)
	}
	if info.AgeSec < 29 || info.AgeSec > 32 {
		t.Errorf("got AgeSec=%f, want ~30", info.AgeSec)
	}
}

func TestStateString(t *testing.T) {
	tests := []struct {
		state State
		want  string
	}{
		{StateReady, "ready"},
		{StateRunning, "running"},
		{StateDraining, "draining"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// --- Pool tests ---

func TestPoolShutdownUnblocksQueuedAcquire(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	p := newPool(
		"python",
		config.Workload{PoolSize: 1, MaxPending: 1, PreRestore: true},
		&config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits},
		&fakeRuntime{},
		nil, nil,
		log,
	)

	p.sentries = []*Sentry{{
		ID:      "python-1",
		PID:     0,
		RootDir: filepath.Join(root, "python-1", "root"),
		State:   StateRunning,
	}}

	errCh := make(chan error, 1)
	go func() {
		_, err := p.acquire()
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	p.Shutdown()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrPoolClosed) {
			t.Fatalf("acquire error = %v, want %v", err, ErrPoolClosed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not unblock on shutdown")
	}

	p.Shutdown()
}

func TestSpawnUsesRuntimeInterface(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	runtime := &fakeRuntime{statePID: 4242}
	p := newPool(
		"python",
		config.Workload{
			PoolSize:   1,
			PreRestore: true,
			Rootfs:     filepath.Join(root, "rootfs"),
			Checkpoint: filepath.Join(root, "ckpt"),
			MaxPending: 1,
		},
		&config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits},
		runtime,
		nil, nil,
		log,
	)

	s, err := p.spawn()
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if s.PID != 4242 {
		t.Fatalf("pid = %d, want 4242", s.PID)
	}
	if runtime.prepareBundleCalls != 1 {
		t.Fatalf("prepare bundle calls = %d, want 1", runtime.prepareBundleCalls)
	}
	if runtime.cleanFilestoresCall != 1 {
		t.Fatalf("clean filestores calls = %d, want 1", runtime.cleanFilestoresCall)
	}
	if len(runtime.restoreCheckpoints) != 1 || runtime.restoreCheckpoints[0] != p.workload.Checkpoint {
		t.Fatalf("restore checkpoints = %v, want [%q]", runtime.restoreCheckpoints, p.workload.Checkpoint)
	}
}

func TestExecuteUsesCheckpointOverrideWithRuntime(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	rt := &fakeRuntime{}
	cfg := &config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits}
	mgr := NewManagerForTest(cfg, log, rt, ManagerOpts{})

	p := newPool(
		"python",
		config.Workload{
			PoolSize:   1,
			PreRestore: false,
			Rootfs:     filepath.Join(root, "rootfs"),
			Checkpoint: filepath.Join(root, "default-ckpt"),
			MaxPending: 1,
		},
		cfg, rt, nil, nil, log,
	)

	p.sentries = []*Sentry{{
		ID:        "python-1",
		PID:       1,
		RootDir:   filepath.Join(root, "python-1", "root"),
		BundleDir: filepath.Join(root, "python-1", "bundle"),
		State:     StateReady,
		Created:   time.Now(),
		LastReset: time.Now(),
		LastUsed:  time.Now(),
	}}

	mgr.RegisterPool("python", p)

	override := filepath.Join(root, "override-ckpt")
	result, err := p.Execute(override, mgr)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Checkpoint != override {
		t.Fatalf("checkpoint = %q, want %q", result.Checkpoint, override)
	}
	if result.LeaseID == "" {
		t.Fatal("expected non-empty lease ID")
	}
	if len(rt.restoreCheckpoints) == 0 || rt.restoreCheckpoints[0] != override {
		t.Fatalf("restore checkpoints = %v, want first call %q", rt.restoreCheckpoints, override)
	}

	// Complete the lease
	if err := mgr.Complete(result.LeaseID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	mgr.Shutdown()
}

func TestLeaseLifecycle(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	rt := &fakeRuntime{statePID: 100}
	cfg := &config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits}
	mgr := NewManagerForTest(cfg, log, rt, ManagerOpts{})

	p := newPool(
		"node",
		config.Workload{
			PoolSize:   1,
			PreRestore: true,
			Rootfs:     filepath.Join(root, "rootfs"),
			Checkpoint: filepath.Join(root, "ckpt"),
			MaxPending: 2,
		},
		cfg, rt, nil, nil, log,
	)

	p.sentries = []*Sentry{{
		ID:        "node-1",
		PID:       100,
		RootDir:   filepath.Join(root, "node-1", "root"),
		BundleDir: filepath.Join(root, "node-1", "bundle"),
		State:     StateReady,
		Created:   time.Now(),
		LastReset: time.Now(),
		LastUsed:  time.Now(),
	}}

	mgr.RegisterPool("node", p)

	// Execute returns a lease
	result, err := p.Execute("", mgr)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.LeaseID == "" {
		t.Fatal("expected lease_id")
	}
	if result.SentryID != "node-1" {
		t.Fatalf("sentry_id = %q, want node-1", result.SentryID)
	}

	// Sentry should be Running while lease is active
	p.sentries[0].mu.Lock()
	st := p.sentries[0].State
	p.sentries[0].mu.Unlock()
	if st != StateRunning {
		t.Fatalf("sentry state = %v, want Running", st)
	}

	// Complete should not error
	if err := mgr.Complete(result.LeaseID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Double-complete should error
	if err := mgr.Complete(result.LeaseID); err == nil {
		t.Fatal("expected error on double-complete")
	}
	mgr.Shutdown()
}

func TestCapacityExhausted(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	rt := &fakeRuntime{statePID: 1}
	cfg := &config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits}
	mgr := NewManagerForTest(cfg, log, rt, ManagerOpts{})

	p := newPool(
		"python",
		config.Workload{
			PoolSize:   1,
			PreRestore: true,
			MaxPending: 0, // will default to pool_size*2 = 2
			Rootfs:     filepath.Join(root, "rootfs"),
			Checkpoint: filepath.Join(root, "ckpt"),
		},
		cfg, rt, nil, nil, log,
	)

	// One sentry, already busy
	p.sentries = []*Sentry{{
		ID:      "python-1",
		PID:     1,
		RootDir: filepath.Join(root, "python-1", "root"),
		State:   StateRunning,
		Created: time.Now(),
	}}

	// First pending request blocks; start it in goroutine
	errCh1 := make(chan error, 1)
	go func() {
		_, err := p.Execute("", mgr)
		errCh1 <- err
	}()
	time.Sleep(20 * time.Millisecond)

	// Second pending request blocks
	errCh2 := make(chan error, 1)
	go func() {
		_, err := p.Execute("", mgr)
		errCh2 <- err
	}()
	time.Sleep(20 * time.Millisecond)

	// Third should get ErrCapacityExhausted (max_pending defaults to 2)
	errCh3 := make(chan error, 1)
	go func() {
		_, err := p.Execute("", mgr)
		errCh3 <- err
	}()

	select {
	case err := <-errCh3:
		if !errors.Is(err, ErrCapacityExhausted) {
			t.Fatalf("expected ErrCapacityExhausted, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third request did not return ErrCapacityExhausted")
	}

	p.Shutdown()
}

func TestRecycleSendsTermThenKill(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	rt := &fakeRuntime{statePID: 1, warmSentry: true}
	cfg := &config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits}

	p := newPool(
		"python",
		config.Workload{
			PoolSize:   1,
			PreRestore: true,
			Rootfs:     filepath.Join(root, "rootfs"),
			Checkpoint: filepath.Join(root, "ckpt"),
			MaxPending: 1,
		},
		cfg, rt, nil, nil, log,
	)

	s := &Sentry{
		ID:        "python-1",
		PID:       1,
		RootDir:   filepath.Join(root, "python-1", "root"),
		BundleDir: filepath.Join(root, "python-1", "bundle"),
		State:     StateRunning,
		Created:   time.Now(),
		LastReset: time.Now(),
		LastUsed:  time.Now(),
	}
	p.mu.Lock()
	p.sentries = []*Sentry{s}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.recycle(s)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recycle did not complete")
	}

	rt.mu.Lock()
	if len(rt.killCalls) == 0 || rt.killCalls[0] != "TERM" {
		t.Errorf("expected first kill signal to be TERM, got %v", rt.killCalls)
	}
	if rt.resetCalls != 1 {
		t.Errorf("expected 1 reset call, got %d", rt.resetCalls)
	}
	rt.mu.Unlock()
}

func TestRecycleReplacesOnResetFailure(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	rt := &fakeRuntime{statePID: 1, resetErr: fmt.Errorf("reset broken"), warmSentry: true}
	cfg := &config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits}

	p := newPool(
		"python",
		config.Workload{
			PoolSize:   1,
			PreRestore: true,
			Rootfs:     filepath.Join(root, "rootfs"),
			Checkpoint: filepath.Join(root, "ckpt"),
			MaxPending: 1,
		},
		cfg, rt, nil, nil, log,
	)

	s := &Sentry{
		ID:        "python-1",
		PID:       1,
		RootDir:   filepath.Join(root, "python-1", "root"),
		BundleDir: filepath.Join(root, "python-1", "bundle"),
		State:     StateRunning,
		Created:   time.Now(),
		LastReset: time.Now(),
		LastUsed:  time.Now(),
	}
	p.mu.Lock()
	p.sentries = []*Sentry{s}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.recycle(s)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recycle did not complete on reset failure")
	}

	// After reset failure, the old sentry should be removed and a replacement spawned.
	rt.mu.Lock()
	if rt.deleteCalls < 1 {
		t.Error("expected sentry to be deleted after reset failure")
	}
	// The replacement spawn calls create again
	if rt.createCalls < 1 {
		t.Error("expected replacement sentry to be spawned")
	}
	rt.mu.Unlock()
}

func TestConcurrentExecute(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	rt := &fakeRuntime{statePID: 1}
	cfg := &config.Config{Daemon: config.DaemonConfig{Workdir: root, LeaseTimeout: 5 * time.Minute}, Limits: config.DefaultLimits}
	mgr := NewManagerForTest(cfg, log, rt, ManagerOpts{})

	poolSize := 3
	p := newPool(
		"python",
		config.Workload{
			PoolSize:   poolSize,
			PreRestore: true,
			Rootfs:     filepath.Join(root, "rootfs"),
			Checkpoint: filepath.Join(root, "ckpt"),
			MaxPending: 10,
		},
		cfg, rt, nil, nil, log,
	)

	// Seed pool with ready sentries
	for i := 0; i < poolSize; i++ {
		p.sentries = append(p.sentries, &Sentry{
			ID:        fmt.Sprintf("python-%d", i),
			PID:       i + 1,
			RootDir:   filepath.Join(root, fmt.Sprintf("python-%d", i), "root"),
			BundleDir: filepath.Join(root, fmt.Sprintf("python-%d", i), "bundle"),
			State:     StateReady,
			Created:   time.Now(),
			LastReset: time.Now(),
			LastUsed:  time.Now(),
		})
	}

	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := p.Execute("", mgr)
			if err != nil {
				t.Errorf("execute error: %v", err)
				return
			}
			if result.LeaseID == "" {
				t.Error("expected lease_id")
				return
			}
			successes.Add(1)
			_ = mgr.Complete(result.LeaseID)
		}()
	}
	wg.Wait()

	if int(successes.Load()) != poolSize {
		t.Fatalf("expected %d successes, got %d", poolSize, successes.Load())
	}

	p.Shutdown()
}

func TestManagerExecuteAndComplete(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	rt := &fakeRuntime{statePID: 99}
	cfg := &config.Config{
		Daemon: config.DaemonConfig{Workdir: root},
		Limits: config.DefaultLimits,
		Templates: map[string]config.Template{
			"python": {
				Rootfs:     filepath.Join(root, "rootfs"),
				Checkpoint: filepath.Join(root, "ckpt"),
				Pools:      []config.PoolConfig{{ResourceClass: "small", PoolSize: 1, PreRestore: true}},
			},
		},
		ResourceClasses: []config.ResourceClassConfig{{Name: "small", CPUMillis: 500, MemoryMB: 512}},
	}

	mgr := &Manager{
		pools:   make(map[string]*Pool),
		cfg:     cfg,
		runtime: rt,
		log:     log,
		leases:  make(map[string]*lease),
	}

	for name, wl := range cfg.Workloads() {
		mgr.pools[name] = newPool(name, wl, cfg, rt, nil, nil, log)
		// Seed with a ready sentry
		mgr.pools[name].sentries = []*Sentry{{
			ID:        name + "-1",
			PID:       99,
			RootDir:   filepath.Join(root, name+"-1", "root"),
			BundleDir: filepath.Join(root, name+"-1", "bundle"),
			State:     StateReady,
			Created:   time.Now(),
			LastReset: time.Now(),
			LastUsed:  time.Now(),
		}}
	}

	result, err := mgr.Execute("python/small", "")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.LeaseID == "" {
		t.Fatal("expected lease_id")
	}

	status := mgr.Status()
	if status.ActiveLeases != 1 {
		t.Fatalf("active_leases = %d, want 1", status.ActiveLeases)
	}

	if err := mgr.Complete(result.LeaseID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Unknown workload returns error
	_, err = mgr.Execute("ruby", "")
	if err == nil {
		t.Fatal("expected error for unknown workload")
	}
	mgr.Shutdown()
}
