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

// Package runtime replaces the change routines of lib/state and differs in three
// ways that are the point of the rewrite:
//
//   - Reload and Remove affect one environment, where the legacy runtime
//     restarted every ticker of every world on every edit.
//   - One mutex per environment, so two environments run in parallel.
//   - State is kept in memory and flushed on an interval, not written whole on
//     every state.set().
//
// The javascript surface is deliberately unchanged, see jsapi.go.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/formula"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/timeseries"
	"github.com/SENERGY-Platform/moses/lib/util"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
)

// defaultJsTimeout and defaultStateFlushInterval are fallbacks for a
// configuration that leaves the value at zero. A zero js timeout would
// interrupt every script in its first statement and a zero flush interval
// would make a ticker panic, so neither may be taken at face value.
const (
	defaultJsTimeout          = 2 * time.Second
	defaultStateFlushInterval = 5 * time.Second
)

// storeTimeout bounds one database call made by the runtime itself.
const storeTimeout = 30 * time.Second

// seriesLoadTimeout bounds the whole series loading phase of one environment
// start (every gridfs read and platform fetch together). It is far larger than
// storeTimeout because a fetch may legitimately take minutes, and it is bounded
// rather than open ended because startEnvironment runs with lifecycle held, so
// a stalled fetch must not block every other Start, Stop, Reload and Remove.
const seriesLoadTimeout = 10 * time.Minute

// Runtime runs every environment of the store.
type Runtime struct {
	config        config.Config
	environments  repo.Environments
	states        repo.States
	datasets      repo.Datasets
	fetcher       seriesFetcher
	ownerToken    func(userId string) (string, error)
	publisher     eventPublisher
	stateLogger   deviceStateLogger
	jsTimeout     time.Duration
	flushInterval time.Duration

	// lifecycle serialises Start, Stop, Reload and Remove against each other, so
	// that two api calls arriving at once cannot interleave their stop and start
	// phases. It is never held while a script runs.
	lifecycle sync.Mutex

	// mux guards envs, commands and devices, and the gen field of an
	// environment. It is held only for lookups, never across a script run or a
	// database call.
	mux      sync.RWMutex
	envs     map[string]*environment
	commands map[commandKey]*runningChannel
	devices  map[string]*environment

	// backfills holds one job per environment, guarded by backfillMux;
	// backfillWorkers counts the goroutines running them so that Stop waits for
	// the publish in flight instead of leaving it half sent.
	//
	// The registry is memory only and a job is not resumable: after a restart
	// nothing is known about what already reached timescale, which is what
	// BackfillStatusOf reports.
	backfillMux      sync.Mutex
	backfills        map[string]*backfillJob
	backfillWorkers  sync.WaitGroup
	backfillsStopped bool

	// histories holds one history run per environment, guarded by historyMux, and
	// historyWorkers counts the engine phase of the running ones so that Stop
	// waits for it. The registry is memory only and a run is not resumable, the
	// same way a backfill is not.
	//
	// A run and a backfill of the same environment exclude each other, and both
	// registries are consulted for that: historyMux is always taken before
	// backfillMux, never the other way round.
	historyMux       sync.Mutex
	histories        map[string]*historyJob
	historyWorkers   sync.WaitGroup
	historiesStopped bool

	// historyEngine is what a run executes. It is a field so a test of the
	// lifecycle can inject an engine of its own instead of simulating a window.
	historyEngine historyEngineFunc

	ctx     context.Context
	cancel  context.CancelFunc
	flusher sync.WaitGroup
	running bool
}

// runningChannel is an entry of the command index: the channel to execute plus
// the environment and the definition generation it belongs to.
type runningChannel struct {
	env     *environment
	gen     *generation
	binding channelBinding
}

func New(config config.Config, environments repo.Environments, states repo.States, datasets repo.Datasets, connector *platform_connector_lib.Connector, stateLogger deviceStateLogger) *Runtime {
	result := newRuntime(config, environments, states, datasets, &connectorPublisher{
		connector:   connector,
		segmentName: config.ProtocolSegmentName,
	})
	result.stateLogger = stateLogger
	if config.TimescaleWrapperUrl != "" {
		result.fetcher = timeseries.New(config.TimescaleWrapperUrl)
	}
	//the fetch runs with the owner's exchanged token, so the wrapper checks
	//the owner's real permissions instead of trusting this service's account
	result.ownerToken = func(userId string) (string, error) {
		token, err := connector.Security().GetCachedUserToken(userId, model.RemoteInfo{})
		return string(token), err
	}
	return result
}

// seriesFetcher is what the platform origin needs from the timescale-wrapper.
// ctx is the load's budget: a fetch outlives neither the start nor the reload
// that asked for it.
type seriesFetcher interface {
	Fetch(ctx context.Context, token string, deviceId string, serviceId string, column string, start time.Time, end time.Time) ([]dataset.Point, error)
}

// newRuntime is what the tests use: everything except the connector is already
// an interface.
func newRuntime(config config.Config, environments repo.Environments, states repo.States, datasets repo.Datasets, publisher eventPublisher) *Runtime {
	jsTimeout := config.JsTimeout
	if jsTimeout <= 0 {
		util.Logger.Warn("no js timeout configured, using the default", "default", defaultJsTimeout)
		jsTimeout = defaultJsTimeout
	}
	flushInterval := config.StateFlushInterval
	if flushInterval <= 0 {
		util.Logger.Warn("no state flush interval configured, using the default", "default", defaultStateFlushInterval)
		flushInterval = defaultStateFlushInterval
	}
	result := &Runtime{
		config:        config,
		environments:  environments,
		states:        states,
		datasets:      datasets,
		publisher:     publisher,
		jsTimeout:     jsTimeout,
		flushInterval: flushInterval,
		envs:          map[string]*environment{},
		commands:      map[commandKey]*runningChannel{},
		devices:       map[string]*environment{},
		backfills:     map[string]*backfillJob{},
		histories:     map[string]*historyJob{},
	}
	result.historyEngine = result.runHistory
	return result
}

