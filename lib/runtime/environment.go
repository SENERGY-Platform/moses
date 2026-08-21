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
	"math"
	"sync"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// environment is one running environment. There is exactly one of these per
// environment id for as long as the runtime knows the environment, and it
// survives a Reload: the in memory runtime state and the mutex protecting it
// must not be replaced under a flush that is already under way.
type environment struct {
	id string

	// mux serialises every script run of this environment and, with that, every
	// read and write of state. A script holds it for its whole execution, so the
	// state maps need no lock of their own - but they may only be touched from
	// inside a run, or by a caller that holds mux itself (the flusher).
	mux sync.Mutex

	// state, dirty and removed are guarded by mux.
	state   repo.RuntimeState
	dirty   bool
	removed bool

	// saves counts the Save calls that have left the mutex but not yet returned.
	// Remove waits for it before deleting the stored state, so that a flush in
	// flight cannot resurrect the document of a deleted environment.
	saves sync.WaitGroup

	// gen, cancel and runners are only written while the runtime's lifecycle
	// mutex is held; gen is additionally guarded by the runtime's mux, because
	// rebuildIndex reads it.
	gen     *generation
	cancel  context.CancelFunc
	runners sync.WaitGroup
}

// generation is the immutable view of one version of a definition. A channel
// runner captures the generation it was started with, so a Reload cannot change
// what a running script sees; the new definition arrives as a new generation
// with new runners.
type generation struct {
	def domain.Environment

	// zones and assets are the lookup tables behind moses.world.getRoom() and
	// moses.room.getDevice(), and the source of the initial states.
	zones  map[string]*zoneInfo
	assets map[string]*assetInfo

	// sensors is what gets a ticker.
	sensors []channelBinding

	// commands maps an incoming command to the channel that answers it.
	commands map[commandKey]channelBinding

	// deviceRefs is every platform device this environment owns. It answers
	// "is this device mine" for HandleCommand, which has to be answerable even
	// for an asset whose channels cannot be executed.
	deviceRefs map[string]bool
}

type zoneInfo struct {
	initialStates map[string]interface{}
}

type assetInfo struct {
	zoneId        string
	initialStates map[string]interface{}
}

// assetRef is the part of an asset a running channel needs. It deliberately
// does not carry the asset's channel slice: a runner must not hold a reference
// into a definition it could otherwise be tempted to read while it changes.
type assetRef struct {
	id          string
	externalRef string
}

// maxIntervalSeconds is the largest interval that still fits into a
// time.Duration. Validation only rejects a negative interval, so an absurd
// positive one has to be caught here.
const maxIntervalSeconds = int64(math.MaxInt64 / int64(time.Second))

type commandKey struct {
	deviceRef  string
	serviceRef string
}

// channelBinding is everything needed to execute one channel.
type channelBinding struct {
	zoneId  string
	asset   assetRef
	channel domain.Channel
	code    string

	// sourceInterval is how often the script runs. Zero means it runs when the
	// channel publishes, which is the only behaviour the legacy runtime had.
	sourceInterval int64
}

// latest holds the value a source produced but has not published yet. It exists
// only for a channel whose source runs more often than the channel publishes:
// the source goroutine writes, the publish goroutine reads.
//
// The value is kept, not consumed. A legacy sensor re-read the state on every
// tick, so a publish interval that elapses without the source having run again
// sent the previous value rather than nothing.
type latest struct {
	mux   sync.Mutex
	value interface{}
	set   bool
}

func (this *latest) put(value interface{}) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.value = value
	this.set = true
}

func (this *latest) get() (interface{}, bool) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.value, this.set
}

// newGeneration indexes a definition. It reports what it cannot execute rather
// than dropping it silently: validation prevents all of these from being stored
// through the api, so anything found here came from a hand written document or
// from a future version of the format.
func newGeneration(def domain.Environment) *generation {
	result := &generation{
		def:        def,
		zones:      map[string]*zoneInfo{},
		assets:     map[string]*assetInfo{},
		commands:   map[commandKey]channelBinding{},
		deviceRefs: map[string]bool{},
	}
	result.addZones(def.Id, def.Zones, 1)
	return result
}

