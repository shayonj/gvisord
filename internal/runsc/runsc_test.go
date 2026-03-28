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

package runsc

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewClientWarmSentryFlag(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	c := NewClient("/usr/local/bin/runsc", []string{"--warm-sentry", "--debug"}, log)
	if !c.WarmSentry() {
		t.Error("expected warm sentry to be true")
	}
	for _, f := range c.extraFlags {
		if f == "--warm-sentry" {
			t.Error("--warm-sentry should be stripped from extraFlags")
		}
	}
	if len(c.extraFlags) != 1 || c.extraFlags[0] != "--debug" {
		t.Errorf("extraFlags = %v, want [--debug]", c.extraFlags)
	}

	c2 := NewClient("/usr/local/bin/runsc", []string{"--debug"}, log)
	if c2.WarmSentry() {
		t.Error("expected warm sentry to be false")
	}
}

func TestNewClientNoFlags(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewClient("/usr/bin/runsc", nil, log)
	if c.WarmSentry() {
		t.Error("expected warm sentry to be false with nil flags")
	}
	if len(c.extraFlags) != 0 {
		t.Errorf("extraFlags = %v, want empty", c.extraFlags)
	}
}

func TestPrepareBundleCreatesSymlinkAndConfig(t *testing.T) {
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		t.Fatal(err)
	}

	checkpointDir := filepath.Join(root, "checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "config.json"), []byte(`{"ociVersion":"1.0"}`), 0644); err != nil {
		t.Fatal(err)
	}

	bundleDir := filepath.Join(root, "bundle")
	if err := PrepareBundle(bundleDir, rootfs, checkpointDir); err != nil {
		t.Fatalf("PrepareBundle: %v", err)
	}

	// Check rootfs symlink
	link := filepath.Join(bundleDir, "rootfs")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("reading rootfs symlink: %v", err)
	}
	if target != rootfs {
		t.Errorf("rootfs symlink target = %q, want %q", target, rootfs)
	}

	// Check config.json was copied
	configData, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		t.Fatalf("reading config.json: %v", err)
	}
	if string(configData) != `{"ociVersion":"1.0"}` {
		t.Errorf("config.json = %q, want {\"ociVersion\":\"1.0\"}", string(configData))
	}
}

func TestPrepareBundleMissingConfig(t *testing.T) {
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		t.Fatal(err)
	}
	checkpointDir := filepath.Join(root, "empty-checkpoint")
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		t.Fatal(err)
	}

	bundleDir := filepath.Join(root, "bundle")
	err := PrepareBundle(bundleDir, rootfs, checkpointDir)
	if err == nil {
		t.Fatal("expected error when config.json is missing")
	}
	if !strings.Contains(err.Error(), "config.json") {
		t.Errorf("error = %v, want mention of config.json", err)
	}
}

func TestCleanFilestoresRemovesGvisorFiles(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".gvisor.filestore"), []byte("x"), 0644)
	_ = os.WriteFile(filepath.Join(root, ".gvisor-overlay"), []byte("y"), 0644)
	_ = os.WriteFile(filepath.Join(root, "keep-me.txt"), []byte("z"), 0644)

	CleanFilestores(root)

	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gvisor") {
			t.Errorf("expected .gvisor file to be removed: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(root, "keep-me.txt")); err != nil {
		t.Error("keep-me.txt should not have been removed")
	}
}

func TestCleanFilestoresNonexistentDir(t *testing.T) {
	// Should not panic
	CleanFilestores("/nonexistent/path/to/rootfs")
}

func TestKillProcessZeroPID(t *testing.T) {
	// Should not panic
	KillProcess(0)
	KillProcess(-1)
}

func TestRuntimeInterfaceCompliance(t *testing.T) {
	// Verify Client implements Runtime at compile time.
	var _ Runtime = (*Client)(nil)
}

func writeTestConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readTestSpec(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestInjectBundleMountsAddsHarnessAndCache(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"ociVersion":"1.0","mounts":[]}`)

	if err := InjectBundleMounts(dir, "/usr/bin/harness", "/var/cache"); err != nil {
		t.Fatal(err)
	}

	spec := readTestSpec(t, dir)
	mounts := spec["mounts"].([]any)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}

	m0 := mounts[0].(map[string]any)
	if m0["destination"] != "/harness/gvisord-exec" {
		t.Errorf("mount[0] dest = %v", m0["destination"])
	}
	m1 := mounts[1].(map[string]any)
	if m1["destination"] != "/cache" {
		t.Errorf("mount[1] dest = %v", m1["destination"])
	}
}

func TestInjectBundleMountsSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"ociVersion":"1.0","mounts":[]}`)

	if err := InjectBundleMounts(dir, "", ""); err != nil {
		t.Fatal(err)
	}

	spec := readTestSpec(t, dir)
	mounts := spec["mounts"].([]any)
	if len(mounts) != 0 {
		t.Errorf("expected 0 mounts for empty paths, got %d", len(mounts))
	}
}

func TestInjectNetNSAddsNamespace(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"ociVersion":"1.0","linux":{"namespaces":[{"type":"pid"}]}}`)

	if err := InjectNetNS(dir, "/var/run/netns/test-1"); err != nil {
		t.Fatal(err)
	}

	spec := readTestSpec(t, dir)
	linux := spec["linux"].(map[string]any)
	ns := linux["namespaces"].([]any)
	if len(ns) != 2 {
		t.Fatalf("expected 2 namespaces, got %d", len(ns))
	}
	netns := ns[1].(map[string]any)
	if netns["type"] != "network" || netns["path"] != "/var/run/netns/test-1" {
		t.Errorf("netns = %v", netns)
	}
}

func TestInjectNetNSUpdatesExisting(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `{"ociVersion":"1.0","linux":{"namespaces":[{"type":"network","path":"/old"}]}}`)

	if err := InjectNetNS(dir, "/new/path"); err != nil {
		t.Fatal(err)
	}

	spec := readTestSpec(t, dir)
	linux := spec["linux"].(map[string]any)
	ns := linux["namespaces"].([]any)
	if len(ns) != 1 {
		t.Fatalf("expected 1 namespace (updated, not added), got %d", len(ns))
	}
	netns := ns[0].(map[string]any)
	if netns["path"] != "/new/path" {
		t.Errorf("netns path = %v, want /new/path", netns["path"])
	}
}

func TestInjectNetNSNoopForEmpty(t *testing.T) {
	if err := InjectNetNS("/nonexistent", ""); err != nil {
		t.Errorf("expected no-op for empty path: %v", err)
	}
}
