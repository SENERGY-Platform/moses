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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/mgocompat"
)

// The fakes below implement the real interfaces rather than mocking single
// calls, so the assertions are about stored values and published events.

// ctxBudget is what a fake recorded about the context it was called with: how
// much time was left on it and whether it was already spent. It is how the
// context tests tell the series budget of one start from the store budget of
// the same start, which is a difference no return value shows.
type ctxBudget struct {
	remaining   time.Duration
	hasDeadline bool
	err         error
}

func budgetOf(ctx context.Context) ctxBudget {
	result := ctxBudget{err: ctx.Err()}
	if deadline, ok := ctx.Deadline(); ok {
		result.hasDeadline = true
		result.remaining = time.Until(deadline)
	}
	return result
}

type fakeEnvironments struct {
	mux    sync.Mutex
	stored map[string]domain.Environment
	allErr error
	getErr error
	gets   int

	// getFailures is how many of the next Get calls fail with failure before
	// the store answers normally again - a database blip in the middle of a
	// reload, which the caller has to survive rather than give up on.
	getFailures int
	failure     error

	// getDelay slows every Get down, which is what makes a reload long enough
	// for a test to see whether anything spawns a second one meanwhile.
	getDelay time.Duration
}

// failGets makes the next count Get calls fail. Called before the runtime
// starts, or from a test goroutine while it runs; the counter is guarded by
// the same mutex Get takes.
func (this *fakeEnvironments) failGets(count int, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.getFailures = count
	this.failure = err
}

// getCount is how many Get calls the store has answered, failures included.
func (this *fakeEnvironments) getCount() int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.gets
}

func newFakeEnvironments(envs ...domain.Environment) *fakeEnvironments {
	result := &fakeEnvironments{stored: map[string]domain.Environment{}}
	for _, env := range envs {
		result.stored[env.Id] = env
	}
	return result
}

func (this *fakeEnvironments) Put(ctx context.Context, env domain.Environment) (int64, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	env.Version = this.stored[env.Id].Version + 1
	this.stored[env.Id] = env
	return env.Version, nil
}

// PutIfVersion is here for the interface; the runtime never writes a definition.
func (this *fakeEnvironments) PutIfVersion(ctx context.Context, env domain.Environment, expectedVersion int64) (int64, error) {
	this.mux.Lock()
	stored, exists := this.stored[env.Id]
	this.mux.Unlock()
	if !exists || stored.Version != expectedVersion {
		return 0, &repo.VersionConflictError{
			Id: env.Id, Expected: expectedVersion, Stored: stored.Version, Gone: !exists,
		}
	}
	return this.Put(ctx, env)
}

func (this *fakeEnvironments) Get(ctx context.Context, id string) (domain.Environment, error) {
	this.mux.Lock()
	delay := this.getDelay
	this.mux.Unlock()
	//outside the mutex, so that concurrent reads really do overlap instead of
	//queueing behind each other
	if delay > 0 {
		time.Sleep(delay)
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	this.gets++
	if this.getErr != nil {
		return domain.Environment{}, this.getErr
	}
	if this.getFailures > 0 {
		this.getFailures--
		return domain.Environment{}, this.failure
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
	// loadBudgets records the context of the last Load per environment, which is
	// what pins that the state read runs on a budget of its own rather than on
	// what the series load left over.
	loadBudgets map[string]ctxBudget
	// ops records the writes in order, so that a test can tell "saved and then
	// deleted" from "deleted and then saved again" - the second one leaves the
	// state document of a deleted environment behind forever.
	ops []string
}

func newFakeStates() *fakeStates {
	return &fakeStates{stored: map[string]repo.RuntimeState{}, loads: map[string]int{},
		loadBudgets: map[string]ctxBudget{}}
}

func (this *fakeStates) Load(ctx context.Context, environmentId string) (repo.RuntimeState, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.loads[environmentId]++
	this.loadBudgets[environmentId] = budgetOf(ctx)
	//a real store fails on a spent context, and the fake has to as well: a
	//state read handed a context the fetches before it used up is exactly the
	//failure that leaves an environment unstarted
	if err := ctx.Err(); err != nil {
		return repo.RuntimeState{}, err
	}
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

// savesOf counts the writes of one environment, which is how a test tells a
// flusher that keeps running from one that got stuck on another environment.
func (this *fakeStates) savesOf(environmentId string) int {
	this.mux.Lock()
	defer this.mux.Unlock()
	count := 0
	for _, saved := range this.saves {
		if saved.state.EnvironmentId == environmentId {
			count++
		}
	}
	return count
}

func (this *fakeStates) loadBudgetFor(environmentId string) ctxBudget {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.loadBudgets[environmentId]
}

func (this *fakeStates) deletedIds() []string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]string{}, this.deleted...)
}

