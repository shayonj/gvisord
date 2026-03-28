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

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the top-level gvisord configuration.
type Config struct {
	Daemon          DaemonConfig          `json:"daemon"`
	Runsc           RunscConfig           `json:"runsc"`
	Network         NetworkConfig         `json:"network"`
	Cgroup          CgroupConfig          `json:"cgroup"`
	ResourceClasses []ResourceClassConfig `json:"resource_classes"`
	Limits          Limits                `json:"limits"`
	Templates       map[string]Template   `json:"templates"`
}

// DaemonConfig controls gvisord's own process and API.
type DaemonConfig struct {
	Socket         string        `json:"socket"`
	SocketMode     string        `json:"socket_mode"`
	SocketGroup    string        `json:"socket_group"`
	PIDFile        string        `json:"pid_file"`
	Workdir        string        `json:"workdir"`
	HarnessPath    string        `json:"harness_path"`
	CacheDir       string        `json:"cache_dir"`
	LeaseTimeout   time.Duration `json:"lease_timeout"`
	CheckpointDirs []string      `json:"checkpoint_dirs"`
}

func (d *DaemonConfig) UnmarshalJSON(data []byte) error {
	type raw struct {
		Socket         *string  `json:"socket,omitempty"`
		SocketMode     *string  `json:"socket_mode,omitempty"`
		SocketGroup    *string  `json:"socket_group,omitempty"`
		PIDFile        *string  `json:"pid_file,omitempty"`
		Workdir        *string  `json:"workdir,omitempty"`
		HarnessPath    *string  `json:"harness_path,omitempty"`
		CacheDir       *string  `json:"cache_dir,omitempty"`
		LeaseTimeout   *string  `json:"lease_timeout,omitempty"`
		CheckpointDirs []string `json:"checkpoint_dirs,omitempty"`
	}
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.Socket != nil {
		d.Socket = *r.Socket
	}
	if r.SocketMode != nil {
		d.SocketMode = *r.SocketMode
	}
	if r.SocketGroup != nil {
		d.SocketGroup = *r.SocketGroup
	}
	if r.PIDFile != nil {
		d.PIDFile = *r.PIDFile
	}
	if r.Workdir != nil {
		d.Workdir = *r.Workdir
	}
	if r.HarnessPath != nil {
		d.HarnessPath = *r.HarnessPath
	}
	if r.CacheDir != nil {
		d.CacheDir = *r.CacheDir
	}
	if r.LeaseTimeout != nil {
		dur, err := time.ParseDuration(*r.LeaseTimeout)
		if err != nil {
			return fmt.Errorf("parsing lease_timeout: %w", err)
		}
		d.LeaseTimeout = dur
	}
	if r.CheckpointDirs != nil {
		d.CheckpointDirs = r.CheckpointDirs
	}
	return nil
}

// RunscConfig controls the runsc binary.
type RunscConfig struct {
	Path       string   `json:"path"`
	ExtraFlags []string `json:"extra_flags"`
}

// NetworkConfig controls CNI bridge networking for sentries.
type NetworkConfig struct {
	Mode      string `json:"mode"`
	PluginDir string `json:"plugin_dir"`
	ConfigDir string `json:"config_dir"`
	Bridge    string `json:"bridge"`
	Subnet    string `json:"subnet"`
}

// CgroupConfig controls cgroup resource isolation.
type CgroupConfig struct {
	BasePath string `json:"base_path"`
	Prefix   string `json:"prefix"`
}

// ResourceClassConfig defines CPU and memory limits for a resource class.
type ResourceClassConfig struct {
	Name      string `json:"name"`
	CPUMillis int    `json:"cpu_millis"`
	MemoryMB  int    `json:"memory_mb"`
}

// Template defines a workload template: a rootfs + checkpoint pair that
// sentries are restored from. Each template can have pools across
// multiple resource classes.
type Template struct {
	Rootfs     string       `json:"rootfs"`
	Checkpoint string       `json:"checkpoint"`
	Pools      []PoolConfig `json:"pools"`
}