// Start loads every environment and starts its channels. ctx bounds the whole
// runtime: cancelling it stops every ticker and the flusher, but only Stop
// writes the state that is still in memory.
func (this *Runtime) Start(ctx context.Context) error {
	this.lifecycle.Lock()
	defer this.lifecycle.Unlock()
	if this.running {
		return errors.New("runtime is already started")
	}
	defs, err := this.environments.All(ctx)
	if err != nil {
		util.Logger.Error("unable to load the environments", attributes.ErrorKey, err)
		return err
	}
	this.ctx, this.cancel = context.WithCancel(ctx)
	this.running = true
	this.mux.Lock()
	this.envs = map[string]*environment{}
	this.commands = map[commandKey]*runningChannel{}
	this.devices = map[string]*environment{}
	this.mux.Unlock()

	//the two job registries belong to the incarnation that is starting: without
	//this a restarted runtime would keep refusing every run and every backfill,
	//because the stop flags of the previous one are still set, and would answer
	//status calls out of a registry describing a runtime that no longer exists
	this.historyMux.Lock()
	this.histories = map[string]*historyJob{}
	this.historiesStopped = false
	this.historyMux.Unlock()
	this.backfillMux.Lock()
	this.backfills = map[string]*backfillJob{}
	this.backfillsStopped = false
	this.backfillMux.Unlock()

	started := 0
	for _, def := range defs {
		if this.startEnvironment(ctx, def) {
			started++
		}
	}
	this.rebuildIndex()

	this.flusher.Add(1)
	go this.flushLoop()

	util.Logger.Info("runtime started", "environments", started, "not_started", len(defs)-started, "flush_interval", this.flushInterval)
	return nil
}

// Stop stops every ticker, ends the flusher and writes what is still dirty.
//
// It is safe to call after ctx of Start has been cancelled, and that is the
// normal case: the final flush therefore does not use that context.
func (this *Runtime) Stop() {
	this.lifecycle.Lock()
	defer this.lifecycle.Unlock()
	if !this.running {
		return
	}
	this.running = false
	this.cancel()

	envs := this.snapshotEnvs()
	for _, env := range envs {
		env.runners.Wait()
	}
	//the history runs first, and only their engine phase is waited for: the phase
	//after it needs the lifecycle mutex this call holds, so counting it here
	//would deadlock. It flushes and reports on its own once Stop has returned.
	this.stopHistories()
	this.historyWorkers.Wait()
	//no further job is accepted and the running ones end; waited for so that a
	//publish in flight finishes rather than being torn out of the connector
	this.stopBackfills()
	this.backfillWorkers.Wait()
	this.flusher.Wait()
	for _, env := range envs {
		this.flush(env)
	}
	util.Logger.Info("runtime stopped", "environments", len(envs))
}

// Reload picks up the current definition of one environment and restarts its
// channels. Nothing else is touched: the other environments keep ticking, and
// this environment keeps the runtime state it has in memory, which is newer
// than what the store holds between two flushes.
func (this *Runtime) Reload(id string) {
	this.lifecycle.Lock()
	defer this.lifecycle.Unlock()
	if !this.running {
		util.Logger.Warn("the runtime is not running, ignoring the reload", "environment", id)
		return
	}
	if this.historyRunning(id) {
		//restarting the channels now would tear the virtual clock out of the run.
		//Nothing is lost: the run reads the definition again when it ends.
		util.Logger.Info("a history run owns this environment, the reload takes effect when it ends", "environment", id)
		return
	}
	//deliberately not derived from this.ctx: a reload arriving while the service
	//shuts down should read the definition and then find a cancelled runtime,
	//not fail with a context error that reads like a database problem
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	def, err := this.environments.Get(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		util.Logger.Info("the environment no longer exists, stopping it", "environment", id)
		this.removeEnvironment(id)
		return
	}
	if err != nil {
		util.Logger.Error("unable to read the environment, it keeps running unchanged", attributes.ErrorKey, err, "environment", id)
		return
	}
	this.stopRunners(id)
	//deliberately not the ctx above: that budget is for the definition read and
	//may already be spent by the time we get here. startEnvironment needs a
	//fresh parent for its own two budgets, and Background for the same reason as
	//the read above.
	this.startEnvironment(context.Background(), def)
	this.rebuildIndex()
	util.Logger.Info("environment reloaded", "environment", id)
}

// Remove stops one environment. It is called after its definition was deleted.
func (this *Runtime) Remove(id string) {
	this.lifecycle.Lock()
	defer this.lifecycle.Unlock()
	if !this.running {
		util.Logger.Warn("the runtime is not running, ignoring the removal", "environment", id)
		return
	}
	this.removeEnvironment(id)
	util.Logger.Info("environment removed", "environment", id)
}

// HandleCommand executes the channel a command addresses and reports whether
// this runtime is responsible for the device at all.
//
// The bool is the cutover: false means the device belongs to no environment, so
// the caller has to offer the command to the legacy runtime. A device that does
// belong to an environment is never handed on, even when no channel of it
// matches the service - otherwise a half migrated document would let both
// runtimes answer the same command.
func (this *Runtime) HandleCommand(externalDeviceRef string, externalServiceRef string, cmdMsg interface{}, responder func(respMsg interface{})) bool {
	this.mux.RLock()
	channel, hasChannel := this.commands[commandKey{deviceRef: externalDeviceRef, serviceRef: externalServiceRef}]
	_, hasDevice := this.devices[externalDeviceRef]
	this.mux.RUnlock()

	if !hasDevice && !hasChannel {
		return false
	}
	if !hasChannel {
		util.Logger.Warn("no channel of this asset publishes to that service, the command is dropped",
			"device_ref", externalDeviceRef, "service_ref", externalServiceRef)
		return true
	}
	env := channel.env
	//the dispatch is counted before it starts, so that a caller replacing the
	//state waits for it: the command does not run on the environment context, so
	//cancelling the runners leaves it in flight
	if accepted, reason := env.enterCommand(); !accepted {
		util.Logger.Warn("the command is dropped", "reason", reason,
			"environment", env.id, "device_ref", externalDeviceRef)
		return true
	}
	defer env.leaveCommand()
	this.dispatch(env, channel.gen, channel.binding, cmdMsg, responder, false, time.Now())
	return true
}