// eventClock hands out increasing numbers to more than one fake, so a test can
// compare what the store wrote with what the publisher sent: the two happen on
// different goroutines, and a wall clock reading of each would not order them.
// Nil safe, because most tests need no order between the two.
type eventClock struct {
	mux sync.Mutex
	n   int64
}

func (this *eventClock) next() int64 {
	if this == nil {
		return 0
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	this.n++
	return this.n
}

// historyJobCheckpoint is one Checkpoint call as the fake recorded it.
type historyJobCheckpoint struct {
	environmentId string
	progress      repo.HistoryJobProgress
}

// fakeHistoryJobs is the history job store in memory. It keeps every call, so a
// test can count the checkpoints of a run and read what the last one stored.
//
// Records and progress are copied through bson with the registry the real store
// uses, which is what makes the fake usable for a resume test: a run that keeps
// mutating its maps after a checkpoint cannot change what is stored, and the
// values come back in the types mongodb would hand back, timestamps at
// millisecond precision included.
type fakeHistoryJobs struct {
	mux         sync.Mutex
	stored      map[string]repo.HistoryJobRecord
	saves       []repo.HistoryJobRecord
	checkpoints []historyJobCheckpoint
	deleted     []string

	// saveSeqs is the sequence number of every Save, in lockstep with saves and
	// zero for one that failed: it says when a record became readable, which is
	// what a test comparing the store against the publisher needs.
	saveSeqs []int64

	// saveBudgets is the deadline every Save was called with, in lockstep with
	// saves: the terminal write of a run happens with the lifecycle mutex held,
	// so how long one attempt of it may take is part of the behaviour.
	saveBudgets []time.Duration
	clock       *eventClock

	saveErr       error
	checkpointErr error
	loadErr       error
	runningErr    error

	// saveErrIf decides per Save whether it fails, which saveErr alone cannot:
	// a test that lets the abort's own write through and fails only the terminal
	// one needs the record and the number of the call, counted from one.
	saveErrIf func(record repo.HistoryJobRecord, n int) error

	// saveDelay is how long a Save takes before the record is readable,
	// runningGate, when set, holds Running until it is closed, and resumeGate
	// does the same for the Save that counts a resume - the window between an
	// environment being built and its run taking it over. All three are how a
	// test widens a window in the start that is otherwise microseconds long.
	// Written before the runtime starts and read under the mutex.
	saveDelay   time.Duration
	runningGate chan struct{}
	resumeGate  chan struct{}

	// onCheckpoint is called with the number of the checkpoint, counted from
	// one, after it has been stored: a hook that stops the run therefore leaves
	// the boundary it fired at persisted, which is what a resume continues from.
	// It is set before the run starts and read under the mutex, but called
	// without it, so a hook may call back into the fake.
	onCheckpoint func(n int) error
}

func newFakeHistoryJobs() *fakeHistoryJobs {
	return &fakeHistoryJobs{stored: map[string]repo.HistoryJobRecord{}}
}

func (this *fakeHistoryJobs) Save(ctx context.Context, record repo.HistoryJobRecord) error {
	copied, err := copyHistoryJobRecord(record)
	if err != nil {
		return err
	}
	budget := time.Duration(0)
	if deadline, bounded := ctx.Deadline(); bounded {
		budget = time.Until(deadline)
	}
	this.mux.Lock()
	delay := this.saveDelay
	number := len(this.saves) + 1
	decide := this.saveErrIf
	failure := this.saveErr
	gate := this.resumeGate
	this.mux.Unlock()
	if gate != nil && copied.Resumes > 0 {
		//the write of a resume count, held until the test lets it through: the
		//environment is built by then and the run has not taken it over yet
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if delay > 0 {
		//the record becomes readable only after this, which is what makes the
		//sequence number below say when the write landed rather than when it began
		time.Sleep(delay)
	}
	if failure == nil && decide != nil {
		failure = decide(copied, number)
	}

	this.mux.Lock()
	defer this.mux.Unlock()
	this.saves = append(this.saves, copied)
	this.saveBudgets = append(this.saveBudgets, budget)
	if failure != nil {
		this.saveSeqs = append(this.saveSeqs, 0)
		return failure
	}
	//set by the store, as the real one does
	copied.UpdatedAtUnix = time.Now().Unix()
	this.stored[record.EnvironmentId] = copied
	this.saveSeqs = append(this.saveSeqs, this.clock.next())
	return nil
}

func (this *fakeHistoryJobs) Checkpoint(ctx context.Context, environmentId string, progress repo.HistoryJobProgress) error {
	copied, err := copyHistoryJobProgress(progress)
	if err != nil {
		return err
	}
	this.mux.Lock()
	this.checkpoints = append(this.checkpoints, historyJobCheckpoint{environmentId: environmentId, progress: copied})
	number := len(this.checkpoints)
	hook := this.onCheckpoint
	stored, exists := this.stored[environmentId]
	failure := this.checkpointErr
	if failure == nil && exists {
		//the same fields the real Checkpoint sets, and no other: a checkpoint
		//never rewrites the definition of the run
		stored.Published = copied.Published
		stored.Failed = copied.Failed
		stored.LastError = copied.LastError
		if !copied.Checkpoint.Position.IsZero() {
			//the same rule as the real store: a checkpoint without an instant is
			//not a chunk boundary, so the stored one stays
			checkpoint := copied.Checkpoint
			stored.Checkpoint = &checkpoint
		}
		if copied.Position.IsZero() {
			stored.Position = nil
		} else {
			position := copied.Position
			stored.Position = &position
		}
		if copied.AtBoundary {
			//the same rule as the real store: a run that reached a boundary has
			//got going, so the resume count starts over
			stored.Resumes = 0
		}
		stored.UpdatedAtUnix = time.Now().Unix()
		this.stored[environmentId] = stored
	}
	this.mux.Unlock()

	if hook != nil {
		if hookErr := hook(number); hookErr != nil {
			return hookErr
		}
	}
	if failure != nil {
		return failure
	}
	if !exists {
		return fmt.Errorf("%w: history job of %v", repo.ErrNotFound, environmentId)
	}
	return nil
}

func (this *fakeHistoryJobs) Load(ctx context.Context, environmentId string) (repo.HistoryJobRecord, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.loadErr != nil {
		return repo.HistoryJobRecord{}, this.loadErr
	}
	stored, ok := this.stored[environmentId]
	if !ok {
		return repo.HistoryJobRecord{}, fmt.Errorf("%w: history job of %v", repo.ErrNotFound, environmentId)
	}
	//copied and initialised exactly as the real Load hands a record out, so a
	//resume driven by the fake meets the same record it would in production
	result, err := copyHistoryJobRecord(stored)
	if err != nil {
		return repo.HistoryJobRecord{}, err
	}
	result.Checkpoint.Initialise()
	return result, nil
}

// Running returns the running records in environment id order: nothing may
// depend on the order, and a random one would make a failure flaky.
func (this *fakeHistoryJobs) Running(ctx context.Context) ([]repo.HistoryJobRecord, error) {
	this.mux.Lock()
	gate := this.runningGate
	this.mux.Unlock()
	if gate != nil {
		//a store that answers slowly, which is what widens the window between the
		//environments being built and their runs being resumed
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.runningErr != nil {
		return nil, this.runningErr
	}
	ids := []string{}
	for id, record := range this.stored {
		if record.State == repo.HistoryJobRunning {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := []repo.HistoryJobRecord{}
	for _, id := range ids {
		record, err := copyHistoryJobRecord(this.stored[id])
		if err != nil {
			return nil, err
		}
		record.Checkpoint.Initialise()
		result = append(result, record)
	}
	return result, nil
}

func (this *fakeHistoryJobs) Delete(ctx context.Context, environmentId string) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.deleted = append(this.deleted, environmentId)
	delete(this.stored, environmentId)
	return nil
}

// recordFor is what a test reads to see the stored run.
func (this *fakeHistoryJobs) recordFor(environmentId string) (repo.HistoryJobRecord, bool) {
	this.mux.Lock()
	defer this.mux.Unlock()
	stored, ok := this.stored[environmentId]
	return stored, ok
}

// checkpointCount counts every Checkpoint call, the refused ones included.
func (this *fakeHistoryJobs) checkpointCount() int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return len(this.checkpoints)
}

// checkpointsOf returns the progress of every checkpoint of one environment, in
// call order.
func (this *fakeHistoryJobs) checkpointsOf(environmentId string) []repo.HistoryJobProgress {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []repo.HistoryJobProgress{}
	for _, call := range this.checkpoints {
		if call.environmentId == environmentId {
			result = append(result, call.progress)
		}
	}
	return result
}

// savesOf returns every record written by Save for one environment, in call
// order, so a test can tell the write at the start from the one at the end.
func (this *fakeHistoryJobs) savesOf(environmentId string) []repo.HistoryJobRecord {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []repo.HistoryJobRecord{}
	for _, record := range this.saves {
		if record.EnvironmentId == environmentId {
			result = append(result, record)
		}
	}
	return result
}

// saveSeqOfState is when the first successful Save of one environment that
// stored the given state became readable, or 0 for none. It is comparable with
// the sequence of a published event where both fakes share an eventClock.
func (this *fakeHistoryJobs) saveSeqOfState(environmentId string, state HistoryState) int64 {
	this.mux.Lock()
	defer this.mux.Unlock()
	for i, record := range this.saves {
		if record.EnvironmentId != environmentId || record.State != string(state) {
			continue
		}
		if i < len(this.saveSeqs) && this.saveSeqs[i] != 0 {
			return this.saveSeqs[i]
		}
	}
	return 0
}

// saveBudgetsOfState is the deadline every Save of one environment that carried
// the given state was called with, failed attempts included.
func (this *fakeHistoryJobs) saveBudgetsOfState(environmentId string, state HistoryState) []time.Duration {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []time.Duration{}
	for i, record := range this.saves {
		if record.EnvironmentId != environmentId || record.State != string(state) {
			continue
		}
		if i < len(this.saveBudgets) {
			result = append(result, this.saveBudgets[i])
		}
	}
	return result
}

func (this *fakeHistoryJobs) deletedJobIds() []string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]string{}, this.deleted...)
}

