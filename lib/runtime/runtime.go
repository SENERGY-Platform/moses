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
	"sort"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
	platform_connector_lib "github.com/SENERGY-Platform/platform-connector-lib"
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

// Runtime runs every environment of the store.
type Runtime struct {
	config        config.Config
	environments  repo.Environments
	states        repo.States
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

func New(config config.Config, environments repo.Environments, states repo.States, connector *platform_connector_lib.Connector, stateLogger deviceStateLogger) *Runtime {
	result := newRuntime(config, environments, states, &connectorPublisher{
		connector:   connector,
		segmentName: config.ProtocolSegmentName,
	})
	result.stateLogger = stateLogger
	return result
}

// newRuntime is what the tests use: everything except the connector is already
// an interface.
func newRuntime(config config.Config, environments repo.Environments, states repo.States, publisher eventPublisher) *Runtime {
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
	return &Runtime{
		config:        config,
		environments:  environments,
		states:        states,
		publisher:     publisher,
		jsTimeout:     jsTimeout,
		flushInterval: flushInterval,
		envs:          map[string]*environment{},
		commands:      map[commandKey]*runningChannel{},
		devices:       map[string]*environment{},
	}
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
	this.startEnvironment(ctx, def)
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
	env.mux.Lock()
	removed := env.removed
	env.mux.Unlock()
	if removed {
		util.Logger.Warn("the environment was removed while the command was in flight, the command is dropped",
			"environment", env.id, "device_ref", externalDeviceRef)
		return true
	}
	this.dispatch(env, channel.gen, channel.binding, cmdMsg, responder, false)
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
func (this *Runtime) startEnvironment(ctx context.Context, def domain.Environment) bool {
	if def.Id == "" {
		util.Logger.Warn("environment without an id is not started", "name", def.Name)
		return false
	}
	gen := newGeneration(def)

	this.mux.RLock()
	env, known := this.envs[def.Id]
	this.mux.RUnlock()

	if !known {
		state, err := this.states.Load(ctx, def.Id)
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

	env.seed(gen)

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

// SetState merges values into the live state of one running environment. This is
// how a boundary condition is turned from outside the simulation: an outdoor
// temperature in the context, a hall temperature on a zone, a machine's speed on
// an asset. The scripts read it on their next tick.
//
// The change is applied to the in memory state and marked dirty, not written
// through to the store: the flusher owns that write, and a direct one would be
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
	if err := validateChangeIds(gen, change); err != nil {
		return err
	}

	env.mux.Lock()
	defer env.mux.Unlock()
	if env.removed {
		return repo.ErrNotRunning
	}
	now := time.Now()
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

// validateChangeIds refuses a change naming a zone or asset the definition does
// not have, and names every one of them rather than the first.
func validateChangeIds(gen *generation, change repo.StateChange) error {
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

// runChannel is one channel on a ticker. Without a source interval it is the
// legacy shape: one ticker, and what the script sends goes out at once.
func (this *Runtime) runChannel(ctx context.Context, env *environment, gen *generation, binding channelBinding) {
	defer env.runners.Done()
	if binding.sourceInterval > 0 {
		this.runSplitChannel(ctx, env, gen, binding)
		return
	}
	ticker := time.NewTicker(time.Duration(binding.channel.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			//input is nil on a tick, as it was for a legacy sensor service
			this.dispatch(env, gen, binding, nil, func(value interface{}) {
				this.publish(env, binding, value)
			}, true)
		}
	}
}

// runSplitChannel drives a channel whose source computes more often than the
// channel publishes. The source ticker evolves the state and hands its value to
// pending; the publish ticker sends what is there.
//
// A channel with a source interval and no publish interval is legitimate: it
// only evolves state that the other channels of the asset read. It then has no
// publish ticker, and what its script sends is dropped rather than queued
// forever - the legacy runtime had no way to express this at all, so there is no
// behaviour to be faithful to.
func (this *Runtime) runSplitChannel(ctx context.Context, env *environment, gen *generation, binding channelBinding) {
	pending := &latest{}
	source := time.NewTicker(time.Duration(binding.sourceInterval) * time.Second)
	defer source.Stop()

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
			this.dispatch(env, gen, binding, nil, pending.put, true)
		case <-publishC:
			//nothing to send before the source has run once. Skipping is right
			//rather than sending a zero value: the channel carries a unit, and a
			//fabricated reading is worse than a missing one.
			if value, ok := pending.get(); ok {
				this.publish(env, binding, value)
			}
		}
	}
}

// dispatch runs whatever drives the channel. tick separates a schedule tick
// from a command: a cumulative profile advances its meter only on ticks, or
// every read command would inflate the reading.
func (this *Runtime) dispatch(env *environment, gen *generation, binding channelBinding, input interface{}, send func(value interface{}), tick bool) {
	if binding.channel.Source.Kind == domain.SourceProfile {
		this.executeProfile(env, gen, binding, send, tick)
		return
	}
	this.execute(env, gen, binding, input, send)
}

// executeProfile computes the profile under the environment mutex, like a
// script run, so the cumulative state in the asset map is as safe as any other
// state. send happens under the mutex too, which is what a script's
// moses.service.send does.
func (this *Runtime) executeProfile(env *environment, gen *generation, binding channelBinding, send func(value interface{}), tick bool) {
	p := *binding.channel.Source.Profile
	env.mux.Lock()
	defer env.mux.Unlock()
	value := profileValue(p, gen.def.Seed, binding.channel.Id, binding.channel.IntervalSeconds, time.Now())
	if p.Cumulative {
		states := env.assetStates(binding.asset.id)
		counter, _ := asFloat(states[binding.channel.Id])
		if tick {
			//the profile value is a rate per hour; one tick adds its share.
			//a gap (restart, downtime) is not caught up: a stopped plant does
			//not consume.
			counter += value * float64(binding.channel.IntervalSeconds) / 3600
			states[binding.channel.Id] = counter
			env.dirty = true
		}
		value = counter
	}
	send(value)
}

func (this *Runtime) execute(env *environment, gen *generation, binding channelBinding, input interface{}, send func(value interface{})) {
	err := run(binding.code, this.jsApi(env, gen, binding, input, send), this.jsTimeout, &env.mux)
	if err != nil {
		util.Logger.Warn("channel script failed", attributes.ErrorKey, err,
			"environment", env.id, "asset", binding.asset.id, "channel", binding.channel.Id,
			"code", trimCodeDefault(binding.code))
	}
}

// publish sends what a script handed to moses.service.send().
func (this *Runtime) publish(env *environment, binding channelBinding, value interface{}) {
	if this.config.Debug {
		util.Logger.Debug("send channel data", "environment", env.id, "asset", binding.asset.id,
			"channel", binding.channel.Id, "value", value)
	}
	if binding.asset.externalRef == "" {
		util.Logger.Warn("no external ref for asset, nothing was sent", "environment", env.id, "asset", binding.asset.id)
		return
	}
	if binding.channel.ExternalRef == "" {
		util.Logger.Warn("no external ref for channel, nothing was sent", "environment", env.id, "channel", binding.channel.Id)
		return
	}
	err := this.publisher.PublishEvent(binding.asset.externalRef, binding.channel.ExternalRef, value)
	if err != nil {
		util.Logger.Error("unable to send channel data", attributes.ErrorKey, err,
			"device_ref", binding.asset.externalRef, "service_ref", binding.channel.ExternalRef)
	}
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
func (this *Runtime) flush(env *environment) {
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
