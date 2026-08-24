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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// The fakes below implement the real interfaces rather than mocking single
// calls, so the assertions are about stored values and published events.

type fakeEnvironments struct {
	mux    sync.Mutex
	stored map[string]domain.Environment
	allErr error
	getErr error
	gets   int
}

func newFakeEnvironments(envs ...domain.Environment) *fakeEnvironments {
	result := &fakeEnvironments{stored: map[string]domain.Environment{}}
	for _, env := range envs {
		result.stored[env.Id] = env
	}
	return result
}

func (this *fakeEnvironments) Put(ctx context.Context, env domain.Environment) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.stored[env.Id] = env
	return nil
}

func (this *fakeEnvironments) Get(ctx context.Context, id string) (domain.Environment, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.gets++
	if this.getErr != nil {
		return domain.Environment{}, this.getErr
	}
	env, ok := this.stored[id]
	if !ok {
		return domain.Environment{}, fmt.Errorf("looking for %v: %w", id, repo.ErrNotFound)
	}
	return env, nil
}

func (this *fakeEnvironments) ListByOwner(ctx context.Context, owner string) ([]domain.Environment, error) {
	return nil, nil
}

// All returns the environments in id order: the runtime is allowed to depend on
// nothing about the order, and a random one would make a failure flaky.
func (this *fakeEnvironments) All(ctx context.Context) ([]domain.Environment, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.allErr != nil {
		return nil, this.allErr
	}
	ids := make([]string, 0, len(this.stored))
	for id := range this.stored {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]domain.Environment, 0, len(ids))
	for _, id := range ids {
		result = append(result, this.stored[id])
	}
	return result, nil
}

func (this *fakeEnvironments) Delete(ctx context.Context, id string) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	delete(this.stored, id)
	return nil
}

type savedState struct {
	state repo.RuntimeState
	// ctxErr is what the context handed to Save carried at the time of the call.
	// A cancelled context here means the runtime would have lost the write.
	ctxErr error
}

type fakeStates struct {
	mux     sync.Mutex
	stored  map[string]repo.RuntimeState
	saves   []savedState
	loads   map[string]int
	deleted []string
	loadErr error
	saveErr error
	// ops records the writes in order, so that a test can tell "saved and then
	// deleted" from "deleted and then saved again" - the second one leaves the
	// state document of a deleted environment behind forever.
	ops []string
}

func newFakeStates() *fakeStates {
	return &fakeStates{stored: map[string]repo.RuntimeState{}, loads: map[string]int{}}
}

func (this *fakeStates) Load(ctx context.Context, environmentId string) (repo.RuntimeState, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.loads[environmentId]++
	if this.loadErr != nil {
		return repo.RuntimeState{}, this.loadErr
	}
	stored, ok := this.stored[environmentId]
	if !ok {
		return repo.RuntimeState{
			EnvironmentId: environmentId,
			Context:       map[string]interface{}{},
			Zones:         map[string]map[string]interface{}{},
			Assets:        map[string]map[string]interface{}{},
		}, nil
	}
	return stored, nil
}

func (this *fakeStates) Save(ctx context.Context, state repo.RuntimeState) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.saves = append(this.saves, savedState{state: state, ctxErr: ctx.Err()})
	if this.saveErr != nil {
		return this.saveErr
	}
	this.ops = append(this.ops, "save:"+state.EnvironmentId)
	this.stored[state.EnvironmentId] = state
	return nil
}

func (this *fakeStates) Delete(ctx context.Context, environmentId string) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.deleted = append(this.deleted, environmentId)
	this.ops = append(this.ops, "delete:"+environmentId)
	delete(this.stored, environmentId)
	return nil
}

// lastOpFor returns "save", "delete" or "" for the last write that touched one
// environment.
func (this *fakeStates) lastOpFor(environmentId string) string {
	this.mux.Lock()
	defer this.mux.Unlock()
	for i := len(this.ops) - 1; i >= 0; i-- {
		switch this.ops[i] {
		case "save:" + environmentId:
			return "save"
		case "delete:" + environmentId:
			return "delete"
		}
	}
	return ""
}

func (this *fakeStates) savedFor(environmentId string) []savedState {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []savedState{}
	for _, save := range this.saves {
		if save.state.EnvironmentId == environmentId {
			result = append(result, save)
		}
	}
	return result
}

func (this *fakeStates) loadCount(environmentId string) int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.loads[environmentId]
}

func (this *fakeStates) deletedIds() []string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]string{}, this.deleted...)
}

type publishedEvent struct {
	deviceRef  string
	serviceRef string
	value      interface{}
}

type fakePublisher struct {
	mux    sync.Mutex
	events []publishedEvent
	err    error
}