// ExternalDeviceRefs returns every platform device the runtime acts on, sorted.
// It exists so that the wiring can check the one thing the per world cutover
// cannot see: a legacy world that was not migrated but whose devices an
// environment claims anyway.
func (this *Runtime) ExternalDeviceRefs() []string {
	this.mux.RLock()
	defer this.mux.RUnlock()
	result := make([]string, 0, len(this.devices))
	for ref := range this.devices {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result
}

// startEnvironment prepares one environment and starts its channel runners. It
// must be called with lifecycle held, and it must not be called for an
// environment whose runners are still running.
//
// ctx is the cancellation parent of the two phases below and must not carry a
// deadline of its own: loading the series and reading the runtime state have
// separate budgets, for the reason seriesLoadTimeout gives, and a deadline
// handed in here would silently cap both.
func (this *Runtime) startEnvironment(ctx context.Context, def domain.Environment) bool {
	if def.Id == "" {
		util.Logger.Warn("environment without an id is not started", "name", def.Name)
		return false
	}
	seriesCtx, cancelSeries := context.WithTimeout(ctx, seriesLoadTimeout)
	series := this.loadSeries(seriesCtx, def)
	//released as soon as the phase ends rather than deferred, so nothing below
	//can accidentally be written against a budget the fetches already spent
	cancelSeries()
	gen := newGeneration(def, series)

	this.mux.RLock()
	env, known := this.envs[def.Id]
	this.mux.RUnlock()

	if !known {
		//a fresh budget, taken after the series are in: whatever the fetches
		//above needed is not deducted from this read
		stateCtx, cancelState := context.WithTimeout(ctx, storeTimeout)
		state, err := this.states.Load(stateCtx, def.Id)
		cancelState()
		if err != nil {
			//NOT started on purpose. Seeding from the definition and then
			//flushing would overwrite a stored state that is only temporarily
			//unreadable, and losing what a simulation produced is worse than not
			//simulating until the next restart or edit.
			util.Logger.Error("unable to load the runtime state, the environment is not started",
				attributes.ErrorKey, err, "environment", def.Id)
			return false
		}
		env = &environment{id: def.Id, state: state}
	}

	//the live start seeds the governed context keys with the value of now; a
	//history run seeds them with the value of its own window start instead
	env.seed(gen, time.Now())
	//after seed, because it reads the persisted meter readings seed has just
	//made sure are there, and before the new generation is published, because
	//from that moment on its runners write the cache it is fixing up
	env.carryLastValues(gen)
	//what the timeline governs is a property of the definition, so what has
	//already been reported about it belongs to the generation that is going away
	env.forgetTimelineWarnings()

	this.mux.Lock()
	env.gen = gen
	this.envs[def.Id] = env
	this.mux.Unlock()

	this.reportDevicesOnline(gen)

	envCtx, cancel := context.WithCancel(this.ctx)
	env.cancel = cancel
	for _, sensor := range gen.sensors {
		env.runners.Add(1)
		go this.runChannel(envCtx, env, gen, sensor)
	}
	for key, source := range gen.def.ContextSources {
		env.runners.Add(1)
		go this.runContextSource(envCtx, env, gen, key, source)
	}
	return true
}

// reportDevicesOnline tells the platform that the simulated devices of this
// environment are reachable. A failure is a WARN: the simulation itself works,
// only the displayed connection state is stale, and the next start reports again.
func (this *Runtime) reportDevicesOnline(gen *generation) {
	if this.stateLogger == nil {
		return
	}
	for ref := range gen.deviceRefs {
		if ref == "" {
			continue
		}
		if err := this.stateLogger.LogDeviceConnect(ref); err != nil {
			util.Logger.Warn("unable to report the device as online",
				attributes.ErrorKey, err, "environment", gen.def.Id, "device", ref)
		}
	}
}

// stopRunners ends the tickers of one environment and waits for the script that
// may be running right now. The environment, its state and its mutex stay.
func (this *Runtime) stopRunners(id string) {
	this.mux.RLock()
	env := this.envs[id]
	this.mux.RUnlock()
	if env == nil {
		return
	}
	if env.cancel != nil {
		env.cancel()
	}
	env.runners.Wait()
}

// removeEnvironment stops one environment for good. It must be called with
// lifecycle held.
//
// The order matters. The environment leaves the index first, so that no new
// command and no further flush can pick it up; then it is marked removed, which
// is what a flush that is already waiting for the mutex checks; then the
// runners and the flush in flight are waited for. Only after that is it safe to
// look at the stored state.
func (this *Runtime) removeEnvironment(id string) {
	//before anything else: a backfill or a history run of a deleted environment
	//publishes to devices that are being deleted with it
	this.cancelHistory(id)
	this.cancelBackfill(id)

	this.mux.Lock()
	env := this.envs[id]
	delete(this.envs, id)
	this.mux.Unlock()
	this.rebuildIndex()
	if env == nil {
		return
	}

	env.mux.Lock()
	env.removed = true
	env.mux.Unlock()

	if env.cancel != nil {
		env.cancel()
	}
	env.runners.Wait()
	//a command runs off the runner context, so it is waited for separately; the
	//removed flag above is what keeps a new one from being counted after this
	env.commands.Wait()
	env.saves.Wait()

	this.deleteStateIfDefinitionIsGone(id)
}

// deleteStateIfDefinitionIsGone closes a window the api cannot: it deletes
// definition and state before telling the runtime, so a flush already in flight
// can recreate the state document afterwards and leave it behind forever. This
// is the second delete mongodb recommends for a non transactional two step.
//
// The definition is read first, so the state of a live environment is safe.
func (this *Runtime) deleteStateIfDefinitionIsGone(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	_, err := this.environments.Get(ctx, id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		err = this.states.Delete(ctx, id)
		if err != nil {
			util.Logger.Error("unable to delete the runtime state of the removed environment",
				attributes.ErrorKey, err, "environment", id)
		}
	case err != nil:
		util.Logger.Warn("unable to check whether the definition is gone, the runtime state is kept",
			attributes.ErrorKey, err, "environment", id)
	default:
		util.Logger.Warn("the environment was stopped although its definition still exists, its runtime state is kept",
			"environment", id)
	}
}

// rebuildIndex rebuilds the command and device lookup from the generations that
// are currently running. It must be called with lifecycle held.
//
// The environments are visited in id order rather than in map order: if two of
// them claim the same platform device, which one wins has to be the same on
// every rebuild, otherwise a command would land in a different environment
// after every edit.
func (this *Runtime) rebuildIndex() {
	this.mux.Lock()
	defer this.mux.Unlock()

	ids := make([]string, 0, len(this.envs))
	for id := range this.envs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	commands := map[commandKey]*runningChannel{}
	devices := map[string]*environment{}
	for _, id := range ids {
		env := this.envs[id]
		gen := env.gen
		if gen == nil {
			continue
		}
		refs := make([]string, 0, len(gen.deviceRefs))
		for ref := range gen.deviceRefs {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for _, ref := range refs {
			if previous, taken := devices[ref]; taken && previous != env {
				util.Logger.Warn("two environments claim the same platform device, only the first one acts on it",
					"device_ref", ref, "environment", previous.id, "ignored_environment", env.id)
				continue
			}
			devices[ref] = env
		}
		for key, binding := range gen.commands {
			if previous, taken := commands[key]; taken {
				util.Logger.Warn("two environments claim the same platform service, only the first one answers commands",
					"device_ref", key.deviceRef, "service_ref", key.serviceRef,
					"environment", previous.env.id, "ignored_environment", env.id)
				continue
			}
			commands[key] = &runningChannel{env: env, gen: gen, binding: binding}
		}
	}
	this.commands = commands
	this.devices = devices
}

func (this *Runtime) snapshotEnvs() []*environment {
	this.mux.RLock()
	defer this.mux.RUnlock()
	ids := make([]string, 0, len(this.envs))
	for id := range this.envs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]*environment, 0, len(ids))
	for _, id := range ids {
		result = append(result, this.envs[id])
	}
	return result
}

