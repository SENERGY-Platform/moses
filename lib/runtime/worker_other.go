//go:build !linux

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

import "syscall"

const (
	WorkerMemoryLimitEnv = "MOSES_WORKER_MEMORY_LIMIT"
	WorkerRlimitEnv      = "MOSES_WORKER_RLIMIT"
)

// the worker mode is linux only: off linux it runs without limits
func applyWorkerLimits() error { return nil }

func workerProcAttr() *syscall.SysProcAttr { return nil }
