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

// Package pool manages partitioned pools of warm gVisor sentry processes.
// Each workload type gets its own independent pool with dedicated sentries.
package pool

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shayonj/gvisord/internal/cgroup"
	"github.com/shayonj/gvisord/internal/cni"
	"github.com/shayonj/gvisord/internal/config"
	"github.com/shayonj/gvisord/internal/runsc"
)

// lease tracks an acquired sentry that is awaiting completion.
type lease struct {
	pool    *Pool
	sentry  *Sentry
	created time.Time
	timer   *time.Timer
}

// Manager holds partitioned pools keyed by workload name.
type Manager struct {
	pools        map[string]*Pool
	cfg          *config.Config
	runtime      runsc.Runtime
	cgroupMgr    *cgroup.Manager
	cniMgr       *cni.Manager
	log          *slog.Logger
	shutdownOnce sync.Once

	leaseMu sync.Mutex
	leases  map[string]*lease
}

// ManagerOpts holds optional dependencies for Manager.
type ManagerOpts struct {
	CgroupMgr *cgroup.Manager
	CNIMgr    *cni.Manager
}

// NewManager creates a new pool manager with one pool per workload.
func NewManager(cfg *config.Config, log *slog.Logger, opts ManagerOpts) *Manager {
	runtime := runsc.NewClient(cfg.Runsc.Path, cfg.Runsc.ExtraFlags, log)
	return &Manager{
		pools:     make(map[string]*Pool),
		cfg:       cfg,
		runtime:   runtime,
		cgroupMgr: opts.CgroupMgr,
		cniMgr:    opts.CNIMgr,
		log:       log,
		leases:    make(map[string]*lease),
	}
}

// NewManagerForTest creates a Manager with a custom runtime for testing.
func NewManagerForTest(cfg *config.Config, log *slog.Logger, rt runsc.Runtime, opts ManagerOpts) *Manager {
	return &Manager{
		pools:     make(map[string]*Pool),
		cfg:       cfg,
		runtime:   rt,
		cgroupMgr: opts.CgroupMgr,
		cniMgr:    opts.CNIMgr,
		log:       log,
		leases:    make(map[string]*lease),
	}
}

// RegisterPool adds a pool to the manager, primarily for testing.
func (m *Manager) RegisterPool(name string, p *Pool) {
	m.pools[name] = p
}

// Start pre-warms all workload pools.
func (m *Manager) Start() error {
	for name, wl := range m.cfg.Workloads() {
		p := newPool(name, wl, m.cfg, m.runtime, m.cgroupMgr, m.cniMgr, m.log)
		if err := p.Start(); err != nil {
			return fmt.Errorf("starting pool %q: %w", name, err)
		}
		m.pools[name] = p
	}
	return nil
}

// Shutdown drains all pools and cancels outstanding leases.
func (m *Manager) Shutdown() {
	m.shutdownOnce.Do(func() {
		m.leaseMu.Lock()
		for id, l := range m.leases {
			l.timer.Stop()
			delete(m.leases, id)
		}
		m.leaseMu.Unlock()

		for name, p := range m.pools {
			m.log.Info("shutting down pool", "workload", name)
			p.Shutdown()
		}
	})
}

// Execute dispatches to the named workload pool and returns a lease.
// The sentry remains acquired until Complete is called with the lease ID.
func (m *Manager) Execute(workload, checkpoint string) (*ExecuteResult, error) {
	p, ok := m.pools[workload]
	if !ok {
		return nil, fmt.Errorf("unknown workload %q (available: %v)", workload, m.workloadNames())
	}
	return p.Execute(checkpoint, m)
}

// Complete releases a lease and triggers sentry recycling.
func (m *Manager) Complete(leaseID string) error {
	m.leaseMu.Lock()
	l, ok := m.leases[leaseID]
	if !ok {
		m.leaseMu.Unlock()
		return fmt.Errorf("unknown lease %q", leaseID)
	}
	l.timer.Stop()
	delete(m.leases, leaseID)
	m.leaseMu.Unlock()

	l.pool.recycleWg.Add(1)
	go func() {
		defer l.pool.recycleWg.Done()
		l.pool.recycle(l.sentry)
	}()
	return nil
}