// PoolConfig defines the warm sentry pool for a template + resource class.
type PoolConfig struct {
	ResourceClass string `json:"resource_class"`
	PoolSize      int    `json:"pool_size"`
	MaxPending    int    `json:"max_pending"`
	PreRestore    bool   `json:"pre_restore"`
}

func (p *PoolConfig) UnmarshalJSON(data []byte) error {
	type raw struct {
		ResourceClass string `json:"resource_class"`
		PoolSize      int    `json:"pool_size"`
		MaxPending    int    `json:"max_pending"`
		PreRestore    *bool  `json:"pre_restore"`
	}
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	p.ResourceClass = r.ResourceClass
	p.PoolSize = r.PoolSize
	p.MaxPending = r.MaxPending
	p.PreRestore = true
	if r.PreRestore != nil {
		p.PreRestore = *r.PreRestore
	}
	return nil
}

// EffectiveMaxPending returns max_pending or a sensible default.
func (p PoolConfig) EffectiveMaxPending() int {
	if p.MaxPending > 0 {
		return p.MaxPending
	}
	if p.PoolSize > 0 {
		return p.PoolSize * 2
	}
	return 2
}

// Limits controls sentry health and eviction.
type Limits struct {
	MaxRestoresPerSentry int           `json:"max_restores_per_sentry"`
	MaxSentryAge         time.Duration `json:"max_sentry_age"`
	MaxRSSGrowthKB       int64         `json:"max_rss_growth_kb"`
	MaxOpenFDs           int           `json:"max_open_fds"`
	IdleTimeout          time.Duration `json:"idle_timeout"`
}

func (l *Limits) UnmarshalJSON(data []byte) error {
	type raw struct {
		MaxRestoresPerSentry *int    `json:"max_restores_per_sentry,omitempty"`
		MaxSentryAge         *string `json:"max_sentry_age,omitempty"`
		MaxRSSGrowthKB       *int64  `json:"max_rss_growth_kb,omitempty"`
		MaxOpenFDs           *int    `json:"max_open_fds,omitempty"`
		IdleTimeout          *string `json:"idle_timeout,omitempty"`
	}
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.MaxRestoresPerSentry != nil {
		l.MaxRestoresPerSentry = *r.MaxRestoresPerSentry
	}
	if r.MaxRSSGrowthKB != nil {
		l.MaxRSSGrowthKB = *r.MaxRSSGrowthKB
	}
	if r.MaxOpenFDs != nil {
		l.MaxOpenFDs = *r.MaxOpenFDs
	}
	if r.MaxSentryAge != nil {
		d, err := time.ParseDuration(*r.MaxSentryAge)
		if err != nil {
			return fmt.Errorf("parsing max_sentry_age: %w", err)
		}
		l.MaxSentryAge = d
	}
	if r.IdleTimeout != nil {
		d, err := time.ParseDuration(*r.IdleTimeout)
		if err != nil {
			return fmt.Errorf("parsing idle_timeout: %w", err)
		}
		l.IdleTimeout = d
	}
	return nil
}

var DefaultLimits = Limits{
	MaxRestoresPerSentry: 100,
	MaxSentryAge:         10 * time.Minute,
	MaxRSSGrowthKB:       200 * 1024,
	MaxOpenFDs:           512,
	IdleTimeout:          5 * time.Minute,
}

// Load reads a JSON config file with sensible defaults.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("config file path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg := &Config{
		Daemon: DaemonConfig{
			Socket:       "/run/gvisord/gvisord.sock",
			SocketMode:   "0600",
			PIDFile:      "/run/gvisord/gvisord.pid",
			Workdir:      "/run/gvisord/sentries",
			HarnessPath:  "/var/gvisord/harness/gvisord-exec",
			CacheDir:     "/var/cache/gvisord",
			LeaseTimeout: 5 * time.Minute,
		},
		Runsc: RunscConfig{
			Path:       "/usr/local/bin/runsc",
			ExtraFlags: []string{},
		},
		Network: NetworkConfig{
			Mode:      "cni",
			PluginDir: "/opt/cni/bin",
			ConfigDir: "/etc/cni/net.d",
			Bridge:    "gvisord0",
			Subnet:    "10.88.0.0/16",
		},
		Cgroup: CgroupConfig{
			BasePath: "/sys/fs/cgroup",
			Prefix:   "gvisord",
		},
		ResourceClasses: []ResourceClassConfig{
			{Name: "small", CPUMillis: 500, MemoryMB: 512},
			{Name: "medium", CPUMillis: 1000, MemoryMB: 1024},
			{Name: "large", CPUMillis: 2000, MemoryMB: 4096},
		},
		Limits: DefaultLimits,
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return cfg, nil
}