// SetState merges values into the live state of one running environment - how
// a boundary condition (an outdoor temperature, a zone's hall temperature, an
// asset's machine speed) is turned from outside the simulation. Scripts read it
// on their next tick.
//
// The change is applied to the in-memory state and marked dirty rather than
// written through: the flusher owns that write, and a direct one would be
// overwritten by it anyway.
func (this *Runtime) SetState(id string, change repo.StateChange) error {
	//gen is guarded by the runtime mux, the same way rebuildIndex reads it
	this.mux.RLock()
	env, running := this.envs[id]
	var gen *generation
	if running {
		gen = env.gen
	}
	this.mux.RUnlock()
	if !running {
		return repo.ErrNotRunning
	}

	env.mux.Lock()
	defer env.mux.Unlock()
	now := time.Now()
	//judged under the mutex, because a governed context key is compared against
	//the value the reading direction answers with, and that is the live state
	//covered by the timeline. Still ahead of the two checks below, so a malformed
	//change is a 400 whatever else is going on with the environment.
	if err := this.refuseChange(env, gen, change, now); err != nil {
		return err
	}
	if env.removed {
		return repo.ErrNotRunning
	}
	//read under the same mutex the run replaces the state with, so a change can
	//never land in a state that is about to be thrown away
	if env.underHistory {
		return ErrHistoryRunning
	}
	mergeInto(env.state.Context, change.Context)
	for zoneId, values := range change.Zones {
		if env.state.Zones[zoneId] == nil {
			env.state.Zones[zoneId] = map[string]interface{}{}
		}
		//a value the zone gives a time constant follows its set point instead of
		//jumping to it; everything else is set at once
		constants := gen.zones[zoneId].timeConstants
		for key, value := range values {
			tau := constants[key]
			target, numeric := asFloat(value)
			if tau <= 0 || !numeric {
				env.state.Zones[zoneId][key] = copyValue(value)
				continue
			}
			env.startApproach(zoneId, key, target, tau, now)
		}
	}
	for assetId, values := range change.Assets {
		if env.state.Assets[assetId] == nil {
			env.state.Assets[assetId] = map[string]interface{}{}
		}
		mergeInto(env.state.Assets[assetId], values)
	}
	env.dirty = true
	return nil
}

func mergeInto(target map[string]interface{}, values map[string]interface{}) {
	for key, value := range values {
		target[key] = copyValue(value)
	}
}

// refuseChange collects everything about a state change that cannot be applied
// and reports all of it at once, rather than the first thing it finds: fixing a
// change one round trip per mistake is the same unusable endpoint that made
// validation collect its problems.
//
// It must be called with env.mux held, since the governed-key check reads the
// live state.
func (this *Runtime) refuseChange(env *environment, gen *generation, change repo.StateChange, now time.Time) error {
	unknown := unknownChangeIds(gen, change)
	governed := this.governedChangeKeys(env, gen, change, now)
	switch {
	case unknown != nil && governed != nil:
		//joined rather than folded into a third type: each of the two is
		//meaningful on its own everywhere else, errors.As still finds either,
		//and the message carries both
		return errors.Join(unknown, governed)
	case unknown != nil:
		return unknown
	case governed != nil:
		return governed
	}
	return nil
}