func (m *Manager) registerLease(p *Pool, s *Sentry) string {
	id := generateLeaseID()

	s.mu.Lock()
	s.LeaseID = id
	s.mu.Unlock()

	timer := time.AfterFunc(m.cfg.Daemon.LeaseTimeout, func() {
		m.log.Warn("lease auto-expired", "lease_id", id, "sentry_id", s.ID)
		_ = m.Complete(id)
	})

	m.leaseMu.Lock()
	m.leases[id] = &lease{pool: p, sentry: s, created: time.Now(), timer: timer}
	m.leaseMu.Unlock()
	return id
}

// Status returns aggregate status across all pools.
func (m *Manager) Status() ManagerStatus {
	var pools []PoolStatus
	for _, p := range m.pools {
		pools = append(pools, p.Status())
	}
	m.leaseMu.Lock()
	activeLeases := len(m.leases)
	m.leaseMu.Unlock()
	return ManagerStatus{Pools: pools, ActiveLeases: activeLeases}
}

// Healthy returns true if at least one pool has a ready sentry.
func (m *Manager) Healthy() bool {
	for _, p := range m.pools {
		if p.Healthy() {
			return true
		}
	}
	return false
}

func (m *Manager) workloadNames() []string {
	names := make([]string, 0, len(m.pools))
	for name := range m.pools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ManagerStatus is the aggregate status returned by the API.
type ManagerStatus struct {
	Pools        []PoolStatus `json:"pools"`
	ActiveLeases int          `json:"active_leases"`
}

// Pool manages sentries for a single workload type.
type Pool struct {
	name     string
	workload config.Workload
	cfg      *config.Config
	runtime  runsc.Runtime
	cgroupMgr *cgroup.Manager
	cniMgr   *cni.Manager
	log      *slog.Logger

	mu       sync.Mutex
	cond     *sync.Cond
	sentries []*Sentry
	pending  int
	nextID   atomic.Int64
	stopCh   chan struct{}
	closed   bool

	TotalExecutions atomic.Int64
	TotalRestoreNs  atomic.Int64
	TotalAcquireNs  atomic.Int64
	recycleWg       sync.WaitGroup
	shutdownOnce    sync.Once
}

func newPool(name string, wl config.Workload, cfg *config.Config, runtime runsc.Runtime, cgMgr *cgroup.Manager, cniMgr *cni.Manager, log *slog.Logger) *Pool {
	p := &Pool{
		name:      name,
		workload:  wl,
		cfg:       cfg,
		runtime:   runtime,
		cgroupMgr: cgMgr,
		cniMgr:    cniMgr,
		log:       log.With("workload", name),
		stopCh:    make(chan struct{}),
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *Pool) Start() error {
	p.log.Info("starting pool", "pool_size", p.workload.PoolSize, "pre_restore", p.workload.PreRestore, "rootfs", p.workload.Rootfs, "checkpoint", p.workload.Checkpoint)
	for i := 0; i < p.workload.PoolSize; i++ {
		s, err := p.spawn()
		if err != nil {
			return fmt.Errorf("spawning sentry %d: %w", i, err)
		}
		p.mu.Lock()
		p.sentries = append(p.sentries, s)
		p.mu.Unlock()
		p.log.Info("sentry ready", "id", s.ID, "pid", s.PID, "ip", s.IP, "pre_restored", p.workload.PreRestore)
	}
	if p.cfg.Limits.IdleTimeout > 0 {
		go p.reaper()
	}
	return nil
}

func (p *Pool) Shutdown() {
	p.shutdownOnce.Do(func() {
		close(p.stopCh)

		// Wait for in-flight recycles to finish before cleaning up.
		p.recycleWg.Wait()

		p.mu.Lock()
		p.closed = true
		sentries := make([]*Sentry, len(p.sentries))
		copy(sentries, p.sentries)
		p.sentries = nil
		p.cond.Broadcast()
		p.mu.Unlock()

		for _, s := range sentries {
			p.log.Info("killing sentry", "id", s.ID, "pid", s.PID)
			p.cleanup(s)
		}
	})
}

// Execute acquires a ready sentry, optionally restores from a checkpoint,
// registers a lease, and returns the result. The sentry stays acquired
// until the lease is completed via Manager.Complete.
func (p *Pool) Execute(checkpoint string, mgr *Manager) (*ExecuteResult, error) {
	acquireStart := time.Now()
	s, err := p.acquire()
	if err != nil {
		return nil, err
	}
	acquireLatency := time.Since(acquireStart)
	p.TotalAcquireNs.Add(acquireLatency.Nanoseconds())

	ckpt := p.workload.Checkpoint
	if checkpoint != "" {
		if p.workload.PreRestore {
			p.release(s)
			return nil, fmt.Errorf("checkpoint override not supported with pre_restore=true (sentry is already restored from %q)", p.workload.Checkpoint)
		}
		if err := p.cfg.ValidateCheckpointPath(checkpoint); err != nil {
			p.release(s)
			return nil, err
		}
		ckpt = checkpoint
	}

	var restoreLatency time.Duration
	if !p.workload.PreRestore {
		p.runtime.CleanFilestores(p.workload.Rootfs)
		start := time.Now()
		if err := p.runtime.Restore(s.RootDir, s.BundleDir, ckpt, s.ID); err != nil {
			p.release(s)
			return nil, fmt.Errorf("restore failed: %w", err)
		}
		restoreLatency = time.Since(start)
		p.TotalRestoreNs.Add(restoreLatency.Nanoseconds())
	}
	p.TotalExecutions.Add(1)

	leaseID := mgr.registerLease(p, s)

	s.mu.Lock()
	restoreNum := s.Restores + 1
	s.mu.Unlock()

	result := &ExecuteResult{
		Workload:   p.name,
		SentryID:   s.ID,
		LeaseID:    leaseID,
		PID:        s.PID,
		IP:         s.IP,
		Checkpoint: ckpt,
		AcquireMs:  float64(acquireLatency.Microseconds()) / 1000.0,
		RestoreMs:  float64(restoreLatency.Microseconds()) / 1000.0,
		RestoreNum: restoreNum,
	}

	return result, nil
}

func (p *Pool) Status() PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	var infos []SentryInfo
	ready, running := 0, 0
	for _, s := range p.sentries {
		info := s.Info()
		infos = append(infos, info)
		switch info.State {
		case "ready":
			ready++
		case "running":
			running++
		}
	}

	total := p.TotalExecutions.Load()
	avgAcquireMs := float64(0)
	avgRestoreMs := float64(0)
	if total > 0 {
		avgAcquireMs = float64(p.TotalAcquireNs.Load()) / float64(total) / 1e6
		avgRestoreMs = float64(p.TotalRestoreNs.Load()) / float64(total) / 1e6
	}

	maxPending := p.workload.MaxPending
	if maxPending <= 0 {
		maxPending = p.workload.PoolSize * 2
	}

	return PoolStatus{
		Workload:        p.name,
		PoolSize:        p.workload.PoolSize,
		MaxPending:      maxPending,
		Ready:           ready,
		Running:         running,
		Pending:         p.pending,
		Checkpoint:      p.workload.Checkpoint,
		Rootfs:          p.workload.Rootfs,
		PreRestore:      p.workload.PreRestore,
		TotalExecutions: total,
		AvgAcquireMs:    avgAcquireMs,
		AvgRestoreMs:    avgRestoreMs,
		Sentries:        infos,
	}
}

func (p *Pool) Healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sentries {
		s.mu.Lock()
		st := s.State
		s.mu.Unlock()
		if st == StateReady {
			return true
		}
	}
	return false
}