func copyHistoryJobRecord(record repo.HistoryJobRecord) (repo.HistoryJobRecord, error) {
	result := repo.HistoryJobRecord{}
	err := copyThroughBson(record, &result)
	return result, err
}

func copyHistoryJobProgress(progress repo.HistoryJobProgress) (repo.HistoryJobProgress, error) {
	result := repo.HistoryJobProgress{}
	err := copyThroughBson(progress, &result)
	return result, err
}

// copyThroughBson is the copy the real store makes when it writes and reads a
// document, mgo compatible registry included.
func copyThroughBson(from interface{}, into interface{}) error {
	encoded, err := bson.MarshalWithRegistry(mgocompat.Registry, from)
	if err != nil {
		return fmt.Errorf("unable to encode %T: %w", from, err)
	}
	return bson.UnmarshalWithRegistry(mgocompat.Registry, encoded, into)
}

type publishedEvent struct {
	deviceRef  string
	serviceRef string
	value      interface{}
	// at is the instant the event claims to have been taken at. For a live
	// publish it is the moment of the call, for a backfilled one the historical
	// instant, which is what the backfill tests assert on.
	at time.Time
	// live tells the two apart without having to reason about the clock.
	live bool
	// seq is the publisher's eventClock reading, 0 when no clock is set: it is
	// what orders a reading against a write of another fake.
	seq int64
}

