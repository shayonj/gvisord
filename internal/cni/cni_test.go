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

package cni

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEnsureConfigWritesFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		PluginDir: "/opt/cni/bin",
		ConfigDir: dir,
		Bridge:    "test0",
		Subnet:    "10.99.0.0/16",
		NetnsDir:  "/var/run/netns",
	}
	mgr := NewManager(cfg, testLogger())

	if err := mgr.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}

	// Check bridge config
	bridgePath := filepath.Join(dir, "10-gvisord-bridge.conf")
	data, err := os.ReadFile(bridgePath)
	if err != nil {
		t.Fatalf("reading bridge config: %v", err)
	}
	var bridgeCfg map[string]any
	if err := json.Unmarshal(data, &bridgeCfg); err != nil {
		t.Fatalf("parsing bridge config: %v", err)
	}
	if bridgeCfg["type"] != "bridge" {
		t.Errorf("type = %v, want bridge", bridgeCfg["type"])
	}
	if bridgeCfg["bridge"] != "test0" {
		t.Errorf("bridge = %v, want test0", bridgeCfg["bridge"])
	}

	// Check loopback config
	loPath := filepath.Join(dir, "99-loopback.conf")
	loData, err := os.ReadFile(loPath)
	if err != nil {
		t.Fatalf("reading loopback config: %v", err)
	}
	var loCfg map[string]any
	if err := json.Unmarshal(loData, &loCfg); err != nil {
		t.Fatalf("parsing loopback config: %v", err)
	}
	if loCfg["type"] != "loopback" {
		t.Errorf("type = %v, want loopback", loCfg["type"])
	}
}

func TestEnsureConfigDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		PluginDir: "/opt/cni/bin",
		ConfigDir: dir,
		Bridge:    "test0",
		Subnet:    "10.99.0.0/16",
		NetnsDir:  "/var/run/netns",
	}
	mgr := NewManager(cfg, testLogger())

	// Pre-create the files with custom content
	bridgePath := filepath.Join(dir, "10-gvisord-bridge.conf")
	if err := os.WriteFile(bridgePath, []byte(`{"custom":"yes"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := mgr.EnsureConfig(); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}

	// File should not be overwritten
	data, _ := os.ReadFile(bridgePath)
	if string(data) != `{"custom":"yes"}` {
		t.Error("EnsureConfig overwrote existing bridge config")
	}
}

func TestGetIPEmptyMap(t *testing.T) {
	cfg := Config{PluginDir: "/opt/cni/bin", NetnsDir: "/var/run/netns"}
	mgr := NewManager(cfg, testLogger())

	_, ok := mgr.GetIP("nonexistent")
	if ok {
		t.Error("expected no IP for nonexistent id")
	}
}

func TestParseIPFromCNIOutput(t *testing.T) {
	cfg := Config{PluginDir: "/opt/cni/bin", NetnsDir: "/var/run/netns"}
	mgr := NewManager(cfg, testLogger())

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "standard output",
			input: `{"ips":[{"address":"10.88.0.5/16"}]}`,
			want:  "10.88.0.5",
		},
		{
			name:  "no CIDR",
			input: `{"ips":[{"address":"10.88.0.5"}]}`,
			want:  "10.88.0.5",
		},
		{
			name:  "empty ips",
			input: `{"ips":[]}`,
			want:  "",
		},
		{
			name:  "bad json",
			input: `not json`,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mgr.parseIP([]byte(tt.input))
			if got != tt.want {
				t.Errorf("parseIP(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCNIEnvContainsRequiredVars(t *testing.T) {
	cfg := Config{
		PluginDir: "/opt/cni/bin",
		ConfigDir: "/etc/cni/net.d",
		Bridge:    "gvisord0",
		Subnet:    "10.88.0.0/16",
		NetnsDir:  "/var/run/netns",
	}
	mgr := NewManager(cfg, testLogger())

	env := mgr.cniEnv("test-id", "ADD")
	found := map[string]bool{}
	for _, e := range env {
		for _, key := range []string{"CNI_PATH=", "CNI_CONTAINERID=", "CNI_COMMAND=", "CNI_NETNS=", "PATH="} {
			if len(e) >= len(key) && e[:len(key)] == key {
				found[key] = true
			}
		}
	}
	for _, key := range []string{"CNI_PATH=", "CNI_CONTAINERID=", "CNI_COMMAND=", "CNI_NETNS=", "PATH="} {
		if !found[key] {
			t.Errorf("missing env var %s", key)
		}
	}
}

func TestCNIEnvUsesConfiguredNetnsDir(t *testing.T) {
	cfg := Config{
		PluginDir: "/opt/cni/bin",
		NetnsDir:  "/custom/netns",
	}
	mgr := NewManager(cfg, testLogger())

	env := mgr.cniEnv("sentry-1", "ADD")
	for _, e := range env {
		if len(e) > 10 && e[:10] == "CNI_NETNS=" {
			want := "CNI_NETNS=/custom/netns/sentry-1"
			if e != want {
				t.Errorf("CNI_NETNS = %q, want %q", e, want)
			}
			return
		}
	}
	t.Error("CNI_NETNS not found in env")
}

func TestValidatePluginsSuccess(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"bridge", "loopback"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{PluginDir: dir}
	mgr := NewManager(cfg, testLogger())
	if err := mgr.ValidatePlugins(); err != nil {
		t.Errorf("expected success: %v", err)
	}
}

func TestValidatePluginsMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{PluginDir: dir}
	mgr := NewManager(cfg, testLogger())
	if err := mgr.ValidatePlugins(); err == nil {
		t.Error("expected error when plugins are missing")
	}
}

func TestValidatePluginsNotExecutable(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"bridge", "loopback"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{PluginDir: dir}
	mgr := NewManager(cfg, testLogger())
	if err := mgr.ValidatePlugins(); err == nil {
		t.Error("expected error when plugins are not executable")
	}
}