func (p *Pool) acquire() (*Sentry, error) {
	p.mu.Lock()

	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}

	if s := p.findReadyLocked(); s != nil {
		p.mu.Unlock()
		return s, nil
	}

	// Pool is empty (idle eviction drained it). Spawn synchronously.
	// Concurrent callers may each trigger a spawn; extras become pool members.
	if len(p.sentries) == 0 {
		p.pending++
		p.mu.Unlock()
		p.log.Info("pool empty, spawning sentry on demand (cold path)")
		s, err := p.spawn()
		p.mu.Lock()
		p.pending--
		if err != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("on-demand spawn failed: %w", err)
		}
		if p.closed {
			p.mu.Unlock()
			p.cleanup(s)
			return nil, ErrPoolClosed
		}
		s.mu.Lock()
		s.State = StateRunning
		s.LastUsed = time.Now()
		s.mu.Unlock()
		p.sentries = append(p.sentries, s)
		p.mu.Unlock()
		return s, nil
	}

	maxPending := p.workload.MaxPending
	if maxPending <= 0 {
		maxPending = p.workload.PoolSize * 2
	}
	if p.pending >= maxPending {
		p.mu.Unlock()
		return nil, ErrCapacityExhausted
	}

	p.pending++
	p.log.Debug("execute queued", "pending", p.pending, "max", maxPending)
	for {
		p.cond.Wait()
		if p.closed {
			p.pending--
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}
		if s := p.findReadyLocked(); s != nil {
			p.pending--
			p.mu.Unlock()
			return s, nil
		}
	}
}