func (this *fakePublisher) PublishEvent(externalDeviceRef string, externalServiceRef string, value interface{}) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.events = append(this.events, publishedEvent{deviceRef: externalDeviceRef, serviceRef: externalServiceRef, value: value})
	return this.err
}

func (this *fakePublisher) all() []publishedEvent {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]publishedEvent{}, this.events...)
}

func (this *fakePublisher) count() int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return len(this.events)
}

// forDevice returns the values published for one platform device, so that a test
// with two environments can tell them apart.
func (this *fakePublisher) forDevice(deviceRef string) []interface{} {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []interface{}{}
	for _, event := range this.events {
		if event.deviceRef == deviceRef {
			result = append(result, event.value)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// document builders
// ---------------------------------------------------------------------------

const (
	testZoneId  = "zone-1"
	testAssetId = "asset-1"
)

func deviceRefOf(envId string) string  { return "urn:infai:ses:device:" + envId }
func serviceRefOf(envId string) string { return "urn:infai:ses:service:" + envId }

func scriptChannel(id string, direction domain.Direction, interval int64, externalRef string, code string) domain.Channel {
	return domain.Channel{
		Id:              id,
		Name:            id,
		Direction:       direction,
		ExternalRef:     externalRef,
		IntervalSeconds: interval,
		Source:          domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: code}},
	}
}

// testEnvironment is one environment with one zone, one asset and the given
// channels. The external refs are derived from the environment id, so two test
// environments never share a platform device by accident.
func testEnvironment(id string, channels ...domain.Channel) domain.Environment {
	return domain.Environment{
		Id:      id,
		Name:    id,
		Type:    domain.IndustrialSite,
		Owner:   "test-owner",
		Context: map[string]interface{}{},
		Zones: []domain.Zone{{
			Id:            testZoneId,
			Name:          "hall",
			Type:          domain.ZoneHall,
			InitialStates: map[string]interface{}{},
			Assets: []domain.Asset{{
				Id:             testAssetId,
				Name:           "machine",
				Kind:           domain.AssetMachine,
				ExternalRef:    deviceRefOf(id),
				ExternalTypeId: "urn:infai:ses:device-type:test",
				InitialStates:  map[string]interface{}{},
				Channels:       channels,
			}},
		}},
	}
}

// testConfig keeps the js timeout generous: these tests care about ordering and
// state, and a script that waits for a barrier must not be killed for it.
func testConfig(flushInterval time.Duration) config.Config {
	return config.Config{
		JsTimeout:           5 * time.Second,
		StateFlushInterval:  flushInterval,
		ProtocolSegmentName: "payload",
	}
}

// startRuntime builds a runtime on the fakes and stops it when the test ends.
func startRuntime(t *testing.T, cfg config.Config, envs *fakeEnvironments, states *fakeStates, publisher *fakePublisher) *Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := newRuntime(cfg, envs, states, nil, publisher)
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("unable to start the runtime: %v", err)
	}
	t.Cleanup(rt.Stop)
	return rt
}

func waitFor(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}

// ---------------------------------------------------------------------------
// barrier: an http endpoint a script can block in
// ---------------------------------------------------------------------------

// barrier is how the concurrency tests observe overlap. otto has no sleep, but
// httpGet is part of the script surface, so a script can be parked in a request
// for as long as the test wants.
//
// It records the highest number of scripts that were inside it at the same time,
// and it releases everybody as soon as target of them have arrived. That makes
// the proof deterministic in both directions: "never more than one at a time"
// fails if two ever overlap, and "two at a time" fails by timeout if they are
// serialised.
type barrier struct {
	mux         sync.Mutex
	arrived     int
	inflight    int
	maxInflight int
	target      int
	hold        time.Duration
	release     chan struct{}
	server      *httptest.Server
}

func newBarrier(t *testing.T, target int, hold time.Duration) *barrier {
	result := &barrier{target: target, hold: hold, release: make(chan struct{})}
	result.server = httptest.NewServer(result)
	t.Cleanup(result.server.Close)
	return result
}

func (this *barrier) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	this.mux.Lock()
	this.arrived++
	this.inflight++
	if this.inflight > this.maxInflight {
		this.maxInflight = this.inflight
	}
	reached := this.arrived >= this.target
	this.mux.Unlock()

	if reached {
		//closed at most once: only the arrival that reaches the target closes it
		select {
		case <-this.release:
		default:
			close(this.release)
		}
	}
	select {
	case <-this.release:
	case <-time.After(this.hold):
	}

	this.mux.Lock()
	this.inflight--
	this.mux.Unlock()
	writer.Write([]byte("ok"))
}

func (this *barrier) url() string {
	return this.server.URL
}

func (this *barrier) stats() (arrived int, maxInflight int) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.arrived, this.maxInflight
}
