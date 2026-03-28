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

package pool

import (
	"fmt"
	"sync"
	"time"
)

// State represents the lifecycle state of a warm sentry.
type State int

const (
	StateReady    State = iota // pre-restored, waiting for acquire
	StateRunning               // acquired, container executing (leased)
	StateDraining              // kill+wait+reset+restore in progress
)

func (s State) String() string {
	switch s {
	case StateReady:
		return "ready"
	case StateRunning:
		return "running"
	case StateDraining:
		return "draining"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// Sentry is a single warm gVisor sentry process.
type Sentry struct {
	mu sync.Mutex

	ID            string
	PID           int
	RootDir       string
	BundleDir     string
	State         State
	Restores      int
	Created       time.Time
	LastReset     time.Time
	LastUsed      time.Time
	BaseRSSKB     int64
	IP            string // assigned via CNI, empty when network.mode != "cni"
	NetnsPath     string // network namespace path
	ResourceClass string // cgroup resource class name
	LeaseID       string // set when acquired, cleared on recycle
}

// SentryInfo is a snapshot of sentry state for the API.
type SentryInfo struct {
	ID       string  `json:"id"`
	PID      int     `json:"pid"`
	State    string  `json:"state"`
	Restores int     `json:"restores"`
	AgeSec   float64 `json:"age_s"`
	IdleSec  float64 `json:"idle_s"`
	RSSKB    int64   `json:"rss_kb"`
	FDs      int     `json:"fds"`
	IP       string  `json:"ip,omitempty"`
}

func (s *Sentry) Info() SentryInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	idle := float64(0)
	if !s.LastUsed.IsZero() {
		idle = time.Since(s.LastUsed).Seconds()
	}
	return SentryInfo{
		ID:       s.ID,
		PID:      s.PID,
		State:    s.State.String(),
		Restores: s.Restores,
		AgeSec:   time.Since(s.Created).Seconds(),
		IdleSec:  idle,
		RSSKB:    ReadRSSKB(s.PID),
		FDs:      CountFDs(s.PID),
		IP:       s.IP,
	}
}

// HealthCheck verifies the sentry is within resource limits.
func (s *Sentry) HealthCheck(maxRestores int, maxAge time.Duration, maxRSSGrowthKB int64, maxFDs int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Restores >= maxRestores {
		return fmt.Errorf("restore count %d >= limit %d", s.Restores, maxRestores)
	}
	if time.Since(s.Created) > maxAge {
		return fmt.Errorf("age %s > limit %s", time.Since(s.Created).Round(time.Second), maxAge)
	}
	rss := ReadRSSKB(s.PID)
	if rss > 0 && s.BaseRSSKB > 0 && (rss-s.BaseRSSKB) > maxRSSGrowthKB {
		return fmt.Errorf("RSS growth %dKB > limit %dKB", rss-s.BaseRSSKB, maxRSSGrowthKB)
	}
	fds := CountFDs(s.PID)
	if fds > maxFDs {
		return fmt.Errorf("FD count %d > limit %d", fds, maxFDs)
	}
	return nil
}