// unknownChangeIds names every zone and asset the definition does not have,
// rather than only the first: a key written under an id nothing reads is state
// that looks set and has no effect.
func unknownChangeIds(gen *generation, change repo.StateChange) *repo.UnknownIdsError {
	problem := &repo.UnknownIdsError{}
	for zoneId := range change.Zones {
		if gen == nil || gen.zones[zoneId] == nil {
			problem.Zones = append(problem.Zones, zoneId)
		}
	}
	for assetId := range change.Assets {
		if gen == nil || gen.assets[assetId] == nil {
			problem.Assets = append(problem.Assets, assetId)
		}
	}
	if len(problem.Zones) == 0 && len(problem.Assets) == 0 {
		return nil
	}
	sort.Strings(problem.Zones)
	sort.Strings(problem.Assets)
	return problem
}

// governedChangeKeys names the context keys a change would move although the
// timeline governs them.
//
// Only a value that actually differs is refused, and that is the whole subtlety:
// the reading direction hands out the declared value of a governed key, so a
// client that reads the state, edits a neighbouring key and sends the whole
// thing back submits that value unchanged. Refusing it would break the round
// trip the two endpoints are documented as, for a change that changes nothing.
// A real attempt to move the key is named.
//
// The comparison is the one a schedule makes against a stored state value, so a
// number that arrived as an integer counts as equal to the float it stands for.
// It must be called with env.mux held.
func (this *Runtime) governedChangeKeys(env *environment, gen *generation, change repo.StateChange, now time.Time) *repo.TimelineGovernedError {
	if gen == nil {
		return nil
	}
	problem := &repo.TimelineGovernedError{}
	for key, value := range change.Context {
		if !gen.timeline.governsContext(key) {
			continue
		}
		//exactly what Snapshot would have answered for this key at this instant:
		//the declared value where the timeline has taken effect, the live one
		//before its first change
		declared := this.contextValue(env, gen, key, now)
		//declared second, so the comparison switches on the float64 and the
		//submitted value goes through asFloat
		if sameStateValue(value, declared) {
			continue
		}
		problem.Keys = append(problem.Keys, key)
	}
	if len(problem.Keys) == 0 {
		return nil
	}
	sort.Strings(problem.Keys)
	return problem
}

// runChannel is one channel on a ticker. Without a source interval it is the
// legacy shape: one ticker, and what the script sends goes out at once.
func (this *Runtime) runChannel(ctx context.Context, env *environment, gen *generation, binding channelBinding) {
	defer env.runners.Done()
	//before the split: a change trigger replaces both shapes. Its evaluation
	//ticker is the source interval where the channel has one, and the publish
	//ticker becomes the heartbeat.
	if binding.cov != nil {
		this.runChangeChannel(ctx, env, gen, binding)
		return
	}
	if binding.sourceInterval > 0 {
		this.runSplitChannel(ctx, env, gen, binding)
		return
	}
	ticker := time.NewTicker(time.Duration(binding.channel.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	//one fault memory per runner, touched by this goroutine alone
	faultMemory := &faultRun{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			//input is nil on a tick, as it was for a legacy sensor service
			this.dispatch(env, gen, binding, nil, func(value interface{}) {
				//env.mux is held here: every executor calls send inside its own run
				//under it, which is the same precondition covGate states
				value, send := this.faulted(env, binding, faultMemory, value, now)
				if !send {
					return
				}
				this.publish(env, binding, value)
			}, true, now)
		}
	}
}

// runSplitChannel drives a channel whose source computes more often than the
// channel publishes. The source ticker evolves the state and hands its value to
// pending; the publish ticker sends what is there.
//
// A channel with a source interval and no publish interval is legitimate: it
// only evolves state that other channels of the asset read, has no publish
// ticker, and what its script sends is dropped rather than queued forever.
func (this *Runtime) runSplitChannel(ctx context.Context, env *environment, gen *generation, binding channelBinding) {
	pending := &latest{}
	source := time.NewTicker(time.Duration(binding.sourceInterval) * time.Second)
	defer source.Stop()
	//one fault memory per runner: the source half and the publish half are two
	//cases of the one select below, so this goroutine is its only writer
	faultMemory := &faultRun{}

	publishes := binding.channel.Direction == domain.Sensor && binding.channel.IntervalSeconds > 0
	var publishC <-chan time.Time
	if publishes {
		publish := time.NewTicker(time.Duration(binding.channel.IntervalSeconds) * time.Second)
		defer publish.Stop()
		publishC = publish.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-source.C:
			this.dispatch(env, gen, binding, nil, pending.put, true, time.Now())
		case <-publishC:
			//nothing to send before the source has run once. Skipping is right
			//rather than sending a zero value: the channel carries a unit, and a
			//fabricated reading is worse than a missing one.
			value, ok := pending.get()
			if !ok {
				continue
			}
			//a channel without faults publishes exactly as it did before this
			//existed, and in particular does not take a mutex this branch never
			//needed - the other hooks sit inside a dispatch that holds it anyway
			if len(binding.faults.list) == 0 {
				this.publish(env, binding, value)
				continue
			}
			now := time.Now()
			//this branch runs outside a dispatch, so it takes the environment mutex
			//itself; faulted reads and writes the persisted meter offsets
			env.mux.Lock()
			reading, send := this.faulted(env, binding, faultMemory, value, now)
			env.mux.Unlock()
			if send {
				this.publish(env, binding, reading)
			}
		}
	}
}

