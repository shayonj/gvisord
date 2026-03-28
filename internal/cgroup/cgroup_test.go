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

package cgroup

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEnsureSlicesCreatesDirectoriesAndFiles(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, "gvisord", testLogger())

	classes := []ResourceClass{
		{Name: "small", CPUMillis: 500, MemoryMB: 512},
		{Name: "large", CPUMillis: 2000, MemoryMB: 4096},
	}
	if err := mgr.EnsureSlices(classes); err != nil {
		t.Fatalf("EnsureSlices: %v", err)
	}

	// Check small slice
	smallDir := filepath.Join(base, "gvisord-small.slice")
	if _, err := os.Stat(smallDir); err != nil {
		t.Fatalf("small slice dir missing: %v", err)
	}
	cpuMax, _ := os.ReadFile(filepath.Join(smallDir, "cpu.max"))
	if !strings.Contains(string(cpuMax), "50000 100000") {
		t.Errorf("cpu.max = %q, want '50000 100000'", string(cpuMax))
	}
	memMax, _ := os.ReadFile(filepath.Join(smallDir, "memory.max"))
	if strings.TrimSpace(string(memMax)) != "536870912" {
		t.Errorf("memory.max = %q, want 536870912", strings.TrimSpace(string(memMax)))
	}

	// Check large slice
	largeDir := filepath.Join(base, "gvisord-large.slice")
	if _, err := os.Stat(largeDir); err != nil {
		t.Fatalf("large slice dir missing: %v", err)
	}
	cpuMax2, _ := os.ReadFile(filepath.Join(largeDir, "cpu.max"))
	if !strings.Contains(string(cpuMax2), "200000 100000") {
		t.Errorf("cpu.max = %q, want '200000 100000'", string(cpuMax2))
	}
}

func TestSentrySlicePathCreatesSubdir(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, "gvisord", testLogger())

	classes := []ResourceClass{{Name: "medium", CPUMillis: 1000, MemoryMB: 1024}}
	if err := mgr.EnsureSlices(classes); err != nil {
		t.Fatal(err)
	}

	path, err := mgr.SentrySlicePath("medium", "python-1")
	if err != nil {
		t.Fatalf("SentrySlicePath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("sentry dir not created: %v", err)
	}
	if !strings.Contains(path, "python-1") {
		t.Errorf("path = %q, expected to contain python-1", path)
	}
}

func TestSentrySlicePathUnknownClass(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, "gvisord", testLogger())

	_, err := mgr.SentrySlicePath("nonexistent", "sentry-1")
	if err == nil {
		t.Fatal("expected error for unknown class")
	}
}

func TestPlacePIDWritesProcsFile(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, "gvisord", testLogger())

	classes := []ResourceClass{{Name: "small", CPUMillis: 500, MemoryMB: 512}}
	if err := mgr.EnsureSlices(classes); err != nil {
		t.Fatal(err)
	}

	path, err := mgr.SentrySlicePath("small", "test-sentry")
	if err != nil {
		t.Fatal(err)
	}

	// PlacePID writes to cgroup.procs. In our test env this is just a file.
	if err := mgr.PlacePID(path, 12345); err != nil {
		t.Fatalf("PlacePID: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "12345" {
		t.Errorf("cgroup.procs = %q, want 12345", string(data))
	}
}

func TestCleanupSentryRemovesDir(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, "gvisord", testLogger())

	classes := []ResourceClass{{Name: "small", CPUMillis: 500, MemoryMB: 512}}
	if err := mgr.EnsureSlices(classes); err != nil {
		t.Fatal(err)
	}

	path, err := mgr.SentrySlicePath("small", "doomed-sentry")
	if err != nil {
		t.Fatal(err)
	}

	mgr.CleanupSentry("small", "doomed-sentry")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected sentry dir to be removed")
	}
}

func TestGetClass(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, "gvisord", testLogger())

	classes := []ResourceClass{
		{Name: "small", CPUMillis: 500, MemoryMB: 512},
	}
	if err := mgr.EnsureSlices(classes); err != nil {
		t.Fatal(err)
	}

	cls, ok := mgr.GetClass("small")
	if !ok {
		t.Fatal("expected to find class 'small'")
	}
	if cls.CPUMillis != 500 {
		t.Errorf("CPUMillis = %d, want 500", cls.CPUMillis)
	}

	_, ok = mgr.GetClass("xlarge")
	if ok {
		t.Error("expected not to find class 'xlarge'")
	}
}

func TestSanitizeID(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"python-1", "python-1"},
		{"a/b/c", "a-b-c"},
		{"has space", "has-space"},
		{"with.dots", "with-dots"},
	}
	for _, tt := range tests {
		if got := sanitize(tt.in); got != tt.want {
			t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDetectV2WithControllersFile(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "cgroup.controllers"), []byte("cpu memory"), 0644); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(base, "gvisord", testLogger())
	if err := mgr.DetectV2(); err != nil {
		t.Errorf("expected v2 detected: %v", err)
	}
}

func TestDetectV2MissingFails(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, "gvisord", testLogger())
	if err := mgr.DetectV2(); err == nil {
		t.Error("expected error when cgroup.controllers is missing")
	}
}

func TestCleanupStaleSlices(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "gvisord-small.slice", "old-sentry"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "gvisord-large.slice"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "other-dir"), 0755); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(base, "gvisord", testLogger())
	mgr.CleanupStaleSlices()

	if _, err := os.Stat(filepath.Join(base, "gvisord-small.slice")); !os.IsNotExist(err) {
		t.Error("expected stale small slice to be removed")
	}
	if _, err := os.Stat(filepath.Join(base, "gvisord-large.slice")); !os.IsNotExist(err) {
		t.Error("expected stale large slice to be removed")
	}
	if _, err := os.Stat(filepath.Join(base, "other-dir")); err != nil {
		t.Error("other-dir should not be removed")
	}
}
