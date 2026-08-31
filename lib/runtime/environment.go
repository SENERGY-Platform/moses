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
	"strings"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/formula"
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

	// state, dirty, removed and underHistory are guarded by mux.
	state   repo.RuntimeState
	dirty   bool
	removed bool

	// underHistory is true while a history run drives this environment on a
	// virtual clock. It is read under the same mutex the run replaces the state
	// with, so a state change either happens before the replacement or is
	// refused - never in between.
	underHistory bool

	// lastValues is the most recent numeric value each channel produced,
	// guarded by mux. This is what a formula's channel reference reads: a
	// profile or dataset channel writes no state, so its output only exists
	// here.
	lastValues map[string]float64

	// saves counts the Save calls that have left the mutex but not yet returned.
	// Remove waits for it before deleting the stored state, so that a flush in
	// flight cannot resurrect the document of a deleted environment.
	saves sync.WaitGroup

	// saveMux serialises the flushes of this environment, snapshot included.
	// Without it two flushes - the flusher's and the one of a handover - can read
	// two states and write them to the store in the opposite order, which leaves
	// the older one standing.
	saveMux sync.Mutex

	// commands counts the command dispatches in flight. A command does not run on
	// the environment context, so cancelling the runners does not stop it; a
	// caller that is about to replace the state waits for this instead. The
	// counter is only ever raised under mux and only while the environment takes
	// commands, so a wait that starts after the gate closed cannot race an Add.
	commands sync.WaitGroup

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

	// series carries the parsed uploads while the generation is indexed.
	series map[string][]dataset.Point

	// aggregateInputs maps an aggregate channel's id to the ids of the channels
	// it sums: the channels carrying the same characteristic on every asset
	// whose submetered_by names the aggregate's asset, in document order so the
	// sum is the same sequence of float additions on every start. The
	// indirection through ids keeps the aggregate from recursing over the tree -
	// executeAggregate reads only the last value each input published, one
	// level deep, whatever the level below is made of.
	aggregateInputs map[string][]string

	// candidates is scaffolding of the indexing pass, not part of the indexed
	// generation: the aggregate pass needs the whole document, because the
	// asset a submetered_by names may appear after the asset naming it, and it
	// must see exactly the assets addAsset accepted rather than re-deriving
	// that from the document. Set to nil when the pass is done.
	candidates []submeterCandidate

	// deviceRefs is every platform device this environment owns. It answers
	// "is this device mine" for HandleCommand, which has to be answerable even
	// for an asset whose channels cannot be executed.
	deviceRefs map[string]bool
}

// submeterCandidate is one accepted asset as the aggregate pass needs it:
// which asset meters it too, and its channels in document order.
type submeterCandidate struct {
	assetId      string
	submeteredBy string
	channels     []domain.Channel
}

type zoneInfo struct {
	initialStates map[string]interface{}
	timeConstants map[string]int64
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

	// cov is the resolved change trigger, nil for a channel that publishes on
	// its ticker alone. It is a copy rather than a pointer into the definition,
	// like the profile executeProfile takes, so a runner never reads a document
	// that could be edited under it.
	cov *covSettings

	// stepSeconds is the span one computation of this channel stands for: the
	// evaluation interval with a change trigger, the publish interval without
	// one. It is what a cumulative profile integrates over and what the spread
	// slot and a distributing replay are cut by, so the value is not counted in
	// publish intervals while it is computed far more often. Without a trigger
	// it is exactly channel.IntervalSeconds, which keeps every existing
	// document byte identical.
	stepSeconds int64

	// points is the parsed series of a dataset channel, loaded at start.
	points []dataset.Point

	// program is the compiled expression of a formula channel.
	program *formula.Program
}

