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

// Package cgroup manages per-resource-class cgroup slices for warm sentries.
// Each resource class (small, medium, large) gets its own cgroup slice with
// hard CPU and memory limits. Sentries are placed into the appropriate slice
// at creation time, providing kernel-enforced per-execution resource isolation.
package cgroup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ResourceClass defines CPU and memory limits for a class of sentries.
type ResourceClass struct {
	Name      string `json:"name"`
	CPUMillis int    `json:"cpu_millis"`
	MemoryMB  int    `json:"memory_mb"`
}

// Manager creates and manages cgroup slices for resource classes.
type Manager struct {
	basePath string
	prefix   string
	log      *slog.Logger
	classes  map[string]ResourceClass
}

// NewManager creates a cgroup manager. basePath is typically
// /sys/fs/cgroup. prefix is used to namespace gvisord's slices
// (e.g. "gvisord" → gvisord-small.slice).
func NewManager(basePath, prefix string, log *slog.Logger) *Manager {
	return &Manager{
		basePath: basePath,
		prefix:   prefix,
		log:      log,
		classes:  make(map[string]ResourceClass),
	}
}

// EnsureSlices creates cgroup slices for all resource classes. On cgroup v2
// this creates directories under basePath and writes cpu.max and memory.max.
func (m *Manager) EnsureSlices(classes []ResourceClass) error {
	for _, cls := range classes {
		m.classes[cls.Name] = cls

		slicePath := m.slicePath(cls.Name)
		if err := os.MkdirAll(slicePath, 0755); err != nil {
			return fmt.Errorf("creating cgroup slice %s: %w", cls.Name, err)
		}

		if err := m.writeCPUMax(slicePath, cls.CPUMillis); err != nil {
			return fmt.Errorf("setting cpu.max for %s: %w", cls.Name, err)
		}

		if err := m.writeMemoryMax(slicePath, cls.MemoryMB); err != nil {
			return fmt.Errorf("setting memory.max for %s: %w", cls.Name, err)
		}

		m.log.Info("cgroup slice ready",
			"class", cls.Name,
			"cpu_millis", cls.CPUMillis,
			"memory_mb", cls.MemoryMB,
			"path", slicePath,
		)
	}
	return nil
}

// SentrySlicePath returns the cgroup path for a specific sentry within a
// resource class. The sentry gets its own sub-cgroup so its resource usage
// is isolated from other sentries in the same class.
func (m *Manager) SentrySlicePath(className, sentryID string) (string, error) {
	cls, ok := m.classes[className]
	if !ok {
		return "", fmt.Errorf("unknown resource class %q", className)
	}

	sentryPath := filepath.Join(m.slicePath(cls.Name), sanitize(sentryID))
	if err := os.MkdirAll(sentryPath, 0755); err != nil {
		return "", fmt.Errorf("creating sentry cgroup %s: %w", sentryID, err)
	}

	return sentryPath, nil
}

// PlacePID moves a process into a sentry's cgroup.
func (m *Manager) PlacePID(cgroupPath string, pid int) error {
	procsPath := filepath.Join(cgroupPath, "cgroup.procs")
	return os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0644)
}

// CleanupSentry removes a sentry's cgroup directory.
func (m *Manager) CleanupSentry(className, sentryID string) {
	sentryPath := filepath.Join(m.slicePath(className), sanitize(sentryID))
	if err := os.Remove(sentryPath); err != nil && !os.IsNotExist(err) {
		m.log.Debug("failed to remove sentry cgroup", "path", sentryPath, "err", err)
	}
}

// GetClass returns a resource class by name.
func (m *Manager) GetClass(name string) (ResourceClass, bool) {
	cls, ok := m.classes[name]
	return cls, ok
}

func (m *Manager) slicePath(className string) string {
	return filepath.Join(m.basePath, m.prefix+"-"+className+".slice")
}

// writeCPUMax sets the cpu.max value for cgroup v2.
// CPUMillis=1000 means 1 full CPU (100000/100000).
// CPUMillis=500 means 0.5 CPU (50000/100000).
func (m *Manager) writeCPUMax(slicePath string, cpuMillis int) error {
	quota := cpuMillis * 100
	period := 100000
	value := fmt.Sprintf("%d %d", quota, period)
	return writeFile(filepath.Join(slicePath, "cpu.max"), value)
}

// writeMemoryMax sets the memory.max value for cgroup v2.
func (m *Manager) writeMemoryMax(slicePath string, memoryMB int) error {
	bytes := int64(memoryMB) * 1024 * 1024
	return writeFile(filepath.Join(slicePath, "memory.max"), strconv.FormatInt(bytes, 10))
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// DetectV2 checks whether the base path is a cgroup v2 unified hierarchy
// by looking for the cgroup.controllers file.
func (m *Manager) DetectV2() error {
	controllersPath := filepath.Join(m.basePath, "cgroup.controllers")
	if _, err := os.Stat(controllersPath); err != nil {
		return fmt.Errorf("cgroup v2 not detected at %s: %w (gvisord requires cgroup v2)", m.basePath, err)
	}
	return nil
}

// CleanupStaleSlices removes any leftover gvisord cgroup slices from a
// previous run. Only removes directories matching the prefix pattern.
func (m *Manager) CleanupStaleSlices() {
	entries, err := os.ReadDir(m.basePath)
	if err != nil {
		return
	}
	prefix := m.prefix + "-"
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".slice") {
			slicePath := filepath.Join(m.basePath, e.Name())
			subs, _ := os.ReadDir(slicePath)
			for _, sub := range subs {
				if sub.IsDir() {
					os.Remove(filepath.Join(slicePath, sub.Name()))
				}
			}
			if err := os.Remove(slicePath); err != nil {
				m.log.Debug("failed to remove stale cgroup slice", "path", slicePath, "err", err)
			} else {
				m.log.Info("cleaned up stale cgroup slice", "path", slicePath)
			}
		}
	}
}

func sanitize(id string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ".", "-")
	return r.Replace(id)
}