// Validate checks the configuration for required fields and consistency.
func (c *Config) Validate() error {
	if c.Runsc.Path == "" {
		return fmt.Errorf("runsc.path is required")
	}
	if len(c.Templates) == 0 {
		return fmt.Errorf("at least one template must be defined")
	}
	for name, tmpl := range c.Templates {
		if tmpl.Rootfs == "" {
			return fmt.Errorf("template %q: rootfs is required", name)
		}
		if tmpl.Checkpoint == "" {
			return fmt.Errorf("template %q: checkpoint is required", name)
		}
		if len(tmpl.Pools) == 0 {
			return fmt.Errorf("template %q: at least one pool is required", name)
		}
		for _, pool := range tmpl.Pools {
			if pool.PoolSize < 1 {
				return fmt.Errorf("template %q pool %q: pool_size must be >= 1", name, pool.ResourceClass)
			}
			if pool.ResourceClass == "" {
				return fmt.Errorf("template %q: pool resource_class is required", name)
			}
			if !c.hasResourceClass(pool.ResourceClass) {
				return fmt.Errorf("template %q: unknown resource_class %q", name, pool.ResourceClass)
			}
		}
	}
	if c.Network.Mode == "cni" {
		if c.Network.PluginDir == "" {
			return fmt.Errorf("network.plugin_dir is required when mode=cni")
		}
		if c.Network.Subnet == "" {
			return fmt.Errorf("network.subnet is required when mode=cni")
		}
	}
	return nil
}

// ValidateCheckpointPath checks whether a checkpoint path is allowed by the
// configured checkpoint_dirs allowlist. If checkpoint_dirs is empty, all
// paths are allowed (backward compatible).
func (c *Config) ValidateCheckpointPath(path string) error {
	if len(c.Daemon.CheckpointDirs) == 0 {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolving checkpoint path: %w", err)
	}
	for _, dir := range c.Daemon.CheckpointDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if strings.HasPrefix(abs, absDir+"/") || abs == absDir {
			return nil
		}
	}
	return fmt.Errorf("checkpoint path %q is not under any allowed checkpoint_dirs", path)
}

func (c *Config) hasResourceClass(name string) bool {
	for _, rc := range c.ResourceClasses {
		if rc.Name == name {
			return true
		}
	}
	return false
}

// Workload is used by the pool package to describe a single pool of
// sentries for a specific template + resource class combination.
type Workload struct {
	Rootfs        string `json:"rootfs"`
	Checkpoint    string `json:"checkpoint"`
	PoolSize      int    `json:"pool_size"`
	MaxPending    int    `json:"max_pending"`
	PreRestore    bool   `json:"pre_restore"`
	ResourceClass string `json:"resource_class"`
}

// Workloads returns a flattened map of template name → Workload for
// backward compatibility with the pool package.
func (c *Config) Workloads() map[string]Workload {
	result := make(map[string]Workload)
	for name, tmpl := range c.Templates {
		for _, pool := range tmpl.Pools {
			key := name + "/" + pool.ResourceClass
			result[key] = Workload{
				Rootfs:        tmpl.Rootfs,
				Checkpoint:    tmpl.Checkpoint,
				PoolSize:      pool.PoolSize,
				MaxPending:    pool.EffectiveMaxPending(),
				PreRestore:    pool.PreRestore,
				ResourceClass: pool.ResourceClass,
			}
		}
	}
	return result
}
