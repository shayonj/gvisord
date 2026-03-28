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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCommandWithCommand(t *testing.T) {
	req := ExecRequest{Command: "echo hello"}
	argv, scriptFile, cleanup := buildCommand(req)
	if cleanup != nil {
		defer cleanup()
	}
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" || argv[2] != "echo hello" {
		t.Errorf("argv = %v, want [sh -c echo hello]", argv)
	}
	if scriptFile != "" {
		t.Errorf("scriptFile = %q, want empty", scriptFile)
	}
}

func TestBuildCommandWithScript(t *testing.T) {
	req := ExecRequest{Script: "print('hello')", Interpreter: "python3"}
	argv, scriptFile, cleanup := buildCommand(req)
	if cleanup != nil {
		defer cleanup()
	}
	if len(argv) != 2 || argv[0] != "python3" {
		t.Errorf("argv = %v, want [python3 <file>]", argv)
	}
	if scriptFile == "" {
		t.Fatal("expected scriptFile to be set")
	}
	data, err := os.ReadFile(scriptFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "print('hello')" {
		t.Errorf("script content = %q", string(data))
	}
}

func TestBuildCommandDefaultInterpreter(t *testing.T) {
	req := ExecRequest{Script: "print(1)"}
	argv, _, cleanup := buildCommand(req)
	if cleanup != nil {
		defer cleanup()
	}
	if argv[0] != "python3" {
		t.Errorf("default interpreter = %q, want python3", argv[0])
	}
}

func TestBuildCommandWithEvent(t *testing.T) {
	event := json.RawMessage(`{"key":"value"}`)
	req := ExecRequest{Script: "print(event)", Interpreter: "python3", Event: event}
	argv, scriptFile, cleanup := buildCommand(req)
	if cleanup != nil {
		defer cleanup()
	}
	if len(argv) != 2 {
		t.Fatalf("argv = %v", argv)
	}
	data, err := os.ReadFile(scriptFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "import json as _json") {
		t.Error("expected event injection preamble")
	}
	if !strings.Contains(content, "print(event)") {
		t.Error("expected script body")
	}
}

func TestInjectEventPython(t *testing.T) {
	event := json.RawMessage(`{"foo":"bar"}`)
	result := injectEvent("python3", event)
	if !strings.Contains(result, "_json.loads") {
		t.Errorf("python event = %q, expected json.loads", result)
	}
}

func TestInjectEventNodeUsesJSONParse(t *testing.T) {
	event := json.RawMessage(`{"foo":"bar"}`)
	result := injectEvent("node", event)
	if !strings.Contains(result, "JSON.parse") {
		t.Errorf("node event = %q, expected JSON.parse for safety", result)
	}
	// Must NOT use raw interpolation
	if strings.Contains(result, "= {\"foo\"") {
		t.Error("node event should use JSON.parse, not raw interpolation")
	}
}

func TestInjectEventBun(t *testing.T) {
	event := json.RawMessage(`[1,2,3]`)
	result := injectEvent("bun", event)
	if !strings.Contains(result, "JSON.parse") {
		t.Errorf("bun event = %q, expected JSON.parse", result)
	}
}

func TestInjectEventShell(t *testing.T) {
	event := json.RawMessage(`"hello"`)
	result := injectEvent("bash", event)
	if !strings.Contains(result, "export EVENT=") {
		t.Errorf("shell event = %q, expected export", result)
	}
}

func TestInterpreterExt(t *testing.T) {
	tests := []struct {
		interp, ext string
	}{
		{"python3", ".py"},
		{"python", ".py"},
		{"node", ".js"},
		{"bun", ".js"},
		{"ruby", ".rb"},
		{"bash", ".sh"},
		{"unknown", ".sh"},
	}
	for _, tt := range tests {
		if got := interpreterExt(tt.interp); got != tt.ext {
			t.Errorf("interpreterExt(%q) = %q, want %q", tt.interp, got, tt.ext)
		}
	}
}

func TestBuildEnvAddsDeps(t *testing.T) {
	dir := t.TempDir()
	dep1 := filepath.Join(dir, "pkg1")
	dep2 := filepath.Join(dir, "pkg2")
	if err := os.MkdirAll(dep1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dep2, 0755); err != nil {
		t.Fatal(err)
	}

	old := cacheBase
	cacheBase = dir
	defer func() { cacheBase = old }()

	req := ExecRequest{Deps: []string{"pkg1", "pkg2"}}
	env := buildEnv(req)

	foundPythonPath := false
	foundNodePath := false
	for _, e := range env {
		if strings.HasPrefix(e, "PYTHONPATH=") {
			foundPythonPath = true
			if !strings.Contains(e, dep1) || !strings.Contains(e, dep2) {
				t.Errorf("PYTHONPATH = %q, expected both deps", e)
			}
		}
		if strings.HasPrefix(e, "NODE_PATH=") {
			foundNodePath = true
		}
	}
	if !foundPythonPath {
		t.Error("expected PYTHONPATH in env")
	}
	if !foundNodePath {
		t.Error("expected NODE_PATH in env")
	}
}

func TestBuildEnvSkipsMissingDeps(t *testing.T) {
	dir := t.TempDir()
	old := cacheBase
	cacheBase = dir
	defer func() { cacheBase = old }()

	req := ExecRequest{Deps: []string{"nonexistent"}}
	env := buildEnv(req)

	for _, e := range env {
		if strings.HasPrefix(e, "PYTHONPATH=") {
			t.Errorf("unexpected PYTHONPATH for missing dep: %q", e)
		}
	}
}

func TestBuildEnvCustomEnvVars(t *testing.T) {
	req := ExecRequest{Env: map[string]string{"MY_VAR": "hello"}}
	env := buildEnv(req)

	found := false
	for _, e := range env {
		if e == "MY_VAR=hello" {
			found = true
		}
	}
	if !found {
		t.Error("expected MY_VAR=hello in env")
	}
}

func TestAppendOrReplaceNew(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	result := appendOrReplace(env, "FOO", "bar")
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result[1] != "FOO=bar" {
		t.Errorf("result[1] = %q, want FOO=bar", result[1])
	}
}

func TestAppendOrReplaceExisting(t *testing.T) {
	env := []string{"PYTHONPATH=/old"}
	result := appendOrReplace(env, "PYTHONPATH", "/new")
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1", len(result))
	}
	if !strings.HasPrefix(result[0], "PYTHONPATH=/new:") {
		t.Errorf("result = %q, expected prepend", result[0])
	}
}

func TestExecRequestParsing(t *testing.T) {
	input := `{"script":"print(1)","interpreter":"python3","deps":["numpy"],"timeout":10}`
	var req ExecRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatal(err)
	}
	if req.Script != "print(1)" {
		t.Errorf("script = %q", req.Script)
	}
	if req.Interpreter != "python3" {
		t.Errorf("interpreter = %q", req.Interpreter)
	}
	if len(req.Deps) != 1 || req.Deps[0] != "numpy" {
		t.Errorf("deps = %v", req.Deps)
	}
	if req.Timeout != 10 {
		t.Errorf("timeout = %d", req.Timeout)
	}
}
