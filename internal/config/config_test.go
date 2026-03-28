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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFile(t *testing.T) {
	content := `{
		"templates": {
			"python": {
				"rootfs": "/tmp/r",
				"checkpoint": "/tmp/c",
				"pools": [
					{"resource_class": "small", "pool_size": 5},
					{"resource_class": "large", "pool_size": 2, "max_pending": 8}
				]
			}
		},
		"resource_classes": [
			{"name": "small", "cpu_millis": 500, "memory_mb": 512},
			{"name": "large", "cpu_millis": 2000, "memory_mb": 4096}
		],
		"limits": {"max_restores_per_sentry": 50, "max_sentry_age": "5m"}
	}`
	f, err := os.CreateTemp("", "gvisord-test-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Templates) != 1 {
		t.Fatalf("templates count = %d, want 1", len(cfg.Templates))
	}
	tmpl := cfg.Templates["python"]
	if tmpl.Rootfs != "/tmp/r" {
		t.Errorf("python rootfs = %q, want /tmp/r", tmpl.Rootfs)
	}
	if tmpl.Checkpoint != "/tmp/c" {
		t.Errorf("python checkpoint = %q, want /tmp/c", tmpl.Checkpoint)
	}
	if len(tmpl.Pools) != 2 {
		t.Fatalf("python pools count = %d, want 2", len(tmpl.Pools))
	}
	if tmpl.Pools[0].PoolSize != 5 {
		t.Errorf("small pool_size = %d, want 5", tmpl.Pools[0].PoolSize)
	}
	if tmpl.Pools[1].MaxPending != 8 {
		t.Errorf("large max_pending = %d, want 8", tmpl.Pools[1].MaxPending)
	}
	if !tmpl.Pools[0].PreRestore {
		t.Error("small pre_restore = false, want default true")
	}

	if cfg.Limits.MaxRestoresPerSentry != 50 {
		t.Errorf("max restores = %d, want 50", cfg.Limits.MaxRestoresPerSentry)
	}
	if cfg.Limits.MaxSentryAge != 5*time.Minute {
		t.Errorf("max age = %v, want 5m", cfg.Limits.MaxSentryAge)
	}
	if cfg.Limits.MaxOpenFDs != 512 {
		t.Errorf("max fds = %d, want default 512", cfg.Limits.MaxOpenFDs)
	}

	workloads := cfg.Workloads()
	if len(workloads) != 2 {
		t.Fatalf("workloads count = %d, want 2", len(workloads))
	}
}

func TestLoadPreservesExplicitPreRestoreFalse(t *testing.T) {
	content := `{
		"templates": {
			"python": {
				"rootfs": "/tmp/r",
				"checkpoint": "/tmp/c",
				"pools": [
					{"resource_class": "small", "pool_size": 1, "pre_restore": false}
				]
			}
		},
		"resource_classes": [{"name": "small", "cpu_millis": 500, "memory_mb": 512}]
	}`

	f, err := os.CreateTemp("", "gvisord-test-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Templates["python"].Pools[0].PreRestore {
		t.Fatal("python pre_restore = true, want explicit false preserved")
	}
}