func (this *generation) addZones(envId string, zones []domain.Zone, depth int) {
	if depth > domain.MaxZoneDepth {
		util.Logger.Warn("zones nested deeper than the limit, the deeper ones are ignored",
			"environment", envId, "limit", domain.MaxZoneDepth)
		return
	}
	for _, zone := range zones {
		if zone.Id == "" {
			util.Logger.Warn("zone without an id is ignored", "environment", envId, "zone_name", zone.Name)
			continue
		}
		if _, duplicate := this.zones[zone.Id]; duplicate {
			util.Logger.Warn("duplicate zone id, the second one and everything below it is ignored",
				"environment", envId, "zone", zone.Id)
			continue
		}
		this.zones[zone.Id] = &zoneInfo{initialStates: zone.InitialStates}
		for _, asset := range zone.Assets {
			this.addAsset(envId, zone.Id, asset)
		}
		this.addZones(envId, zone.Zones, depth+1)
	}
}

func (this *generation) addAsset(envId string, zoneId string, asset domain.Asset) {
	if asset.Id == "" {
		util.Logger.Warn("asset without an id is ignored", "environment", envId, "asset_name", asset.Name)
		return
	}
	if _, duplicate := this.assets[asset.Id]; duplicate {
		util.Logger.Warn("duplicate asset id, the second one is ignored", "environment", envId, "asset", asset.Id)
		return
	}
	this.assets[asset.Id] = &assetInfo{zoneId: zoneId, initialStates: asset.InitialStates}
	if asset.ExternalRef != "" {
		this.deviceRefs[asset.ExternalRef] = true
	}
	ref := assetRef{id: asset.Id, externalRef: asset.ExternalRef}
	for _, channel := range asset.Channels {
		if channel.Source.Kind != domain.SourceScript || channel.Source.Script == nil {
			//validation rejects these on the way in, so this is a document that
			//bypassed the api or one written for a later version of the format
			util.Logger.Warn("channel source kind is not executed yet, the channel does nothing",
				"environment", envId, "asset", asset.Id, "channel", channel.Id, "kind", channel.Source.Kind)
			continue
		}
		binding := channelBinding{zoneId: zoneId, asset: ref, channel: channel, code: channel.Source.Script.Code}
		//seconds times time.Second overflows int64 beyond this limit and produces
		//a negative duration, which makes time.NewTicker panic and would take the
		//process down. Validation only rejects a negative interval.
		if channel.Source.IntervalSeconds > maxIntervalSeconds {
			util.Logger.Warn("source interval is out of range, it is ignored and the source runs when the channel publishes",
				"environment", envId, "channel", channel.Id,
				"interval_seconds", channel.Source.IntervalSeconds, "limit", maxIntervalSeconds)
		} else {
			binding.sourceInterval = channel.Source.IntervalSeconds
		}
		publishes := channel.Direction == domain.Sensor && channel.IntervalSeconds > 0
		if publishes && channel.IntervalSeconds > maxIntervalSeconds {
			util.Logger.Warn("channel interval is out of range, the channel does not tick",
				"environment", envId, "channel", channel.Id,
				"interval_seconds", channel.IntervalSeconds, "limit", maxIntervalSeconds)
			publishes = false
		}
		//a source interval makes the channel tick even when nothing is published
		//on a schedule: that is a state the other channels of the asset read
		if publishes || binding.sourceInterval > 0 {
			this.sensors = append(this.sensors, binding)
		}
		//a channel is addressable as a command whichever direction it has: the
		//legacy runtime ran the code of any service a command named, and the
		//migrated documents rely on exactly that
		if ref.externalRef == "" || channel.ExternalRef == "" {
			continue
		}
		key := commandKey{deviceRef: ref.externalRef, serviceRef: channel.ExternalRef}
		if previous, duplicate := this.commands[key]; duplicate {
			util.Logger.Warn("two channels claim the same platform service, the second one will not receive commands",
				"environment", envId, "channel", channel.Id, "conflicts_with", previous.channel.Id,
				"device_ref", key.deviceRef, "service_ref", key.serviceRef)
			continue
		}
		this.commands[key] = binding
	}
}