// latest holds the value a source produced but has not published yet, for a
// channel whose source runs more often than the channel publishes: the source
// goroutine writes, the publish goroutine reads. The value is kept, not
// consumed, matching a legacy sensor that re-read state on every tick - a
// publish interval that elapses without the source running again sends the
// previous value rather than nothing.
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
func newGeneration(def domain.Environment, series map[string][]dataset.Point) *generation {
	result := &generation{
		def:             def,
		zones:           map[string]*zoneInfo{},
		assets:          map[string]*assetInfo{},
		commands:        map[commandKey]channelBinding{},
		deviceRefs:      map[string]bool{},
		aggregateInputs: map[string][]string{},
		series:          series,
	}
	result.addZones(def.Id, def.Zones, 1)
	//second pass: the meter tree only exists once the whole document has been
	//walked, and the set of ticking channels only once every runner-to-be is
	//known
	result.indexAggregates(def.Id)
	return result
}

// indexAggregates resolves every aggregate channel to the channels it sums. It
// runs after the zones are indexed, over exactly the assets that were accepted
// there, and reports what will contribute nothing rather than leaving a sum
// quietly short.
func (this *generation) indexAggregates(envId string) {
	defer func() { this.candidates = nil }()

	//children in document order: the order of the sum, and with it the order
	//of the float additions, must not depend on map iteration
	children := map[string][]int{}
	for i, candidate := range this.candidates {
		//a self reference is refused by validation; a hand written document
		//carrying one would make an aggregate sum its own last value, so it is
		//dropped here rather than fed back
		if candidate.submeteredBy == "" || candidate.submeteredBy == candidate.assetId {
			continue
		}
		children[candidate.submeteredBy] = append(children[candidate.submeteredBy], i)
	}

	ticking := map[string]bool{}
	for _, binding := range this.sensors {
		ticking[binding.channel.Id] = true
	}
	//a channel a command can reach produces a value too, whenever one arrives.
	//It is also why environment.carryLastValues keeps its remembered value
	//across a reload, so reporting it as contributing nothing would be wrong.
	commanded := map[string]bool{}
	for _, binding := range this.commands {
		commanded[binding.channel.Id] = true
	}

	for _, candidate := range this.candidates {
		for _, channel := range candidate.channels {
			if channel.Source.Kind != domain.SourceAggregate || channel.Id == "" {
				continue
			}
			if _, duplicate := this.aggregateInputs[channel.Id]; duplicate {
				//channel ids are unique per validation; if two channels share
				//one anyway they also share their entry in lastValues, so the
				//first one keeps its inputs like the first asset keeps its id
				util.Logger.Warn("duplicate aggregate channel id, the second one keeps the inputs of the first",
					"environment", envId, "asset", candidate.assetId, "channel", channel.Id)
				continue
			}
			//trimmed on both sides, here and on every sub-metered channel below:
			//validation only refuses a characteristic that is empty after
			//trimming, so comparing the raw values could make the aggregate miss
			//a match over nothing more than trailing whitespace.
			characteristic := strings.TrimSpace(channel.CharacteristicId)
			if characteristic == "" {
				//validation demands one: without it there is nothing to match
				//the sub-metered channels by, and matching "the ones that also
				//have none" would be a rule nobody wrote down
				util.Logger.Warn("aggregate channel without a characteristic, it sums nothing",
					"environment", envId, "asset", candidate.assetId, "channel", channel.Id)
				this.aggregateInputs[channel.Id] = nil
				continue
			}
			inputs := []string{}
			for _, index := range children[candidate.assetId] {
				child := this.candidates[index]
				for _, sub := range child.channels {
					if sub.Id == "" || sub.Id == channel.Id || strings.TrimSpace(sub.CharacteristicId) != characteristic {
						continue
					}
					//the aggregate channels of an intermediate level are summed
					//like any other channel: a nested tree adds up the totals
					//of the level below, which is what makes the tree work at
					//more than one depth
					inputs = append(inputs, sub.Id)
					switch {
					case ticking[sub.Id]:
					case commanded[sub.Id]:
						//not silent, and not zero either: it keeps the value it
						//last produced (carryLastValues) until the next command
						//moves it, so the total above it stands still with it
						util.Logger.Warn("a channel this aggregate sums has no tick of its own, it contributes the value it last produced until a command drives it",
							"environment", envId, "aggregate_channel", channel.Id,
							"channel", sub.Id, "asset", child.assetId)
					default:
						//nothing in this generation can produce a value for it,
						//and carryLastValues dropped whatever it had, so it
						//contributes exactly 0 for as long as this generation
						//runs - a sum that is silently short is worse than a
						//loud one
						util.Logger.Warn("a channel this aggregate sums has neither a tick nor a command it could arrive on, it will contribute nothing until it ticks",
							"environment", envId, "aggregate_channel", channel.Id,
							"channel", sub.Id, "asset", child.assetId)
					}
				}
			}
			if len(inputs) == 0 && len(children[candidate.assetId]) > 0 {
				//the tree is there and the sum is still zero: either the
				//characteristics do not match, or the sub-metered channels
				//carry none at all - which is what a document migrated from the
				//legacy format looks like (lib/repo/convert.go leaves
				//characteristic_id empty), and it publishes a plausible 0
				//forever without this line
				util.Logger.Warn("this aggregate has sub-metered assets but none of their channels carries its characteristic, it sums nothing",
					"environment", envId, "asset", candidate.assetId, "channel", channel.Id,
					"characteristic", characteristic, "submetered_assets", len(children[candidate.assetId]))
			}
			this.aggregateInputs[channel.Id] = inputs
		}
	}
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
		this.zones[zone.Id] = &zoneInfo{initialStates: zone.InitialStates, timeConstants: zone.TimeConstants}
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
	//recorded for the aggregate pass, which cannot run before the last asset of
	//the document has been seen
	this.candidates = append(this.candidates, submeterCandidate{
		assetId: asset.Id, submeteredBy: asset.SubmeteredBy, channels: asset.Channels,
	})
	ref := assetRef{id: asset.Id, externalRef: asset.ExternalRef}
	for _, channel := range asset.Channels {
		script := channel.Source.Kind == domain.SourceScript && channel.Source.Script != nil
		profile := channel.Source.Kind == domain.SourceProfile && channel.Source.Profile != nil
		//a dataset channel is executable only with its series loaded; a failed
		//load was already reported by the loader
		replay := channel.Source.Kind == domain.SourceDataset && len(this.series[channel.Id]) >= 2
		//an aggregate needs nothing loaded or compiled: its inputs are resolved
		//by indexAggregates below, and an aggregate over no children is a
		//meaningful channel too - a distribution meter without sub-meters
		//reads zero, which is a reading and not silence
		aggregate := channel.Source.Kind == domain.SourceAggregate
		//a schedule without states has no programme to run: it would publish the
		//value of a state that does not exist, so it is dropped here the way an
		//unloadable dataset is
		schedule := channel.Source.Kind == domain.SourceSchedule &&
			channel.Source.Schedule != nil && len(channel.Source.Schedule.States) > 0
		var program *formula.Program
		if channel.Source.Kind == domain.SourceFormula && channel.Source.Formula != nil {
			var err error
			program, err = formula.Compile(channel.Source.Formula.Expression, channel.Source.Formula.Inputs)
			if err != nil {
				//validation refuses this on the way in, so it bypassed the api
				util.Logger.Warn("the formula does not compile, the channel does nothing",
					attributes.ErrorKey, err, "environment", envId, "channel", channel.Id)
			}
		}
		if !script && !profile && !replay && !aggregate && !schedule && program == nil {
			//validation rejects these on the way in, so this is a document that
			//bypassed the api or one written for a later version of the format
			util.Logger.Warn("channel source kind is not executed yet, the channel does nothing",
				"environment", envId, "asset", asset.Id, "channel", channel.Id, "kind", channel.Source.Kind)
			continue
		}
		binding := channelBinding{zoneId: zoneId, asset: ref, channel: channel}
		if script {
			binding.code = channel.Source.Script.Code
		}
		if replay {
			binding.points = this.series[channel.Id]
		}
		binding.program = program
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
		//the span one computation stands for, overwritten below when a change
		//trigger moves the computation onto its own cadence
		binding.stepSeconds = channel.IntervalSeconds
		if cov, usable, reason := covOf(channel); reason != "" {
			//validation refuses every one of these, so the document bypassed the
			//api or was written for a later version of the format. Degrading to
			//the plain ticker is the honest fallback: the channel keeps
			//publishing, only not on change.
			util.Logger.Warn("the change trigger of this channel is unusable, it publishes on its interval alone",
				"environment", envId, "channel", channel.Id, "reason", reason)
		} else if usable {
			binding.cov = &cov
			binding.stepSeconds = cov.evalSeconds
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
// initial states. It never removes anything, since a zone or asset that is
// currently not in the definition may come back with the next edit. The
// definition itself is not touched, and the values are copied deeply, so a
// script writing into a nested map cannot reach the exported document.
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

// markUnderHistory closes the environment to everything that would mix the
// present into a run. It happens before the runners and the commands in flight
// are waited for, so that nothing new can enter behind the wait.
func (this *environment) markUnderHistory() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.underHistory = true
}

// enterCommand reserves a slot for one command dispatch, or refuses it and says
// why. The check and the Add are one operation under mux, which is what makes
// the wait in StartHistory complete: a dispatch that passed the check is counted
// before the gate can close behind it.
//
// Every caller that gets true must call leaveCommand when the dispatch is over.
func (this *environment) enterCommand() (bool, string) {
	this.mux.Lock()
	defer this.mux.Unlock()
	switch {
	case this.removed:
		return false, "the environment was removed while the command was in flight"
	case this.underHistory:
		return false, "a history run owns this environment"
	}
	this.commands.Add(1)
	return true, ""
}

func (this *environment) leaveCommand() { this.commands.Done() }

// markDirty forces the next flush to write, whatever the state looks like. The
// handover of a history run uses it: the state it hands over has to reach the
// store even when a flush that was already in flight cleared the flag.
func (this *environment) markDirty() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.dirty = true
}

// resetForHistory throws the live state away. Discarding it is the point of the
// mode: the run recomputes the environment from the virtual start, and a value
// left over from the live simulation would be a value from the future.
//
// The caller seeds the definition's initial states afterwards; the value cache
// is deliberately not carried, since the run fills it from its own first ticks.
func (this *environment) resetForHistory() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.underHistory = true
	this.state = repo.RuntimeState{
		EnvironmentId: this.id,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
	}
	this.lastValues = nil
	this.dirty = true
}