// dispatch runs whatever drives the channel. tick separates a schedule tick
// from a command: a cumulative profile advances its meter only on ticks, or
// every read command would inflate the reading.
//
// now is the instant of this run, taken once by the caller: every source, every
// replay anchor and every zone value with a time constant resolves against the
// same moment, so one tick is one point in time rather than a handful of them.
func (this *Runtime) dispatch(env *environment, gen *generation, binding channelBinding, input interface{}, send func(value interface{}), tick bool, now time.Time) {
	//every send below happens while the environment mutex is held (a script
	//calls send inside its run, the other kinds send under their own lock), so
	//remembering the value here needs no lock of its own. The cache is what a
	//formula's channel reference reads.
	remembered := func(value interface{}) {
		if number, ok := asFloat(value); ok {
			if env.lastValues == nil {
				env.lastValues = map[string]float64{}
			}
			env.lastValues[binding.channel.Id] = number
		}
		send(value)
	}
	switch binding.channel.Source.Kind {
	case domain.SourceProfile:
		this.executeProfile(env, gen, binding, remembered, tick, now)
	case domain.SourceDataset:
		this.executeDataset(env, gen, binding, remembered, now)
	case domain.SourceFormula:
		this.executeFormula(env, gen, binding, remembered, now)
	case domain.SourceAggregate:
		//no clock: an aggregate reads the values its inputs last produced and
		//has nothing of its own to resolve against an instant
		this.executeAggregate(env, gen, binding, remembered)
	case domain.SourceSchedule:
		//through remembered like every other declarative source: the value of a
		//schedule is what a formula reading channel.<id> sees, and what an
		//aggregate above the asset sums
		this.executeSchedule(env, gen, binding, remembered, now)
	default:
		this.execute(env, gen, binding, input, remembered, now)
	}
}

// executeFormula resolves the inputs and publishes the result. A missing state
// key counts as 0, like moses.state.get seeds a missing key - a formula over a
// value nothing has produced yet starts from zero rather than failing.
func (this *Runtime) executeFormula(env *environment, gen *generation, binding channelBinding, send func(value interface{}), now time.Time) {
	inputs := binding.channel.Source.Formula.Inputs
	env.mux.Lock()
	defer env.mux.Unlock()
	values := make(map[string]interface{}, len(inputs))
	for name, ref := range inputs {
		values[name] = this.resolveInput(env, gen, binding.zoneId, binding.asset.id, ref, now)
	}
	value, err := binding.program.Evaluate(values)
	if err != nil {
		util.Logger.Warn("the formula failed to evaluate", attributes.ErrorKey, err,
			"environment", env.id, "channel", binding.channel.Id)
		return
	}
	send(value)
}

// executeAggregate publishes the sum of the channels the sub-metered assets
// carry, resolved once at index time (gen.aggregateInputs) and read here from
// the values those channels last published. gen is immutable and captured by
// the runner, so the list needs no lock; env.lastValues does, and it is the
// same mutex every other source sends under - no second lock, no new order.
//
// A channel that has not produced a value yet counts as 0, the same way a
// formula's channel reference does, so one dead sub-meter cannot silence the
// whole total. An aggregate over no inputs publishes 0 rather than nothing.
// lastValues is in memory only, so after a restart the sum is short until
// every sub-metered channel has ticked once, except for cumulative channels,
// whose reading is restored from persisted state at start
// (environment.carryLastValues). An input whose last value is not a finite
// number is left out of the sum rather than carried into it, since a script
// can send NaN or infinity on a channel and one such child would otherwise
// turn every total above it into NaN.
func (this *Runtime) executeAggregate(env *environment, gen *generation, binding channelBinding, send func(value interface{})) {
	inputs := gen.aggregateInputs[binding.channel.Id]
	env.mux.Lock()
	defer env.mux.Unlock()
	//summed in the indexed order, which is document order: float addition is
	//not associative, so a map iteration here would make the same document
	//produce slightly different totals from one start to the next
	sum := 0.0
	skipped := 0
	for _, id := range inputs {
		value := env.lastValues[id]
		if math.IsNaN(value) || math.IsInf(value, 0) {
			skipped++
			continue
		}
		sum += value
	}
	if skipped > 0 {
		//one line per tick of this aggregate, not one per input: the channel
		//that produced the value logs its own problem, and the point here is
		//that this total is incomplete
		util.Logger.Warn("an aggregate left out inputs whose last value is not a finite number, the total is short by them",
			"environment", env.id, "channel", binding.channel.Id, "skipped", skipped, "inputs", len(inputs))
	}
	send(sum)
}

func (this *Runtime) resolveInput(env *environment, gen *generation, zoneId string, assetId string, ref string, now time.Time) interface{} {
	if key, ok := strings.CutPrefix(ref, formula.RefContext); ok {
		//through the read-only layer: a governed key is what the document says
		//it is at this instant, whatever is in the live state
		return this.contextValue(env, gen, key, now)
	}
	if key, ok := strings.CutPrefix(ref, formula.RefZone); ok {
		env.advanceZone(zoneId, now)
		return numericOrZero(env.zoneStates(zoneId)[key])
	}
	if key, ok := strings.CutPrefix(ref, formula.RefAsset); ok {
		return numericOrZero(env.assetStates(assetId)[key])
	}
	if id, ok := strings.CutPrefix(ref, formula.RefChannel); ok {
		return env.lastValues[id]
	}
	return 0.0
}

func numericOrZero(value interface{}) float64 {
	if number, ok := asFloat(value); ok {
		return number
	}
	return 0.0
}

// fileSeriesCache holds every uploaded dataset already fetched and parsed
// during one loadSeries call, keyed by dataset id, so that several channels -
// or a channel and a context source - replaying the same upload share one
// GridFS read and one parse instead of repeating it.
//
// It lives for exactly one loadSeries call and needs no lock: loadSeries and
// everything it calls run on the single goroutine that holds this.lifecycle
// (see startEnvironment), never concurrently with another load. The platform
// origin is deliberately not cached here, since its result depends on the
// window and reference of the one caller asking for it.
type fileSeriesCache map[string][]dataset.Series