func (p *Pool) findReadyLocked() *Sentry {
	for _, s := range p.sentries {
		s.mu.Lock()
		if s.State == StateReady {
			s.State = StateRunning
			s.LastUsed = time.Now()
			s.mu.Unlock()
			return s
		}
		s.mu.Unlock()
	}
	return nil
}

func (p *Pool) release(s *Sentry) {
	s.mu.Lock()
	s.State = StateReady
	s.LeaseID = ""
	s.mu.Unlock()

	p.mu.Lock()
	p.cond.Signal()
	p.mu.Unlock()
}

func (p *Pool) recycle(s *Sentry) {
	s.mu.Lock()
	s.State = StateDraining
	s.LeaseID = ""
	s.mu.Unlock()

	// Send SIGTERM first for graceful shutdown, then SIGKILL if still alive.
	_ = p.runtime.Kill(s.RootDir, s.ID, "TERM")

	waitDone := make(chan struct{})
	go func() {
		_, _ = p.runtime.Wait(s.RootDir, s.ID)
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		_ = p.runtime.Kill(s.RootDir, s.ID, "KILL")
		<-waitDone
	}

	if p.runtime.WarmSentry() {
		// Warm sentry mode: reset the kernel in place and restore.
		if err := p.runtime.Reset(s.RootDir, s.ID); err != nil {
			p.log.Warn("reset failed, replacing sentry", "id", s.ID, "err", err)
			p.remove(s)
			p.cleanup(s)
			p.refill()
			return
		}
		s.mu.Lock()
		s.Restores++
		s.LastReset = time.Now()
		s.mu.Unlock()

		lim := p.cfg.Limits
		if err := s.HealthCheck(lim.MaxRestoresPerSentry, lim.MaxSentryAge, lim.MaxRSSGrowthKB, lim.MaxOpenFDs); err != nil {
			p.log.Warn("sentry failed health check, replacing", "id", s.ID, "err", err)
			p.remove(s)
			p.cleanup(s)
			p.refill()
			return
		}

		p.runtime.CleanFilestores(p.workload.Rootfs)
		restoreStart := time.Now()
		if err := p.runtime.Restore(s.RootDir, s.BundleDir, p.workload.Checkpoint, s.ID); err != nil {
			p.log.Warn("restore failed during recycle, replacing", "id", s.ID, "err", err)
			p.remove(s)
			p.cleanup(s)
			p.refill()
			return
		}
		p.TotalRestoreNs.Add(time.Since(restoreStart).Nanoseconds())

		s.mu.Lock()
		s.State = StateReady
		restores := s.Restores
		s.mu.Unlock()

		p.mu.Lock()
		p.cond.Signal()
		p.mu.Unlock()
		p.log.Debug("sentry recycled (warm)", "id", s.ID, "restores", restores)
	} else {
		// Stock runsc: destroy the container and spawn a fresh one.
		p.remove(s)
		p.cleanup(s)
		p.refill()
	}
}

