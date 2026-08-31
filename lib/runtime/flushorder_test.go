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
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/repo"
)

// slowStates is a store whose write takes measurable time and which does not
// serialise the writes itself, so a test can see whether the runtime does.
type slowStates struct {
	delay time.Duration

	// started is closed when the first write enters, so that a test can begin a
	// second one at a known moment rather than after a sleep.
	started     chan struct{}
	startedOnce sync.Once

	mux      sync.Mutex
	inFlight int
	overlap  int
	// finished is the meter value of every completed write, in completion order.
	finished []float64
}

func (this *slowStates) Load(ctx context.Context, environmentId string) (repo.RuntimeState, error) {
	return repo.RuntimeState{
		EnvironmentId: environmentId,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
	}, nil
}

func (this *slowStates) Save(ctx context.Context, state repo.RuntimeState) error {
	if this.started != nil {
		this.startedOnce.Do(func() { close(this.started) })
	}
	this.mux.Lock()
	this.inFlight++
	if this.inFlight > this.overlap {
		this.overlap = this.inFlight
	}
	this.mux.Unlock()

	time.Sleep(this.delay)

	this.mux.Lock()
	defer this.mux.Unlock()
	this.inFlight--
	value, _ := asFloat(state.Assets[testAssetId]["meter"])
	this.finished = append(this.finished, value)
	return nil
}

func (this *slowStates) Delete(ctx context.Context, environmentId string) error { return nil }

func (this *slowStates) result() (overlap int, finished []float64) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.overlap, append([]float64{}, this.finished...)
}

// TestTwoFlushesOfOneEnvironmentDoNotOverlap: the flusher and the handover of a
// history run can both flush the same environment. Two flushes that read two
// states and write them concurrently land in the store in whichever order the
// database finishes them, and the older one then stands.
func TestTwoFlushesOfOneEnvironmentDoNotOverlap(t *testing.T) {
	const id = "env-flush-order"
	store := &slowStates{delay: 200 * time.Millisecond, started: make(chan struct{})}
	//not started: the flusher would take part in the writes and the test is about
	//two callers of flush
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(historyTestEnvironment(id)), store, nil, &fakePublisher{})
	env := &environment{id: id, state: repo.RuntimeState{
		EnvironmentId: id,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{testAssetId: {"meter": 1.0}},
	}}
	env.dirty = true

	first := make(chan struct{})
	go func() {
		defer close(first)
		rt.flush(env)
	}()

	//the second state is newer, and its flush is started once the first write is
	//known to be on its way to the store
	<-store.started
	env.mux.Lock()
	env.state.Assets[testAssetId]["meter"] = 2.0
	env.dirty = true
	env.mux.Unlock()
	rt.flush(env)
	<-first

	overlap, finished := store.result()
	if overlap > 1 {
		t.Errorf("%d writes of one environment were in flight at once", overlap)
	}
	if len(finished) != 2 {
		t.Fatalf("expected two writes, got %v", finished)
	}
	if finished[len(finished)-1] != 2 {
		t.Errorf("the store was left holding %v, expected the newer state 2 to land last", finished)
	}
}
