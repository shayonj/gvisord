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

// Command gvisord-exec is a static harness binary that runs inside any gVisor
// sandbox. It starts an HTTP server, accepts a single execution request, spawns
// the appropriate interpreter, captures stdout/stderr, returns the result as
// JSON, and exits. Built with CGO_ENABLED=0 so it works in any Linux image.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	listenAddr = envOr("HARNESS_ADDR", ":8080")
	cacheBase  = envOr("HARNESS_CACHE_BASE", "/cache")
)

// ExecRequest is the JSON body for POST /run.
type ExecRequest struct {
	Script      string            `json:"script"`
	Interpreter string            `json:"interpreter,omitempty"`
	Command     string            `json:"command,omitempty"`
	Deps        []string          `json:"deps,omitempty"`
	Event       json.RawMessage   `json:"event,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Timeout     int               `json:"timeout,omitempty"`
}

// ExecResponse is the JSON response from POST /run.
type ExecResponse struct {
	Type      string  `json:"type"`
	ExitCode  int     `json:"exit_code"`
	Stdout    string  `json:"stdout"`
	Stderr    string  `json:"stderr"`
	ElapsedMs float64 `json:"elapsed_ms"`
	Error     string  `json:"error,omitempty"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/run", handleRun)

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	}()

	fmt.Fprintf(os.Stderr, "HARNESS_READY addr=%s pid=%d cache=%s\n", listenAddr, os.Getpid(), cacheBase)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "harness: listen error: %v\n", err)
		os.Exit(1)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, ExecResponse{Type: "error", Error: "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ExecResponse{Type: "error", Error: err.Error()})
		return
	}

	var req ExecRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ExecResponse{Type: "error", Error: err.Error()})
		return
	}

	resp := execute(req)
	writeJSON(w, http.StatusOK, resp)

	// Single-use: shut down after handling one request.
	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Exit(0)
	}()
}

func execute(req ExecRequest) ExecResponse {
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	argv, scriptFile, cleanup := buildCommand(req)
	if cleanup != nil {
		defer cleanup()
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = "/tmp"
	cmd.Env = buildEnv(req)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return ExecResponse{Type: "error", ExitCode: -1, Error: fmt.Sprintf("stdout pipe: %v", err)}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return ExecResponse{Type: "error", ExitCode: -1, Error: fmt.Sprintf("stderr pipe: %v", err)}
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return ExecResponse{Type: "error", ExitCode: -1, Error: err.Error(), ElapsedMs: msSince(start)}
	}

	stdoutBytes, _ := io.ReadAll(io.LimitReader(stdoutPipe, 10<<20))
	stderrBytes, _ := io.ReadAll(io.LimitReader(stderrPipe, 1<<20))

	err = cmd.Wait()
	elapsed := msSince(start)

	if ctx.Err() == context.DeadlineExceeded {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return ExecResponse{
			Type:      "timeout",
			ExitCode:  -1,
			Stdout:    string(stdoutBytes),
			Stderr:    string(stderrBytes),
			ElapsedMs: elapsed,
			Error:     fmt.Sprintf("execution exceeded %s timeout", timeout),
		}
	}

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return ExecResponse{Type: "error", ExitCode: -1, Error: err.Error(), ElapsedMs: elapsed}
		}
	}

	typ := "success"
	if exitCode != 0 {
		typ = "error"
	}

	_ = os.Remove(scriptFile)

	return ExecResponse{
		Type:      typ,
		ExitCode:  exitCode,
		Stdout:    string(stdoutBytes),
		Stderr:    string(stderrBytes),
		ElapsedMs: elapsed,
	}
}

func buildCommand(req ExecRequest) (argv []string, scriptFile string, cleanup func()) {
	if req.Command != "" {
		return []string{"sh", "-c", req.Command}, "", nil
	}

	interp := req.Interpreter
	if interp == "" {
		interp = "python3"
	}

	ext := interpreterExt(interp)
	f, err := os.CreateTemp("/tmp", "gvisord-exec-*"+ext)
	if err != nil {
		return []string{"sh", "-c", "echo 'failed to create script file' >&2; exit 1"}, "", nil
	}

	content := req.Script
	if len(req.Event) > 0 {
		content = injectEvent(interp, req.Event) + "\n" + content
	}

	if _, err := f.WriteString(content); err != nil {
		return []string{"sh", "-c", "echo 'failed to write script' >&2; exit 1"}, "", nil
	}
	f.Close()
	scriptFile = f.Name()

	return []string{interp, scriptFile}, scriptFile, func() { os.Remove(scriptFile) }
}

func buildEnv(req ExecRequest) []string {
	env := os.Environ()

	depPaths := make([]string, 0, len(req.Deps))
	for _, dep := range req.Deps {
		full := cacheBase + "/" + dep
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			depPaths = append(depPaths, full)
		}
	}

	if len(depPaths) > 0 {
		joined := strings.Join(depPaths, ":")
		env = appendOrReplace(env, "PYTHONPATH", joined)
		env = appendOrReplace(env, "NODE_PATH", joined)
	}

	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}

	return env
}

func injectEvent(interp string, event json.RawMessage) string {
	switch {
	case strings.Contains(interp, "python"):
		return fmt.Sprintf("import json as _json\nevent = _json.loads(%q)", string(event))
	case strings.Contains(interp, "node"), strings.Contains(interp, "bun"):
		return fmt.Sprintf("const event = JSON.parse(%q);", string(event))
	default:
		return fmt.Sprintf("export EVENT=%q", string(event))
	}
}

func interpreterExt(interp string) string {
	switch {
	case strings.Contains(interp, "python"):
		return ".py"
	case strings.Contains(interp, "node"), strings.Contains(interp, "bun"):
		return ".js"
	case strings.Contains(interp, "ruby"):
		return ".rb"
	default:
		return ".sh"
	}
}

func appendOrReplace(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value + ":" + e[len(prefix):]
			return env
		}
	}
	return append(env, prefix+value)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