type fakePublisher struct {
	mux    sync.Mutex
	events []publishedEvent
	err    error

	// shapeErr is what TimeShapeOf reports per service ref; the zero value (no
	// entry) means the service takes a timestamp. A test that wants the
	// ordinary "no time path" case puts devices.ErrNoTimePath in here.
	shapeErr map[string]error

	// failAt fails exactly the publishes whose instant it returns true for, so a
	// test can make one reading of a backfill fail without failing the rest.
	failAt func(at time.Time) error

	// gate, when set, holds every timestamped publish until it is closed. It is
	// how a test keeps a job running long enough to assert something about it.
	// Written once before the runtime starts and never again, so reading it
	// outside the mutex is not a race.
	gate chan struct{}

	// latency, when set, is how long one timestamped publish takes. It is how a
	// test makes the workers of the publish pool finish in an order other than
	// the one they were given. Written once before the runtime starts, like gate.
	latency func() time.Duration

	// inFlight and peak count the timestamped publishes that are running at the
	// same time, which is how a test tells a pooled path from a synchronous one:
	// without the pool the peak is one.
	inFlight int
	peak     int

	// clock, when set, stamps every event with a sequence number the fake store
	// shares, so a test can order a live reading against a stored record.
	clock *eventClock
}

func (this *fakePublisher) PublishEvent(externalDeviceRef string, externalServiceRef string, value interface{}) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.events = append(this.events, publishedEvent{
		deviceRef: externalDeviceRef, serviceRef: externalServiceRef, value: value,
		at: time.Now(), live: true, seq: this.clock.next(),
	})
	return this.err
}

