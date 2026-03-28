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

// Command gvisord is a daemon that manages partitioned pools of warm gVisor
// sentry processes for fast checkpoint/restore execution.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/shayonj/gvisord/internal/api"
	"github.com/shayonj/gvisord/internal/cgroup"
	"github.com/shayonj/gvisord/internal/cni"
	"github.com/shayonj/gvisord/internal/config"
	"github.com/shayonj/gvisord/internal/pool"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run", "execute", "complete", "status", "health", "drain":
			sock := "/run/gvisord/gvisord.sock"
			if v := os.Getenv("GVISORD_SOCKET"); v != "" {
				sock = v
			}
			clientMain(os.Args[1], sock, os.Args[2:])
			return
		}
	}

	var (
		configPath string
		logLevel   string
	)
	flag.StringVar(&configPath, "config", "", "path to config file (JSON, required)")
	flag.StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	var level slog.Level
	switch logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Error("failed to load config", "err", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		log.Error("invalid config", "err", err)
		os.Exit(1)
	}

	workloads := cfg.Workloads()
	log.Info("gvisord starting",
		"socket", cfg.Daemon.Socket,
		"templates", len(cfg.Templates),
		"workloads", len(workloads),
		"runsc", cfg.Runsc.Path,
	)
	for name, wl := range workloads {
		log.Info("workload configured", "name", name, "pool_size", wl.PoolSize, "rootfs", wl.Rootfs, "checkpoint", wl.Checkpoint)
	}

	if cfg.Daemon.PIDFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Daemon.PIDFile), 0755); err != nil {
			log.Warn("failed to create PID file directory", "path", filepath.Dir(cfg.Daemon.PIDFile), "err", err)
		}
		if err := os.WriteFile(cfg.Daemon.PIDFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644); err != nil {
			log.Warn("failed to write PID file", "path", cfg.Daemon.PIDFile, "err", err)
		}
		defer os.Remove(cfg.Daemon.PIDFile)
	}

	opts := pool.ManagerOpts{}

	// Initialize cgroup manager if resource classes are configured.
	if len(cfg.ResourceClasses) > 0 {
		cgMgr := cgroup.NewManager(cfg.Cgroup.BasePath, cfg.Cgroup.Prefix, log)
		if err := cgMgr.DetectV2(); err != nil {
			log.Warn("cgroup v2 not available (resource limits will not be enforced)", "err", err)
		} else {
			cgMgr.CleanupStaleSlices()
			classes := make([]cgroup.ResourceClass, len(cfg.ResourceClasses))
			for i, rc := range cfg.ResourceClasses {
				classes[i] = cgroup.ResourceClass{Name: rc.Name, CPUMillis: rc.CPUMillis, MemoryMB: rc.MemoryMB}
			}
			if err := cgMgr.EnsureSlices(classes); err != nil {
				log.Warn("failed to set up cgroup slices (resource limits will not be enforced)", "err", err)
			} else {
				opts.CgroupMgr = cgMgr
				log.Info("cgroup resource isolation enabled", "classes", len(classes))
			}
		}
	}

	// Initialize CNI manager if network mode is cni.
	if cfg.Network.Mode == "cni" {
		cniCfg := cni.Config{
			PluginDir: cfg.Network.PluginDir,
			ConfigDir: cfg.Network.ConfigDir,
			Bridge:    cfg.Network.Bridge,
			Subnet:    cfg.Network.Subnet,
			NetnsDir:  "/var/run/netns",
		}
		cniMgr := cni.NewManager(cniCfg, log)
		if err := cniMgr.ValidatePlugins(); err != nil {
			log.Warn("CNI plugins not available (networking will be disabled)", "err", err)
		} else if err := cniMgr.EnsureConfig(); err != nil {
			log.Warn("failed to write CNI config (networking will be disabled)", "err", err)
		} else {
			opts.CNIMgr = cniMgr
			log.Info("CNI networking enabled", "bridge", cfg.Network.Bridge, "subnet", cfg.Network.Subnet)
		}
	}

	mgr := pool.NewManager(cfg, log, opts)
	if err := mgr.Start(); err != nil {
		log.Error("failed to start pools", "err", err)
		os.Exit(1)
	}

	srv := api.New(mgr, cfg, log)
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(cfg.Daemon.Socket)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info("received signal, shutting down", "signal", sig)
	case err := <-errCh:
		if err != nil {
			log.Error("API server error", "err", err)
		}
	}

	log.Info("draining pools")
	mgr.Shutdown()
	srv.Stop()
	os.Remove(cfg.Daemon.Socket)
	log.Info("gvisord stopped")
}
