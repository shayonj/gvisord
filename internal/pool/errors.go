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

import "errors"

// ErrCapacityExhausted is returned when all sentries are busy and the
// pending queue is full. The caller should retry later or route to
// another host.
var ErrCapacityExhausted = errors.New("capacity_exhausted: all sentries busy and pending queue full")

// ErrPoolClosed is returned when a pool is shutting down and can no longer
// accept or complete new executions.
var ErrPoolClosed = errors.New("pool_closed: pool is shutting down")
