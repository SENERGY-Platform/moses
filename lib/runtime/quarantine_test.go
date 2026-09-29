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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// An environment whose stored state carries a crash-brake quarantine is not
// started, reports why through QuarantineOf, and is brought back by a reload,
// which clears the quarantine in the store.
func TestQuarantinedEnvironmentIsNotStartedUntilReloaded(t *testing.T) {
	const envId = "env-quarantined"
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), "moses.service.send(1);"))
	envs := newFakeEnvironments(def)
	states := newFakeStates()
	states.stored[envId] = repo.RuntimeState{
		EnvironmentId: envId,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
		Quarantine:    &repo.Quarantine{Reason: "a script run ended the process with a fatal error", Channel: "ch-1", AtUnix: 1},
	}
	rt := startRuntime(t, testConfig(time.Hour), envs, states, &fakePublisher{})

	if q := rt.QuarantineOf(envId); q == nil || q.Channel != "ch-1" {
		t.Fatalf("expected the environment to report its quarantine, got %#v", q)
	}
	rt.mux.RLock()
	_, running := rt.envs[envId]
	rt.mux.RUnlock()
	if running {
		t.Fatal("a quarantined environment must not be started")
	}

	rt.Reload(envId)

	if q := rt.QuarantineOf(envId); q != nil {
		t.Fatalf("a reload has to lift the quarantine, still got %#v", q)
	}
	rt.mux.RLock()
	_, running = rt.envs[envId]
	rt.mux.RUnlock()
	if !running {
		t.Fatal("a reload has to start the environment the quarantine held back")
	}
	if stored := states.stored[envId]; stored.Quarantine != nil {
		t.Fatal("a reload has to clear the quarantine in the store")
	}
}

func isRunning(rt *Runtime, id string) bool {
	rt.mux.RLock()
	defer rt.mux.RUnlock()
	_, running := rt.envs[id]
	return running
}

// A boot decision the store failed to persist is handed over in memory and still
// keeps the environment from starting, although its stored state is unmarked.
func TestAQuarantineOnlyInMemoryHoldsTheEnvironment(t *testing.T) {
	const envId = "env-quarantine-memory"
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), "moses.service.send(1);"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	rt.SetQuarantines(map[string]*repo.Quarantine{envId: {Reason: "boom", Channel: "ch-1", AtUnix: 1}})
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	if isRunning(rt, envId) || rt.QuarantineOf(envId) == nil {
		t.Fatal("an in-memory quarantine must keep the environment from starting")
	}
	rt.Reload(envId)
	if !isRunning(rt, envId) || rt.QuarantineOf(envId) != nil {
		t.Fatal("a reload has to lift the in-memory quarantine")
	}
}

// A reload whose quarantine clear fails in the store, on the write or the read,
// leaves the environment quarantined in memory and stopped, so memory and store
// never disagree.
func TestAFailedClearKeepsTheQuarantine(t *testing.T) {
	const envId = "env-quarantine-clear"
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), "moses.service.send(1);"))
	states := newFakeStates()
	states.stored[envId] = repo.RuntimeState{
		EnvironmentId: envId,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
		Quarantine:    &repo.Quarantine{Reason: "boom", Channel: "ch-1", AtUnix: 1},
	}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(def), states, &fakePublisher{})
	states.mux.Lock()
	states.saveErr = errors.New("write refused")
	states.mux.Unlock()
	rt.Reload(envId)
	if isRunning(rt, envId) || rt.QuarantineOf(envId) == nil {
		t.Fatal("a failed clear must leave the environment quarantined and stopped")
	}
	//a read error during the clear must not drop the in-memory entry either
	states.mux.Lock()
	states.saveErr = nil
	states.loadErr = errors.New("read refused")
	states.mux.Unlock()
	rt.Reload(envId)
	if isRunning(rt, envId) || rt.QuarantineOf(envId) == nil {
		t.Fatal("a failed read must leave the environment quarantined and stopped")
	}
	states.mux.Lock()
	states.loadErr = nil
	states.mux.Unlock()
	rt.Reload(envId)
	if !isRunning(rt, envId) || rt.QuarantineOf(envId) != nil {
		t.Fatal("a reload whose clear succeeds has to start the environment")
	}
}

// An id longer than a crash-brake slot is warned about once, not on every start.
func TestALongIdIsWarnedOnce(t *testing.T) {
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(), newFakeStates(), nil, newFakeHistoryJobs(), &fakePublisher{})
	long := strings.Repeat("x", 300)
	if !rt.warnLongId(long) || rt.warnLongId(long) {
		t.Fatal("the first call has to warn and the second not")
	}
	if !rt.warnLongId(long + "y") {
		t.Fatal("another id has to warn on its own")
	}
}
