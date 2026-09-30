/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package runtime

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"syscall"
)

// Worker memory limits, read by the worker before it runs any script.
const (
	WorkerMemoryLimitEnv = "MOSES_WORKER_MEMORY_LIMIT" // bytes; unset or 0 means none
	WorkerRlimitEnv      = "MOSES_WORKER_RLIMIT"       // "data" (default) or "as"
)

// applyWorkerLimits sets a hard rlimit and a Go soft limit at 80 % of it, so the
// collector works harder before an allocation fails with "out of memory".
func applyWorkerLimits() error {
	raw := os.Getenv(WorkerMemoryLimitEnv)
	if raw == "" || raw == "0" {
		return nil
	}
	limit, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %w", WorkerMemoryLimitEnv, err)
	}
	resource := syscall.RLIMIT_DATA
	if os.Getenv(WorkerRlimitEnv) == "as" {
		resource = syscall.RLIMIT_AS
	}
	if err := syscall.Setrlimit(resource, &syscall.Rlimit{Cur: limit, Max: limit}); err != nil {
		return err
	}
	debug.SetMemoryLimit(int64(limit / 10 * 8))
	return nil
}

// workerProcAttr kills the worker with its supervisor, which a pipe EOF alone
// would only do once the worker next reads.
func workerProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