// loadSeries fetches and parses the uploaded datasets the definition's
// channels reference. A channel whose dataset cannot be loaded is reported and
// skipped by newGeneration; the environment still starts, because one deleted
// upload should not take a whole site down.
func (this *Runtime) loadSeries(ctx context.Context, def domain.Environment) map[string][]dataset.Point {
	result := map[string][]dataset.Point{}
	cache := fileSeriesCache{}
	for _, zone := range def.Zones {
		this.loadZoneSeries(ctx, def.Id, def.Owner, zone, result, cache)
	}
	this.loadContextSeries(ctx, def, result, cache)
	return result
}

func (this *Runtime) loadZoneSeries(ctx context.Context, envId string, owner string, zone domain.Zone, result map[string][]dataset.Point, cache fileSeriesCache) {
	for _, nested := range zone.Zones {
		this.loadZoneSeries(ctx, envId, owner, nested, result, cache)
	}
	for _, asset := range zone.Assets {
		for _, channel := range asset.Channels {
			source := channel.Source
			if source.Kind != domain.SourceDataset || source.Dataset == nil {
				continue
			}
			if source.Dataset.Origin != domain.OriginFile && source.Dataset.Origin != domain.OriginPlatform {
				continue
			}
			points, err := this.fetchSeries(ctx, owner, source.Dataset, cache)
			if err != nil {
				util.Logger.Warn("unable to load the dataset of this channel, it does not play",
					attributes.ErrorKey, err, "environment", envId, "channel", channel.Id, "dataset", source.Dataset.Ref)
				continue
			}
			result[channel.Id] = points
		}
	}
}

// fetchSeries resolves one dataset source into points. cache is the parsed
// upload cache of the load in progress; it may be nil, which costs the sharing
// and nothing else.
func (this *Runtime) fetchSeries(ctx context.Context, owner string, source *domain.DatasetSource, cache fileSeriesCache) ([]dataset.Point, error) {
	if source.Origin == domain.OriginPlatform {
		return this.fetchPlatformSeries(ctx, owner, source)
	}
	series, cached := cache[source.Ref]
	if !cached {
		if this.datasets == nil {
			return nil, errors.New("no dataset store configured")
		}
		meta, err := this.datasets.Get(ctx, source.Ref)
		if err != nil {
			return nil, err
		}
		location, err := time.LoadLocation(meta.Timezone)
		if err != nil {
			return nil, fmt.Errorf("the dataset carries the unknown timezone %q: %w", meta.Timezone, err)
		}
		raw, err := this.datasets.Content(ctx, source.Ref)
		if err != nil {
			return nil, err
		}
		series, err = dataset.ParseCSV(raw, location)
		if err != nil {
			//the upload validated this file, so a parse failure here means the
			//parser changed incompatibly - worth a loud word
			return nil, fmt.Errorf("the stored file no longer parses: %w", err)
		}
		//tolerated like a nil map read: the platform branch above returns before
		//ever touching the cache, so a caller that forgot it would look correct
		//in a platform test and panic on the first uploaded dataset of a real
		//start. Losing the sharing is the smaller failure.
		if cache != nil {
			cache[source.Ref] = series
		}
	}
	if source.Column == "" {
		return series[0].Points, nil
	}
	for _, s := range series {
		if s.Name == source.Column {
			return s.Points, nil
		}
	}
	return nil, fmt.Errorf("the dataset has no column %q", source.Column)
}

// fetchPlatformSeries pulls a window of a real timeseries, backwards from now.
// The window is frozen until the next reload, which is what makes the replay
// deterministic between reloads.
func (this *Runtime) fetchPlatformSeries(ctx context.Context, owner string, source *domain.DatasetSource) ([]dataset.Point, error) {
	if this.fetcher == nil {
		return nil, errors.New("no timescale_wrapper_url configured, the platform origin is disabled")
	}
	if this.ownerToken == nil {
		return nil, errors.New("no token source configured")
	}
	window, err := domain.ParseWindow(source.Window)
	if err != nil {
		return nil, err
	}
	token, err := this.ownerToken(owner)
	if err != nil {
		return nil, fmt.Errorf("unable to obtain a token for the owner: %w", err)
	}
	end := time.Now()
	return this.fetcher.Fetch(ctx, token, source.Ref, source.ServiceRef, source.Column, end.Add(-window), end)
}

// executeDataset publishes the replay value for now. The anchor of a looping
// replay is set on first use and persisted with the state, so a restart
// resumes mid-loop.
func (this *Runtime) executeDataset(env *environment, gen *generation, binding channelBinding, send func(value interface{}), now time.Time) {
	//the scale of this instant, which is the one field of a replay the timeline
	//governs; the anchor below is unaffected by it
	source := gen.timeline.effectiveDataset(domain.TimelineChannel, binding.channel.Id, *binding.channel.Source.Dataset, now)
	env.mux.Lock()
	defer env.mux.Unlock()
	anchor := this.anchorFor(env, binding.channel.Id, &source, now)
	//stepSeconds, not the publish interval: a distributing replay hands out the
	//share of a sample one computation stands for, and with a change trigger the
	//value is computed on the evaluation cadence. Without a trigger the two are
	//the same number.
	value, playable := replayValue(source, binding.points, anchor, now, binding.stepSeconds)
	if !playable {
		return
	}
	send(value)
}

// executeProfile computes the profile under the environment mutex, like a
// script run, so the cumulative state in the asset map is as safe as any other
// state. send happens under the mutex too, which is what a script's
// moses.service.send does.
func (this *Runtime) executeProfile(env *environment, gen *generation, binding channelBinding, send func(value interface{}), tick bool, now time.Time) {
	//the profile of this instant: base and spread as the timeline has them here,
	//the inline ones until the first change of each
	p := gen.timeline.effectiveProfile(domain.TimelineChannel, binding.channel.Id, *binding.channel.Source.Profile, now)
	env.mux.Lock()
	defer env.mux.Unlock()
	//stepSeconds is the span one computation stands for: the publish interval
	//without a change trigger, the evaluation interval with one. It cuts the
	//spread slot and, below, the share of the hourly rate a tick adds, so a
	//channel evaluating more often than it publishes does not over-count its
	//meter.
	value := profileValue(p, gen.def.Seed, binding.channel.Id, binding.stepSeconds, now)
	if p.Cumulative {
		states := env.assetStates(binding.asset.id)
		counter, _ := asFloat(states[binding.channel.Id])
		if tick {
			//the profile value is a rate per hour; one tick adds its share.
			//a gap (restart, downtime) is not caught up: a stopped plant does
			//not consume.
			counter += value * float64(binding.stepSeconds) / 3600
			states[binding.channel.Id] = counter
			env.dirty = true
		}
		value = counter
	}
	send(value)
}

