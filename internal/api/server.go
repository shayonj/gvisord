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

// Package api implements the gvisord control API over a Unix domain socket.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"os/user"
	"strconv"

	"github.com/shayonj/gvisord/internal/config"
	"github.com/shayonj/gvisord/internal/pool"
)

// Request is the envelope for all API requests.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// ExecuteParams are the parameters for the execute method.
type ExecuteParams struct {
	Workload   string `json:"workload"`
	Checkpoint string `json:"checkpoint,omitempty"`
}

// CompleteParams are the parameters for the complete method.
type CompleteParams struct {
	LeaseID string `json:"lease_id"`
}

// Response is the envelope for all API responses.
type Response struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
}

// Server serves the gvisord API over a Unix domain socket.
type Server struct {
	mgr      *pool.Manager
	cfg      *config.Config
	listener net.Listener
	log      *slog.Logger
	drainCh  chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

func New(mgr *pool.Manager, cfg *config.Config, log *slog.Logger) *Server {
	return &Server{
		mgr:     mgr,
		cfg:     cfg,
		log:     log,
		drainCh: make(chan struct{}),
	}
}

func (s *Server) Serve(socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return fmt.Errorf("creating socket directory: %w", err)
	}
	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	mode := os.FileMode(0600)
	if s.cfg != nil {
		if m := s.cfg.Daemon.SocketMode; m != "" {
			if parsed, err := strconv.ParseUint(m, 8, 32); err == nil {
				mode = os.FileMode(parsed)
			}
		}
	}
	if err := os.Chmod(socketPath, mode); err != nil {
		s.log.Warn("failed to set socket permissions", "err", err)
	}
	if s.cfg != nil && s.cfg.Daemon.SocketGroup != "" {
		grp, err := user.LookupGroup(s.cfg.Daemon.SocketGroup)
		if err != nil {
			s.log.Warn("failed to look up socket group", "group", s.cfg.Daemon.SocketGroup, "err", err)
		} else {
			gid, _ := strconv.Atoi(grp.Gid)
			if err := os.Chown(socketPath, -1, gid); err != nil {
				s.log.Warn("failed to chown socket", "group", s.cfg.Daemon.SocketGroup, "err", err)
			}
		}
	}
	s.listener = ln
	s.log.Info("API server listening", "socket", socketPath)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.drainCh:
				return nil
			default:
				s.log.Error("accept failed", "err", err)
				continue
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.drainCh)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.wg.Wait()
	})
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req Request
	if err := dec.Decode(&req); err != nil {
		if encodeErr := enc.Encode(Response{Error: fmt.Sprintf("invalid request: %v", err), Code: "bad_request"}); encodeErr != nil {
			s.log.Debug("failed to encode bad request response", "err", encodeErr)
		}
		return
	}

	s.log.Debug("API request", "method", req.Method)

	var resp Response
	switch req.Method {
	case "run":
		resp = s.handleRun(req.Params)
	case "execute":
		resp = s.handleExecute(req.Params)
	case "complete":
		resp = s.handleComplete(req.Params)
	case "status":
		resp = s.handleStatus()
	case "health":
		resp = s.handleHealth()
	case "drain":
		resp = s.handleDrain()
	default:
		resp = Response{Error: fmt.Sprintf("unknown method: %s", req.Method), Code: "unknown_method"}
	}

	if err := enc.Encode(resp); err != nil {
		s.log.Debug("failed to encode API response", "method", req.Method, "err", err)
	}
}

func (s *Server) handleRun(raw json.RawMessage) Response {
	var params pool.RunParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return Response{Error: fmt.Sprintf("invalid run params: %v", err), Code: "bad_request"}
		}
	}
	if params.Workload == "" {
		return Response{Error: "run requires 'workload' param", Code: "bad_request"}
	}
	if params.Script == "" && params.Command == "" {
		return Response{Error: "run requires 'script' or 'command' param", Code: "bad_request"}
	}

	result, err := s.mgr.Run(params)
	if err != nil {
		code := "run_failed"
		if errors.Is(err, pool.ErrCapacityExhausted) {
			code = "capacity_exhausted"
		}
		return Response{Error: err.Error(), Code: code}
	}
	return Response{Result: result}
}

func (s *Server) handleExecute(raw json.RawMessage) Response {
	var params ExecuteParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return Response{Error: fmt.Sprintf("invalid execute params: %v", err), Code: "bad_request"}
		}
	}
	if params.Workload == "" {
		return Response{Error: "execute requires 'workload' param", Code: "bad_request"}
	}

	result, err := s.mgr.Execute(params.Workload, params.Checkpoint)
	if err != nil {
		code := "execute_failed"
		if errors.Is(err, pool.ErrCapacityExhausted) {
			code = "capacity_exhausted"
		}
		return Response{Error: err.Error(), Code: code}
	}
	return Response{Result: result}
}

func (s *Server) handleComplete(raw json.RawMessage) Response {
	var params CompleteParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return Response{Error: fmt.Sprintf("invalid complete params: %v", err), Code: "bad_request"}
		}
	}
	if params.LeaseID == "" {
		return Response{Error: "complete requires 'lease_id' param", Code: "bad_request"}
	}

	if err := s.mgr.Complete(params.LeaseID); err != nil {
		return Response{Error: err.Error(), Code: "complete_failed"}
	}
	return Response{Result: map[string]any{"ok": true}}
}

func (s *Server) handleStatus() Response {
	return Response{Result: s.mgr.Status()}
}

func (s *Server) handleHealth() Response {
	return Response{Result: map[string]any{"healthy": s.mgr.Healthy()}}
}

func (s *Server) handleDrain() Response {
	s.log.Info("drain requested")
	go func() {
		s.mgr.Shutdown()
		s.Stop()
	}()
	return Response{Result: map[string]any{"ok": true}}
}