func (p *Pool) spawn() (*Sentry, error) {
	safeName := strings.ReplaceAll(p.name, "/", "-")
	id := fmt.Sprintf("%s-%d", safeName, p.nextID.Add(1))
	sentryDir := filepath.Join(p.cfg.Daemon.Workdir, id)
	rootDir := filepath.Join(sentryDir, "root")
	bundleDir := filepath.Join(sentryDir, "bundle")

	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("creating root dir: %w", err)
	}
	if err := p.runtime.PrepareBundle(bundleDir, p.workload.Rootfs, p.workload.Checkpoint); err != nil {
		return nil, fmt.Errorf("preparing bundle: %w", err)
	}
	if err := runsc.InjectBundleMounts(bundleDir, p.cfg.Daemon.HarnessPath, p.cfg.Daemon.CacheDir); err != nil {
		os.RemoveAll(sentryDir)
		return nil, fmt.Errorf("injecting bundle mounts: %w", err)
	}
	p.runtime.CleanFilestores(p.workload.Rootfs)

	s := &Sentry{
		ID:            id,
		RootDir:       rootDir,
		BundleDir:     bundleDir,
		State:         StateReady,
		ResourceClass: p.workload.ResourceClass,
	}

	if p.cniMgr != nil {
		ip, netnsPath, err := p.cniMgr.CreateNetNS(id)
		if err != nil {
			os.RemoveAll(sentryDir)
			return nil, fmt.Errorf("creating network namespace: %w", err)
		}
		s.IP = ip
		s.NetnsPath = netnsPath
	}

	if s.NetnsPath != "" {
		if err := runsc.InjectNetNS(bundleDir, s.NetnsPath); err != nil {
			p.cleanupPartial(s)
			return nil, fmt.Errorf("injecting network namespace: %w", err)
		}
	}

	if p.runtime.WarmSentry() {
		// Warm sentry: create persists the sentry process, restore loads
		// the checkpoint into it. Two-step, requires --warm-sentry patch.
		if err := p.runtime.Create(rootDir, bundleDir, id); err != nil {
			p.cleanupPartial(s)
			return nil, fmt.Errorf("runsc create: %w", err)
		}
	}

	// Restore from checkpoint. With warm sentry this loads into the
	// existing sentry process. With stock runsc this creates a new
	// container from the checkpoint in one shot.
	if p.workload.PreRestore || !p.runtime.WarmSentry() {
		if err := p.runtime.Restore(rootDir, bundleDir, p.workload.Checkpoint, id); err != nil {
			p.cleanupPartial(s)
			return nil, fmt.Errorf("restore: %w", err)
		}
	}

	state, err := p.runtime.State(rootDir, id)
	if err != nil {
		p.cleanupPartial(s)
		return nil, fmt.Errorf("reading state: %w", err)
	}

	now := time.Now()
	s.PID = state.PID
	s.Created = now
	s.LastReset = now
	s.LastUsed = now
	s.BaseRSSKB = ReadRSSKB(state.PID)

	if p.cgroupMgr != nil && p.workload.ResourceClass != "" {
		cgPath, err := p.cgroupMgr.SentrySlicePath(p.workload.ResourceClass, id)
		if err != nil {
			p.cleanupPartial(s)
			return nil, fmt.Errorf("creating cgroup for sentry: %w", err)
		}
		if err := p.cgroupMgr.PlacePID(cgPath, state.PID); err != nil {
			p.cleanupPartial(s)
			return nil, fmt.Errorf("placing sentry in cgroup: %w", err)
		}
	}

	return s, nil
}

func (p *Pool) remove(s *Sentry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.sentries {
		if existing.ID == s.ID {
			p.sentries = append(p.sentries[:i], p.sentries[i+1:]...)
			return
		}
	}
}