// seed fills in what the stored state does not have yet from the definition's
// initial states. It never removes anything: a zone or asset that is currently
// not in the definition may come back with the next edit, and its values are
// worth more than the few bytes they occupy.
//
// The definition is not touched, and the values are copied deeply: a script
// writing into a nested map must not reach the document that is exported.
func (this *environment) seed(gen *generation) {
	this.mux.Lock()
	defer this.mux.Unlock()

	//defensive: a store may hand out a state with nil maps
	this.state.EnvironmentId = this.id
	if this.state.Context == nil {
		this.state.Context = map[string]interface{}{}
	}
	if this.state.Zones == nil {
		this.state.Zones = map[string]map[string]interface{}{}
	}
	if this.state.Assets == nil {
		this.state.Assets = map[string]map[string]interface{}{}
	}

	if seedInto(this.state.Context, gen.def.Context) {
		this.dirty = true
	}
	//creating the bucket of a zone or an asset is deliberately NOT a change:
	//an empty one carries nothing, and marking it dirty would make every
	//restart write every environment once for no content at all
	for id, zone := range gen.zones {
		target, ok := this.state.Zones[id]
		if !ok {
			target = map[string]interface{}{}
			this.state.Zones[id] = target
		}
		if seedInto(target, zone.initialStates) {
			this.dirty = true
		}
	}
	for id, asset := range gen.assets {
		target, ok := this.state.Assets[id]
		if !ok {
			target = map[string]interface{}{}
			this.state.Assets[id] = target
		}
		if seedInto(target, asset.initialStates) {
			this.dirty = true
		}
	}
}

// seedInto copies the keys of initial that target does not have. An existing
// live value always wins: an initial state is a starting point, not a default
// that keeps being reapplied.
func seedInto(target map[string]interface{}, initial map[string]interface{}) (changed bool) {
	for key, value := range initial {
		if _, exists := target[key]; exists {
			continue
		}
		target[key] = copyValue(value)
		changed = true
	}
	return changed
}

// contextStates, zoneStates and assetStates return the map a script writes
// into. They may only be called with mux held, which is the case for everything
// reached from a running script.
func (this *environment) contextStates() map[string]interface{} {
	if this.state.Context == nil {
		this.state.Context = map[string]interface{}{}
	}
	return this.state.Context
}

func (this *environment) zoneStates(zoneId string) map[string]interface{} {
	if this.state.Zones == nil {
		this.state.Zones = map[string]map[string]interface{}{}
	}
	states, ok := this.state.Zones[zoneId]
	if !ok {
		states = map[string]interface{}{}
		this.state.Zones[zoneId] = states
	}
	return states
}

func (this *environment) assetStates(assetId string) map[string]interface{} {
	if this.state.Assets == nil {
		this.state.Assets = map[string]map[string]interface{}{}
	}
	states, ok := this.state.Assets[assetId]
	if !ok {
		states = map[string]interface{}{}
		this.state.Assets[assetId] = states
	}
	return states
}

// snapshot copies the state for a write. The copy is what goes to the store, so
// that the store cannot end up holding a map a script keeps writing into, and
// so that the mutex can be released before the database round trip.
//
// It must be called with mux held.
func (this *environment) snapshot() repo.RuntimeState {
	result := repo.RuntimeState{
		EnvironmentId: this.id,
		Context:       copyStates(this.state.Context),
		Zones:         make(map[string]map[string]interface{}, len(this.state.Zones)),
		Assets:        make(map[string]map[string]interface{}, len(this.state.Assets)),
	}
	for id, states := range this.state.Zones {
		result.Zones[id] = copyStates(states)
	}
	for id, states := range this.state.Assets {
		result.Assets[id] = copyStates(states)
	}
	return result
}

func copyStates(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = copyValue(value)
	}
	return out
}

// copyValue copies a state value deeply and replaces a non finite float by 0.
//
// The zero is not cosmetic: NaN and infinity are what a division by zero in a
// script produces, they are rejected by the validator on the way in, and a
// document containing one cannot be marshalled to json - so a state carrying
// one would make every reader of the state fail rather than the one channel
// that produced it. The in memory value is left as it is, so the script that
// produced it still sees what it wrote.
func copyValue(in interface{}) interface{} {
	switch value := in.(type) {
	case map[string]interface{}:
		return copyStates(value)
	case []interface{}:
		out := make([]interface{}, len(value))
		for i := range value {
			out[i] = copyValue(value[i])
		}
		return out
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0
		}
		return value
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return 0
		}
		return value
	default:
		//everything else a state can hold is immutable (number, string, bool, nil)
		return in
	}
}
