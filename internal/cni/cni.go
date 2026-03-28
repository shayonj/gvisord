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

// Package cni manages per-sentry network namespaces using CNI plugins.
// Each sentry gets its own network namespace with a veth pair on a bridge,
// making it addressable from the host via an assigned IP.
package cni

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Config holds CNI configuration.
type Config struct {
	PluginDir string `json:"plugin_dir"`
	ConfigDir string `json:"config_dir"`
	Bridge    string `json:"bridge"`
	Subnet    string `json:"subnet"`
	NetnsDir  string `json:"netns_dir"`
}

// Manager creates and deletes per-sentry network namespaces.
type Manager struct {
	cfg Config
	log *slog.Logger
	mu  sync.Mutex
	nss map[string]string // id → IP
}

// NewManager creates a CNI manager.
func NewManager(cfg Config, log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg,
		log: log,
		nss: make(map[string]string),
	}
}

// EnsureConfig writes CNI bridge and loopback configuration files if they
// do not already exist.
func (m *Manager) EnsureConfig() error {
	if err := os.MkdirAll(m.cfg.ConfigDir, 0755); err != nil {
		return fmt.Errorf("creating CNI config dir: %w", err)
	}

	bridgePath := filepath.Join(m.cfg.ConfigDir, "10-gvisord-bridge.conf")
	if _, err := os.Stat(bridgePath); os.IsNotExist(err) {
		bridgeCfg := map[string]any{
			"cniVersion": "0.3.1",
			"name":       "gvisord-net",
			"type":       "bridge",
			"bridge":     m.cfg.Bridge,
			"isGateway":  true,
			"ipMasq":     true,
			"ipam": map[string]any{
				"type":   "host-local",
				"subnet": m.cfg.Subnet,
				"routes": []map[string]string{{"dst": "0.0.0.0/0"}},
			},
		}
		data, _ := json.MarshalIndent(bridgeCfg, "", "  ")
		if err := os.WriteFile(bridgePath, data, 0644); err != nil {
			return fmt.Errorf("writing bridge config: %w", err)
		}
		m.log.Info("wrote CNI bridge config", "path", bridgePath)
	}

	loopbackPath := filepath.Join(m.cfg.ConfigDir, "99-loopback.conf")
	if _, err := os.Stat(loopbackPath); os.IsNotExist(err) {
		loCfg := map[string]any{
			"cniVersion": "0.3.1",
			"name":       "lo",
			"type":       "loopback",
		}
		data, _ := json.MarshalIndent(loCfg, "", "  ")
		if err := os.WriteFile(loopbackPath, data, 0644); err != nil {
			return fmt.Errorf("writing loopback config: %w", err)
		}
		m.log.Info("wrote CNI loopback config", "path", loopbackPath)
	}

	return nil
}

// CreateNetNS creates a network namespace for a sentry and configures it
// with the bridge and loopback plugins. Returns the assigned IP address
// and the netns path.
func (m *Manager) CreateNetNS(id string) (ip string, netnsPath string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	netnsPath = filepath.Join(m.cfg.NetnsDir, id)

	if err := m.runIP("netns", "add", id); err != nil {
		return "", "", fmt.Errorf("creating netns %s: %w", id, err)
	}

	env := m.cniEnv(id, "ADD")

	bridgeCfg := filepath.Join(m.cfg.ConfigDir, "10-gvisord-bridge.conf")
	out, err := m.runCNI("bridge", bridgeCfg, append(env, "CNI_IFNAME=eth0"))
	if err != nil {
		m.cleanupNetNS(id)
		return "", "", fmt.Errorf("CNI bridge ADD: %w", err)
	}

	ip = m.parseIP(out)

	loopbackCfg := filepath.Join(m.cfg.ConfigDir, "99-loopback.conf")
	if _, err := m.runCNI("loopback", loopbackCfg, append(env, "CNI_IFNAME=lo")); err != nil {
		m.cleanupNetNS(id)
		return "", "", fmt.Errorf("CNI loopback ADD: %w", err)
	}

	m.nss[id] = ip
	m.log.Info("created netns", "id", id, "ip", ip, "path", netnsPath)
	return ip, netnsPath, nil
}

// DeleteNetNS removes a sentry's network namespace and releases its IP.
func (m *Manager) DeleteNetNS(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupNetNS(id)
	delete(m.nss, id)
}

// GetIP returns the IP address assigned to a sentry's network namespace.
func (m *Manager) GetIP(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ip, ok := m.nss[id]
	return ip, ok
}

// ValidatePlugins checks that the required CNI plugin binaries exist and
// are executable.
func (m *Manager) ValidatePlugins() error {
	for _, plugin := range []string{"bridge", "loopback"} {
		path := filepath.Join(m.cfg.PluginDir, plugin)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("CNI plugin %q not found at %s: %w", plugin, path, err)
		}
		if info.Mode()&0111 == 0 {
			return fmt.Errorf("CNI plugin %q at %s is not executable", plugin, path)
		}
	}
	return nil
}

func (m *Manager) cleanupNetNS(id string) {
	env := m.cniEnv(id, "DEL")

	loopbackCfg := filepath.Join(m.cfg.ConfigDir, "99-loopback.conf")
	if _, err := m.runCNI("loopback", loopbackCfg, append(env, "CNI_IFNAME=lo")); err != nil {
		m.log.Debug("CNI loopback DEL failed", "id", id, "err", err)
	}

	bridgeCfg := filepath.Join(m.cfg.ConfigDir, "10-gvisord-bridge.conf")
	if _, err := m.runCNI("bridge", bridgeCfg, append(env, "CNI_IFNAME=eth0")); err != nil {
		m.log.Debug("CNI bridge DEL failed", "id", id, "err", err)
	}

	if err := m.runIP("netns", "delete", id); err != nil {
		m.log.Debug("netns delete failed", "id", id, "err", err)
	}
	m.log.Debug("cleaned up netns", "id", id)
}

func (m *Manager) cniEnv(id, command string) []string {
	return []string{
		"CNI_PATH=" + m.cfg.PluginDir,
		"CNI_CONTAINERID=" + id,
		"CNI_COMMAND=" + command,
		"CNI_NETNS=" + filepath.Join(m.cfg.NetnsDir, id),
		"PATH=" + os.Getenv("PATH"),
	}
}

func (m *Manager) runCNI(plugin, configFile string, env []string) ([]byte, error) {
	pluginPath := filepath.Join(m.cfg.PluginDir, plugin)
	cfgData, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", configFile, err)
	}

	cmd := exec.Command(pluginPath)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(string(cfgData))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", plugin, err, out)
	}
	return out, nil
}

func (m *Manager) runIP(args ...string) error {
	cmd := exec.Command("ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

func (m *Manager) parseIP(cniOutput []byte) string {
	var result struct {
		IPs []struct {
			Address string `json:"address"`
		} `json:"ips"`
	}
	if err := json.Unmarshal(cniOutput, &result); err == nil && len(result.IPs) > 0 {
		addr := result.IPs[0].Address
		if idx := strings.Index(addr, "/"); idx > 0 {
			return addr[:idx]
		}
		return addr
	}
	return ""
}