// cleanupPartial removes resources for a sentry that failed partway through spawn.
func (p *Pool) cleanupPartial(s *Sentry) {
	_ = p.runtime.Kill(s.RootDir, s.ID, "KILL")
	_ = p.runtime.Delete(s.RootDir, s.ID)
	p.runtime.KillProcess(s.PID)
	os.RemoveAll(filepath.Dir(s.RootDir))
}

func (p *Pool) cleanup(s *Sentry) {
	_ = p.runtime.Kill(s.RootDir, s.ID, "KILL")
	if err := p.runtime.Delete(s.RootDir, s.ID); err != nil {
		p.log.Debug("delete failed during cleanup", "id", s.ID, "err", err)
	}
	p.runtime.KillProcess(s.PID)

	if p.cniMgr != nil {
		p.cniMgr.DeleteNetNS(s.ID)
	}
	if p.cgroupMgr != nil && s.ResourceClass != "" {
		p.cgroupMgr.CleanupSentry(s.ResourceClass, s.ID)
	}

	os.RemoveAll(filepath.Dir(s.RootDir))
}

func (p *Pool) refill() {
	p.mu.Lock()
	needed := p.workload.PoolSize - len(p.sentries)
	p.mu.Unlock()

	for i := 0; i < needed; i++ {
		s, err := p.spawn()
		if err != nil {
			p.log.Error("failed to spawn replacement", "err", err)
			continue
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			p.cleanup(s)
			return
		}
		p.sentries = append(p.sentries, s)
		p.cond.Signal()
		p.mu.Unlock()
		p.log.Info("replacement sentry ready", "id", s.ID, "pid", s.PID)
	}
}

// reaper periodically evicts idle sentries. The pool can shrink to zero.
// New sentries are spawned on demand when Execute is called on an empty pool.
func (p *Pool) reaper() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
		}

		idleTimeout := p.cfg.Limits.IdleTimeout
		if idleTimeout <= 0 {
			continue
		}

		var evict []*Sentry
		p.mu.Lock()
		remaining := make([]*Sentry, 0, len(p.sentries))
		for _, s := range p.sentries {
			s.mu.Lock()
			idle := s.State == StateReady && time.Since(s.LastUsed) > idleTimeout
			s.mu.Unlock()
			if idle {
				evict = append(evict, s)
			} else {
				remaining = append(remaining, s)
			}
		}
		p.sentries = remaining
		p.mu.Unlock()

		for _, s := range evict {
			p.log.Info("evicting idle sentry", "id", s.ID, "pid", s.PID)
			p.cleanup(s)
		}
	}
}

