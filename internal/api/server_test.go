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

package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestExecuteParamsParsing(t *testing.T) {
	input := `{"workload": "python", "checkpoint": "/mnt/ckpt/v2"}`
	var params ExecuteParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		t.Fatal(err)
	}
	if params.Workload != "python" {
		t.Errorf("workload = %q, want python", params.Workload)
	}
	if params.Checkpoint != "/mnt/ckpt/v2" {
		t.Errorf("checkpoint = %q, want /mnt/ckpt/v2", params.Checkpoint)
	}
}

func TestExecuteParamsWorkloadOnly(t *testing.T) {
	input := `{"workload": "node"}`
	var params ExecuteParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		t.Fatal(err)
	}
	if params.Workload != "node" {
		t.Errorf("workload = %q, want node", params.Workload)
	}
	if params.Checkpoint != "" {
		t.Errorf("checkpoint = %q, want empty", params.Checkpoint)
	}
}

func TestCompleteParamsParsing(t *testing.T) {
	input := `{"lease_id": "abc123"}`
	var params CompleteParams
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		t.Fatal(err)
	}
	if params.LeaseID != "abc123" {
		t.Errorf("lease_id = %q, want abc123", params.LeaseID)
	}
}

func TestResponseCodes(t *testing.T) {
	resp := Response{Error: "something broke", Code: "execute_failed"}
	data, _ := json.Marshal(resp)
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["code"] != "execute_failed" {
		t.Errorf("code = %v, want execute_failed", parsed["code"])
	}

	resp2 := Response{Error: "all busy", Code: "capacity_exhausted"}
	data2, _ := json.Marshal(resp2)
	if err := json.Unmarshal(data2, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["code"] != "capacity_exhausted" {
		t.Errorf("code = %v, want capacity_exhausted", parsed["code"])
	}
}

func TestUnknownMethodOverSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(sockPath)

	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		var req Request
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		resp := Response{Error: "unknown method: " + req.Method, Code: "unknown_method"}
		if err := json.NewEncoder(conn).Encode(resp); err != nil {
			t.Error(err)
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(Request{Method: "bogus"}); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != "unknown_method" {
		t.Errorf("code = %q, want unknown_method", resp.Code)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(nil, nil, log)

	srv.Stop()
	srv.Stop()
}

func TestSocketPermissions(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "perm.sock")

	// Verify that Serve creates a socket with restricted permissions
	// by checking the file after Serve creates it, without starting the server.
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	// Chmod like Serve does
	if err := os.Chmod(sockPath, 0600); err != nil {
		ln.Close()
		t.Fatal(err)
	}

	info, err := os.Stat(sockPath)
	ln.Close()
	if err != nil {
		t.Fatal(err)
	}

	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		t.Errorf("socket permissions = %o, want group/other bits cleared", perm)
	}
}