func (this *fakePublisher) PublishEventAt(externalDeviceRef string, externalServiceRef string, value interface{}, at time.Time) error {
	if this.gate != nil {
		//before the lock: a test that reads the events while the job is held
		//would otherwise block on it
		<-this.gate
	}
	this.enter()
	defer this.leave()
	//marshalled the way the connector does, outside every lock of the runtime:
	//a reading that is still the map in the environment state races here
	_, _ = json.Marshal(value)
	if this.latency != nil {
		//before the lock as well, or the publishes would serialise on the fake
		//and no test could observe the pool overlapping them
		time.Sleep(this.latency())
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.failAt != nil {
		if err := this.failAt(at); err != nil {
			return err
		}
	}
	this.events = append(this.events, publishedEvent{
		deviceRef: externalDeviceRef, serviceRef: externalServiceRef, value: value, at: at,
		seq: this.clock.next(),
	})
	return this.err
}

func (this *fakePublisher) enter() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.inFlight++
	if this.inFlight > this.peak {
		this.peak = this.inFlight
	}
}

func (this *fakePublisher) leave() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.inFlight--
}

// peakConcurrency is the most timestamped publishes that ever ran at once.
func (this *fakePublisher) peakConcurrency() int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.peak
}

func (this *fakePublisher) TimeShapeOf(externalDeviceRef string, externalServiceRef string) (devices.TimeShape, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if err, known := this.shapeErr[externalServiceRef]; known {
		return devices.TimeShape{}, err
	}
	return devices.TimeShape{
		RootName:     "root",
		ValuePath:    []string{"value"},
		TimePath:     []string{"time"},
		TimeEncoding: devices.TimeAsUnixMilliseconds,
	}, nil
}

// failWith makes every further publish report err, and every event that was
// attempted is still recorded - so a test can count the attempts a channel made
// while the platform refused them. Set under the mutex, because unlike gate and
// failAt it is changed while the runtime is running.
func (this *fakePublisher) failWith(err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.err = err
}

// backfilled returns the timestamped events of one channel in the order they
// were published, which is the order the assertions are written in.
func (this *fakePublisher) backfilled(serviceRef string) []publishedEvent {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []publishedEvent{}
	for _, event := range this.events {
		if event.serviceRef == serviceRef && !event.live {
			result = append(result, event)
		}
	}
	return result
}

// liveEvents and timestampedEvents split what the publisher saw: a live reading
// of the present against one of a run or a job, which is the difference a test
// about the handover turns on.
func (this *fakePublisher) liveEvents() []publishedEvent {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []publishedEvent{}
	for _, event := range this.events {
		if event.live {
			result = append(result, event)
		}
	}
	return result
}

func (this *fakePublisher) timestampedEvents() []publishedEvent {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []publishedEvent{}
	for _, event := range this.events {
		if !event.live {
			result = append(result, event)
		}
	}
	return result
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

// testPublishPool is the pool a test needs when it drives one backfill channel
// directly rather than through a job. It closes with the test, so a test that
// fails halfway still leaves no worker behind.
func testPublishPool(t *testing.T, rt *Runtime) *publishPool {
	t.Helper()
	pool := newPublishPool(context.Background(), rt.publishWorkers, rt.backfillPublisher())
	t.Cleanup(pool.Close)
	return pool
}

// startRuntime builds a runtime on the fakes and stops it when the test ends.
func startRuntime(t *testing.T, cfg config.Config, envs *fakeEnvironments, states *fakeStates, publisher *fakePublisher) *Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := newRuntime(cfg, envs, states, nil, newFakeHistoryJobs(), publisher)
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