func (this *Runtime) execute(env *environment, gen *generation, binding channelBinding, input interface{}, send func(value interface{}), now time.Time) {
	err := run(binding.code, this.jsApi(env, gen, binding, input, send, now), this.jsTimeout, &env.mux)
	if err != nil {
		util.Logger.Warn("channel script failed", attributes.ErrorKey, err,
			"environment", env.id, "asset", binding.asset.id, "channel", binding.channel.Id,
			"code", trimCodeDefault(binding.code))
	}
}

// publish sends what a script handed to moses.service.send().
//
// The bool says whether the value really reached the platform. Only the change
// trigger reads it - it must not remember a value as published when the send
// failed - and the ticker paths ignore it, because there is nothing they could
// do differently: the next tick comes either way.
func (this *Runtime) publish(env *environment, binding channelBinding, value interface{}) bool {
	return this.publishReporting(env, binding, value, true)
}

// publishVia is what every publish path has in common: the two reference checks
// a reading needs to have somewhere to go, and the logging around the attempt.
// publish is called only once both refs are there, so it may read them.
//
// report false suppresses the failure lines, and only the change trigger passes
// it, for an attempt that follows a failure it has already reported - see
// covLogGate. The ticker paths always report: they publish once per interval, so
// a line per failure is a line per interval. The debug line is not throttled: it
// is off unless debug is configured, and whoever turned it on asked for every
// send.
//
// The error is the platform's refusal; it is nil both when the reading went out
// and when it had nowhere to go. Only the history run reads it, because it puts
// the message into the status a caller polls.
func (this *Runtime) publishVia(env *environment, binding channelBinding, value interface{}, report bool, publish func() error) (bool, error) {
	if this.config.Debug {
		util.Logger.Debug("send channel data", "environment", env.id, "asset", binding.asset.id,
			"channel", binding.channel.Id, "value", value)
	}
	if binding.asset.externalRef == "" {
		if report {
			util.Logger.Warn("no external ref for asset, nothing was sent", "environment", env.id, "asset", binding.asset.id)
		}
		return false, nil
	}
	if binding.channel.ExternalRef == "" {
		if report {
			util.Logger.Warn("no external ref for channel, nothing was sent", "environment", env.id, "channel", binding.channel.Id)
		}
		return false, nil
	}
	err := publish()
	if err != nil {
		if report {
			util.Logger.Error("unable to send channel data", attributes.ErrorKey, err,
				"device_ref", binding.asset.externalRef, "service_ref", binding.channel.ExternalRef)
		}
		return false, err
	}
	return true, nil
}

// publishReporting is the live publish: the platform stamps the reading with its
// arrival time.
func (this *Runtime) publishReporting(env *environment, binding channelBinding, value interface{}, report bool) bool {
	sent, _ := this.publishVia(env, binding, value, report, func() error {
		return this.publisher.PublishEvent(binding.asset.externalRef, binding.channel.ExternalRef, value)
	})
	return sent
}

// publishAt publishes a reading that was taken at a past instant. It reaches
// timescale under that instant only for a service whose time path resolves; for
// every other one the platform stamps the arrival time instead.
func (this *Runtime) publishAt(env *environment, binding channelBinding, value interface{}, report bool, at time.Time) (bool, error) {
	return this.publishVia(env, binding, value, report, func() error {
		return this.publisher.PublishEventAt(binding.asset.externalRef, binding.channel.ExternalRef, value, at)
	})
}

// flushLoop is the write behind: one goroutine for the whole runtime, writing
// the environments that changed since the last round.
func (this *Runtime) flushLoop() {
	defer this.flusher.Done()
	ticker := time.NewTicker(this.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-this.ctx.Done():
			return
		case <-ticker.C:
			for _, env := range this.snapshotEnvs() {
				this.flush(env)
			}
		}
	}
}

// flush writes one environment's state if it changed.
//
// The copy is taken under the environment mutex and the write happens outside
// of it, so a slow database delays the state on disk but not the simulation. A
// failed write is an ERROR - it is data loss waiting to happen - and puts the
// dirty flag back, so the next round tries again instead of dropping what the
// simulation produced.
//
// saveMux covers the copy as well as the write, so that two flushes of one
// environment cannot read two states and store them in the opposite order,
// which would leave the older one standing.
func (this *Runtime) flush(env *environment) {
	env.saveMux.Lock()
	defer env.saveMux.Unlock()

	env.mux.Lock()
	if env.removed || !env.dirty {
		env.mux.Unlock()
		return
	}
	state := env.snapshot()
	env.dirty = false
	//counted while the mutex is held, so that a removal either sees this save
	//and waits for it, or prevents it altogether
	env.saves.Add(1)
	env.mux.Unlock()
	defer env.saves.Done()

	//not derived from this.ctx on purpose: the final flush of Stop() runs after
	//that context was cancelled, and a cancelled context would turn it into a
	//guaranteed loss of everything that was not written yet
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	err := this.states.Save(ctx, state)
	if err != nil {
		util.Logger.Error("unable to save the runtime state", attributes.ErrorKey, err, "environment", env.id)
		env.mux.Lock()
		env.dirty = true
		env.mux.Unlock()
	}
}