func TestValidate(t *testing.T) {
	makeValid := func() *Config {
		return &Config{
			Runsc: RunscConfig{Path: "/usr/local/bin/runsc"},
			Templates: map[string]Template{
				"test": {
					Rootfs:     "/r",
					Checkpoint: "/c",
					Pools: []PoolConfig{
						{ResourceClass: "small", PoolSize: 2},
					},
				},
			},
			ResourceClasses: []ResourceClassConfig{
				{Name: "small", CPUMillis: 500, MemoryMB: 512},
			},
			Network: NetworkConfig{Mode: "none"},
			Limits:  DefaultLimits,
		}
	}

	if err := makeValid().Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}

	c := makeValid()
	c.Templates = nil
	if err := c.Validate(); err == nil {
		t.Error("empty templates should fail")
	}

	c = makeValid()
	c.Templates["test"] = Template{Rootfs: "", Checkpoint: "/c", Pools: []PoolConfig{{ResourceClass: "small", PoolSize: 1}}}
	if err := c.Validate(); err == nil {
		t.Error("missing rootfs should fail")
	}

	c = makeValid()
	c.Templates["test"] = Template{Rootfs: "/r", Checkpoint: "/c", Pools: []PoolConfig{{ResourceClass: "small", PoolSize: 0}}}
	if err := c.Validate(); err == nil {
		t.Error("zero pool_size should fail")
	}

	c = makeValid()
	c.Templates["test"] = Template{Rootfs: "/r", Checkpoint: "/c", Pools: []PoolConfig{{ResourceClass: "unknown", PoolSize: 1}}}
	if err := c.Validate(); err == nil {
		t.Error("unknown resource_class should fail")
	}
}

func TestLoadRequiresPath(t *testing.T) {
	_, err := Load("")
	if err == nil {
		t.Error("empty path should fail")
	}
}

func TestResourceClassDefaults(t *testing.T) {
	content := `{
		"templates": {
			"python": {
				"rootfs": "/tmp/r",
				"checkpoint": "/tmp/c",
				"pools": [{"resource_class": "small", "pool_size": 1}]
			}
		}
	}`

	f, err := os.CreateTemp("", "gvisord-test-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.ResourceClasses) != 3 {
		t.Fatalf("default resource_classes count = %d, want 3", len(cfg.ResourceClasses))
	}
	if cfg.Network.Bridge != "gvisord0" {
		t.Errorf("default bridge = %q, want gvisord0", cfg.Network.Bridge)
	}
}

func TestValidateCheckpointPathAllowed(t *testing.T) {
	cfg := &Config{
		Daemon: DaemonConfig{
			CheckpointDirs: []string{"/var/gvisord/checkpoints", "/mnt/ckpts"},
		},
	}
	if err := cfg.ValidateCheckpointPath("/var/gvisord/checkpoints/python"); err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}
	if err := cfg.ValidateCheckpointPath("/mnt/ckpts/node/v2"); err != nil {
		t.Errorf("expected allowed, got: %v", err)
	}
}

func TestValidateCheckpointPathBlocked(t *testing.T) {
	cfg := &Config{
		Daemon: DaemonConfig{
			CheckpointDirs: []string{"/var/gvisord/checkpoints"},
		},
	}
	if err := cfg.ValidateCheckpointPath("/etc/shadow"); err == nil {
		t.Error("expected blocked for /etc/shadow")
	}
	if err := cfg.ValidateCheckpointPath("/var/gvisord/other"); err == nil {
		t.Error("expected blocked for path outside allowed dirs")
	}
}

func TestValidateCheckpointPathEmptyAllowsAll(t *testing.T) {
	cfg := &Config{}
	if err := cfg.ValidateCheckpointPath("/any/path"); err != nil {
		t.Errorf("empty checkpoint_dirs should allow all: %v", err)
	}
}

func TestLeaseTimeoutParsing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
		"daemon": {"lease_timeout": "10m"},
		"templates": {"py": {"rootfs":"/r","checkpoint":"/c","pools":[{"resource_class":"small","pool_size":1}]}}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.LeaseTimeout != 10*time.Minute {
		t.Errorf("lease_timeout = %v, want 10m", cfg.Daemon.LeaseTimeout)
	}
}

func TestCgroupConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{
		"templates": {"py": {"rootfs":"/r","checkpoint":"/c","pools":[{"resource_class":"small","pool_size":1}]}}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cgroup.BasePath != "/sys/fs/cgroup" {
		t.Errorf("cgroup.base_path = %q, want /sys/fs/cgroup", cfg.Cgroup.BasePath)
	}
	if cfg.Cgroup.Prefix != "gvisord" {
		t.Errorf("cgroup.prefix = %q, want gvisord", cfg.Cgroup.Prefix)
	}
}