// endHistory releases the environment again. It runs whatever became of the run,
// so that a failed or cancelled one cannot leave the environment refusing every
// state change forever.
func (this *environment) endHistory() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.underHistory = false
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

// carryLastValues brings the value cache into a new generation: the
// environment object and lastValues survive a Reload, but the generation does
// not, so this is where the two are lined up again - a wrong entry here is
// invisible, since it is what every aggregate above that channel adds into
// its total.
//
// An entry is kept only for a channel that can still produce a value in the
// new generation (gen.sensors or gen.commands, the two paths through dispatch
// that write the cache); anything else would otherwise freeze on its last
// published value and keep being summed into every total above it, even after
// indexAggregates reports it contributes nothing. A cumulative profile
// channel's persisted meter reading is restored into the cache too, so an
// aggregate over cumulative children does not start at 0 and jump to the real
// total once every child has ticked once.
//
// It takes the mutex itself and must be called before the runners of the new
// generation start.
func (this *environment) carryLastValues(gen *generation) {
	this.mux.Lock()
	defer this.mux.Unlock()

	if len(this.lastValues) > 0 {
		writable := make(map[string]bool, len(gen.sensors)+len(gen.commands))
		for _, binding := range gen.sensors {
			writable[binding.channel.Id] = true
		}
		for _, binding := range gen.commands {
			writable[binding.channel.Id] = true
		}
		for id := range this.lastValues {
			if !writable[id] {
				delete(this.lastValues, id)
			}
		}
	}

	// the same prune for the persisted side: an entry only means something for
	// a channel that still compares against it, so once a trigger is removed or
	// the channel is gone it is a number nothing reads and would be written out
	// on every flush forever. Unlike lastValues this is stored, so dropping an
	// entry is a change and has to be flushed.
	if len(this.state.LastPublished) > 0 {
		compares := make(map[string]bool, len(gen.sensors))
		for _, binding := range gen.sensors {
			if binding.cov != nil {
				compares[binding.channel.Id] = true
			}
		}
		for id := range this.state.LastPublished {
			if !compares[id] {
				delete(this.state.LastPublished, id)
				this.dirty = true
			}
		}
	}

	// and the same prune again for the anchors of the declared programmes: an
	// entry is kept only for a channel that still runs a schedule in the new
	// generation, since otherwise the anchor is a start instant nothing reads,
	// written out on every flush forever, and would resurface as a stale
	// position if the source were ever switched back to a schedule.
	if len(this.state.ScheduleRuns) > 0 {
		scheduled := make(map[string]bool, len(gen.sensors))
		keep := func(binding channelBinding) {
			if binding.channel.Source.Kind == domain.SourceSchedule && binding.channel.Source.Schedule != nil {
				scheduled[binding.channel.Id] = true
			}
		}
		for _, binding := range gen.sensors {
			keep(binding)
		}
		//a channel a command can reach advances its programme when the command
		//arrives, the same reason the value cache keeps its entry
		for _, binding := range gen.commands {
			keep(binding)
		}
		for id := range this.state.ScheduleRuns {
			if !scheduled[id] {
				delete(this.state.ScheduleRuns, id)
				this.dirty = true
			}
		}
	}

	for _, binding := range gen.sensors {
		profile := binding.channel.Source.Profile
		if binding.channel.Source.Kind != domain.SourceProfile || profile == nil || !profile.Cumulative {
			continue
		}
		if _, known := this.lastValues[binding.channel.Id]; known {
			//an existing live value wins, like seedInto above. On a reload it is
			//the same number anyway: executeProfile writes the counter into the
			//state and publishes that same counter.
			continue
		}
		counter, ok := asFloat(this.state.Assets[binding.asset.id][binding.channel.Id])
		if !ok || math.IsNaN(counter) || math.IsInf(counter, 0) {
			//no reading yet, or one that is not a finite number: the channel
			//starts from zero, as it did before the seeding existed
			continue
		}
		if this.lastValues == nil {
			this.lastValues = map[string]float64{}
		}
		this.lastValues[binding.channel.Id] = counter
	}
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
	if len(this.state.Anchors) > 0 {
		result.Anchors = make(map[string]int64, len(this.state.Anchors))
		for id, anchor := range this.state.Anchors {
			result.Anchors[id] = anchor
		}
	}
	if len(this.state.LastPublished) > 0 {
		result.LastPublished = make(map[string]repo.PublishedValue, len(this.state.LastPublished))
		for id, published := range this.state.LastPublished {
			result.LastPublished[id] = published
		}
	}
	if len(this.state.ScheduleRuns) > 0 {
		result.ScheduleRuns = make(map[string]repo.ScheduleRun, len(this.state.ScheduleRuns))
		for id, run := range this.state.ScheduleRuns {
			result.ScheduleRuns[id] = run
		}
	}
	if len(this.state.Approaching) > 0 {
		result.Approaching = make(map[string]map[string]repo.Approach, len(this.state.Approaching))
		for zoneId, running := range this.state.Approaching {
			copied := make(map[string]repo.Approach, len(running))
			for key, approach := range running {
				copied[key] = approach
			}
			result.Approaching[zoneId] = copied
		}
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

// copyValue copies a state value deeply and replaces a non finite float by 0
// for storage: NaN and infinity, typically from a division by zero in a
// script, cannot be marshalled to json, and one would make every reader of
// the state fail rather than the one channel that produced it. The in memory
// value is left as it is, so the script that produced it still sees what it
// wrote.
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
