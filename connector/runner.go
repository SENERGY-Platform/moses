/*
 * Copyright 2018 InfAI (CC SES)
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

package connector

import (
	"log"
	"sync"
)

type StopCheckFunc func() bool

type RunnerHandlerFunc func(StopCheckFunc) error

type RunnerTask struct {
	mux  sync.RWMutex
	stop bool
}

func RunTask(handler RunnerHandlerFunc) (task *RunnerTask) {
	task = &RunnerTask{}
	go func() {
		err := handler(func() bool {
			task.mux.RLock()
			shouldStop := task.stop
			task.mux.RUnlock()
			return shouldStop
		})
		if err != nil {
			log.Println("ERROR: RunTask()", err)
		}
	}()
	return
}

func (t *RunnerTask) Stop() {
	t.mux.Lock()
	t.stop = true
	t.mux.Unlock()
}