// RunParams is the input for a single run-and-return-result call.
type RunParams struct {
	Workload    string            `json:"workload"`
	Script      string            `json:"script,omitempty"`
	Interpreter string            `json:"interpreter,omitempty"`
	Command     string            `json:"command,omitempty"`
	Deps        []string          `json:"deps,omitempty"`
	Event       json.RawMessage   `json:"event,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Timeout     int               `json:"timeout,omitempty"`
}

// RunResult is the combined result from acquiring a sentry, running code, and releasing.
type RunResult struct {
	Type      string  `json:"type"`
	ExitCode  int     `json:"exit_code"`
	Stdout    string  `json:"stdout"`
	Stderr    string  `json:"stderr"`
	ElapsedMs float64 `json:"elapsed_ms"`
	Error     string  `json:"error,omitempty"`
	SentryID  string  `json:"sentry_id"`
	AcquireMs float64 `json:"acquire_ms"`
}

// Run acquires a sentry, sends the script to the harness over HTTP,
// collects the result, and recycles the sentry. The full lease cycle
// is handled internally.
func (m *Manager) Run(params RunParams) (*RunResult, error) {
	execResult, err := m.Execute(params.Workload, "")
	if err != nil {
		return nil, err
	}

	// Look up the sentry for exec fallback info.
	m.leaseMu.Lock()
	l, ok := m.leases[execResult.LeaseID]
	var sentryRootDir, sentryID string
	if ok {
		sentryRootDir = l.sentry.RootDir
		sentryID = l.sentry.ID
	}
	m.leaseMu.Unlock()

	harnessResult, harnessErr := m.callHarness(execResult.IP, sentryRootDir, sentryID, params)

	if err := m.Complete(execResult.LeaseID); err != nil {
		m.log.Warn("failed to complete lease after run", "lease_id", execResult.LeaseID, "err", err)
	}

	if harnessErr != nil {
		return nil, fmt.Errorf("harness call failed: %w", harnessErr)
	}

	harnessResult.SentryID = execResult.SentryID
	harnessResult.AcquireMs = execResult.AcquireMs
	return harnessResult, nil
}

func (m *Manager) callHarness(ip, sentryRootDir, sentryID string, params RunParams) (*RunResult, error) {
	if ip == "" {
		return m.callHarnessViaExec(sentryRootDir, sentryID, params)
	}

	timeout := time.Duration(params.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Add buffer for HTTP overhead beyond the script timeout.
	httpTimeout := timeout + 5*time.Second

	body := map[string]any{
		"script":      params.Script,
		"interpreter": params.Interpreter,
		"command":     params.Command,
		"deps":        params.Deps,
		"event":       params.Event,
		"env":         params.Env,
		"timeout":     params.Timeout,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling harness request: %w", err)
	}

	url := fmt.Sprintf("http://%s:8080/run", ip)
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Post(url, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	if err != nil {
		return nil, fmt.Errorf("reading harness response: %w", err)
	}

	var result RunResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing harness response: %w", err)
	}
	return &result, nil
}

// callHarnessViaExec falls back to `runsc exec` when the sentry has no IP
// (network.mode = "none"). It builds a shell command that invokes the
// interpreter directly inside the sandbox.
func (m *Manager) callHarnessViaExec(rootDir, id string, params RunParams) (*RunResult, error) {
	if rootDir == "" || id == "" {
		return nil, fmt.Errorf("sentry info not available for exec fallback")
	}

	var args []string
	if params.Command != "" {
		args = []string{"/bin/sh", "-c", params.Command}
	} else {
		interp := params.Interpreter
		if interp == "" {
			interp = "python3"
		}
		args = []string{interp, "-c", params.Script}
	}

	start := time.Now()
	out, err := m.runtime.Exec(rootDir, id, args)
	elapsed := float64(time.Since(start).Microseconds()) / 1000.0

	if err != nil {
		return &RunResult{
			Type:      "error",
			ExitCode:  -1,
			Stdout:    string(out),
			Error:     err.Error(),
			ElapsedMs: elapsed,
		}, nil
	}

	return &RunResult{
		Type:      "success",
		ExitCode:  0,
		Stdout:    string(out),
		ElapsedMs: elapsed,
	}, nil
}

// --- Result / Status types ---

type ExecuteResult struct {
	Workload   string  `json:"workload"`
	SentryID   string  `json:"sentry_id"`
	LeaseID    string  `json:"lease_id"`
	PID        int     `json:"pid"`
	IP         string  `json:"ip,omitempty"`
	Checkpoint string  `json:"checkpoint"`
	AcquireMs  float64 `json:"acquire_ms"`
	RestoreMs  float64 `json:"restore_ms"`
	RestoreNum int     `json:"restore_num"`
}

type PoolStatus struct {
	Workload        string       `json:"workload"`
	PoolSize        int          `json:"pool_size"`
	MaxPending      int          `json:"max_pending"`
	Ready           int          `json:"ready"`
	Running         int          `json:"running"`
	Pending         int          `json:"pending"`
	Checkpoint      string       `json:"checkpoint"`
	Rootfs          string       `json:"rootfs"`
	PreRestore      bool         `json:"pre_restore"`
	TotalExecutions int64        `json:"total_executions"`
	AvgAcquireMs    float64      `json:"avg_acquire_ms"`
	AvgRestoreMs    float64      `json:"avg_restore_ms"`
	Sentries        []SentryInfo `json:"sentries"`
}

func generateLeaseID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
