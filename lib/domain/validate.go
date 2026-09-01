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

package domain

import (
	"fmt"
	"github.com/SENERGY-Platform/moses/lib/formula"
	"math"
	"sort"
	"strings"
	"time"
)

// MaxZoneDepth bounds how deep zones may nest. Four levels cover building,
// floor, unit and room; the limit exists so that a hand written or generated
// document cannot nest without bound.
const MaxZoneDepth = 8

// MaxNodes bounds zones plus assets plus channels in one environment. An
// imported document is untrusted input and must not be able to exhaust memory.
const MaxNodes = 10000

// MaxScheduleStates bounds the steps of one schedule. The states are not nodes
// in the MaxNodes sense and would otherwise be an unbounded list on an
// untrusted document; the runtime walks all of them on every evaluation of the
// channel, so the bound is a runtime cost as much as a memory one, set well
// above anything a real plant needs.
const MaxScheduleStates = 256

// MaxScheduleDurationSeconds bounds one step at a year. The walk over whole
// cycles sums durations in int64 seconds, and an unbounded duration would let a
// hand written document overflow that sum rather than describe a machine.
const MaxScheduleDurationSeconds = int64(366 * 24 * 60 * 60)

// Problem is a single validation failure. Path points at the offending field in
// the document, so a caller can mark exactly that input instead of reporting
// that something somewhere is wrong.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (this Problem) String() string {
	return this.Path + ": " + this.Message
}

// ValidationError collects every problem found, rather than stopping at the
// first one: fixing an imported document one error per round trip is the
// difference between a usable import and an unusable one.
type ValidationError struct {
	Problems []Problem `json:"problems"`
}

func (this *ValidationError) Error() string {
	parts := make([]string, 0, len(this.Problems))
	for _, p := range this.Problems {
		parts = append(parts, p.String())
	}
	return "invalid environment: " + strings.Join(parts, "; ")
}

type validator struct {
	problems []Problem
	// ids is used to enforce document wide uniqueness: the runtime builds
	// lookup indexes by id, and a duplicate would make an update ambiguous.
	ids   map[string]string
	nodes int

	// channelByIds and channelRefs implement the second pass: a formula may
	// reference a channel defined later in the document, so the reference can
	// only be checked once the whole tree is indexed. The whole channel is kept
	// rather than only its id, because a dated change also has to be checked
	// against the source kind the channel it names is driven by.
	channelByIds map[string]Channel
	channelRefs  []channelRef

	// assetSites and submeterRefs implement the same second pass for
	// submetered_by: the target asset may be defined later in the document or
	// in another zone entirely, and whether it shares a top level zone with the
	// reference is only known once every zone has been walked.
	//
	// assetSites maps an asset id to the index of the top level zone it lives
	// under. The site is all the second pass needs from the target; where to
	// report a problem comes from the reference itself.
	assetSites   map[string]int
	submeterRefs []submeterRef

	// gateRefs implements the second pass for a schedule's gate: the context key
	// it waits for may be declared as a static context entry or driven by a
	// context source, and both of those are read from the top of the document
	// while the reference is found deep inside a zone.
	gateRefs []channelRef
}

type channelRef struct {
	path string
	id   string
}

type submeterRef struct {
	path    string
	assetId string
	target  string
	site    int
}

func (this *validator) fail(path string, format string, args ...interface{}) {
	this.problems = append(this.problems, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

func (this *validator) claimId(path string, id string) {
	if id == "" {
		// an empty id is allowed on input: the server assigns one
		return
	}
	if previous, taken := this.ids[id]; taken {
		this.fail(path, "duplicate id %v, already used at %v", id, previous)
		return
	}
	this.ids[id] = path
}

// Validate checks an environment for everything that would make it unusable or
// ambiguous. It returns a *ValidationError listing every problem, or nil.
func Validate(env Environment) error {
	v := &validator{ids: map[string]string{}, channelByIds: map[string]Channel{}, assetSites: map[string]int{}}

	if strings.TrimSpace(env.Name) == "" {
		v.fail("name", "must not be empty")
	}
	if !validEnvironmentType(env.Type) {
		v.fail("type", "unknown environment type %q, expected one of %v", env.Type, environmentTypes())
	}
	v.claimId("id", env.Id)
	v.checkStates("context", env.Context)
	for key, source := range env.ContextSources {
		v.checkContextSource("context_sources."+key, key, source)
	}

	if len(env.Zones) == 0 {
		v.fail("zones", "an environment needs at least one zone")
	}
	for i := range env.Zones {
		v.checkZone(fmt.Sprintf("zones[%d]", i), env.Zones[i], 1, i)
	}

	if v.nodes > MaxNodes {
		v.fail("", "environment has %d nodes, the limit is %d", v.nodes, MaxNodes)
	}
	for _, ref := range v.channelRefs {
		if _, exists := v.channelByIds[ref.id]; !exists {
			v.fail(ref.path, "the referenced channel %q does not exist in this environment", ref.id)
		}
	}

	// gateRefs: the key a schedule waits for has to be declared somewhere in
	// this environment, either as a static context entry or as a context source
	// driving it. The rule is about the document saying what it does, not about
	// what can write the key - a script or the state endpoint can still set an
	// undeclared key at runtime - so an undeclared gate reads as unreadable
	// rather than necessarily dead. The escape is one line, an initial 0 in
	// context.
	for _, ref := range v.gateRefs {
		if _, static := env.Context[ref.id]; static {
			continue
		}
		if _, driven := env.ContextSources[ref.id]; driven {
			continue
		}
		v.fail(ref.path, "the gate key %q is neither in context nor driven by a context source; declare it in context (an initial 0 is enough) even when a script or the state endpoint writes it, so the document shows what the gate reads", ref.id)
	}

	// submeterRefs: a target has to exist as an asset - a zone or channel id is
	// not a valid target even if the string matches - and stay inside the
	// reference's own top level zone, since a meter tree is modelled per site
	// and a reference crossing that boundary is in practice a misfiled asset.
	//
	// parents carries the whole reference, not just the target, so the cycle
	// pass below can report at the path of the reference that closed the cycle
	// without reassembling it from the target's location.
	parents := map[string]submeterRef{}
	for _, ref := range v.submeterRefs {
		targetSite, exists := v.assetSites[ref.target]
		if !exists {
			v.fail(ref.path, "the referenced asset %q does not exist in this environment", ref.target)
			continue
		}
		if targetSite != ref.site {
			v.fail(ref.path, "submetered_by must stay within the same top level zone: a meter tree is modelled per site, so a reference across that boundary is almost always an asset filed under the wrong zone")
			continue
		}
		if ref.assetId != "" {
			parents[ref.assetId] = ref
		}
	}
	v.checkSubmeterCycles(parents)

	// the third second pass: a dated change names a channel, a context source or
	// a context key, and whether it exists and what kind of source drives it is
	// only known once the whole tree has been walked
	v.checkTimeline(env)

	if len(v.problems) == 0 {
		return nil
	}
	sort.SliceStable(v.problems, func(a, b int) bool { return v.problems[a].Path < v.problems[b].Path })
	return &ValidationError{Problems: v.problems}
}

// checkZone walks one zone and everything below it. site is the index of the
// top level zone this one descends from - fixed at the top of Validate and
// unchanged by every recursive call below, so a submetered_by reference can
// later be checked against the site of both ends without re-walking the tree.
func (this *validator) checkZone(path string, zone Zone, depth int, site int) {
	this.nodes++
	if depth > MaxZoneDepth {
		this.fail(path, "zones are nested deeper than %d levels", MaxZoneDepth)
		return
	}
	if strings.TrimSpace(zone.Name) == "" {
		this.fail(path+".name", "must not be empty")
	}
	if !validZoneType(zone.Type) {
		this.fail(path+".type", "unknown zone type %q, expected one of %v", zone.Type, zoneTypes())
	}
	this.claimId(path+".id", zone.Id)
	this.checkStates(path+".initial_states", zone.InitialStates)
	for key, seconds := range zone.TimeConstants {
		if seconds < 0 {
			this.fail(path+".time_constants."+key, "must not be negative")
		}
	}

	for i := range zone.Zones {
		this.checkZone(fmt.Sprintf("%s.zones[%d]", path, i), zone.Zones[i], depth+1, site)
	}
	for i := range zone.Assets {
		this.checkAsset(fmt.Sprintf("%s.assets[%d]", path, i), zone.Assets[i], site)
	}
}

func (this *validator) checkAsset(path string, asset Asset, site int) {
	this.nodes++
	if strings.TrimSpace(asset.Name) == "" {
		this.fail(path+".name", "must not be empty")
	}
	if !validAssetKind(asset.Kind) {
		this.fail(path+".kind", "unknown asset kind %q, expected one of %v", asset.Kind, assetKinds())
	}
	this.claimId(path+".id", asset.Id)
	// registered for the submetered_by second pass below; an empty id is
	// assigned by the server later. A duplicate id is already reported by
	// claimId above, so the first asset keeps the entry here too - letting the
	// second overwrite it would add a misleading complaint about a site nobody
	// referenced.
	if _, taken := this.assetSites[asset.Id]; asset.Id != "" && !taken {
		this.assetSites[asset.Id] = site
	}
	// external_ref may be empty: the platform device is created by the server
	// when an asset is new. external_type_id however is a choice only the
	// author can make, and every channel's semantics derive from it.
	if strings.TrimSpace(asset.ExternalTypeId) == "" {
		this.fail(path+".external_type_id", "must reference a device type")
	}
	this.checkStates(path+".initial_states", asset.InitialStates)

	if asset.SubmeteredBy != "" {
		if asset.Id != "" && asset.SubmeteredBy == asset.Id {
			this.fail(path+".submetered_by", "an asset cannot be sub-metered by itself")
		} else {
			this.submeterRefs = append(this.submeterRefs, submeterRef{
				path:    path + ".submetered_by",
				assetId: asset.Id,
				target:  asset.SubmeteredBy,
				site:    site,
			})
		}
	}

	for i := range asset.Channels {
		this.checkChannel(fmt.Sprintf("%s.channels[%d]", path, i), asset.Channels[i])
	}
	this.checkAggregateOverlap(path, asset)
	this.checkScheduleKeys(path, asset)
}

// checkAggregateOverlap refuses a second reading of the same quantity on an
// asset that already totals that quantity over its sub-meters - typically a
// distribution meter carrying both its own kWh channel and an aggregate over
// the meters below it. Two channels of the same characteristic on one asset
// are indistinguishable to a reader, and an aggregate one level further up
// sums this asset's channels by characteristic, so the sub-tree is counted
// twice.
//
// The fix is to model the meter's own share as an asset of its own,
// sub-metered by this one and needing no device (docs/submetering.md), so each
// level is summed exactly once. Reported at the colliding channel rather than
// at the aggregate, which is the channel meant to stay.
func (this *validator) checkAggregateOverlap(path string, asset Asset) {
	//the first aggregate per characteristic owns it; every further sensor
	//channel carrying the same one is the collision. Trimmed, like the runtime
	//matches (lib/runtime/environment.go, indexAggregates): "kwh " and "kwh"
	//are the same characteristic and would collide just as badly.
	owner := map[string]int{}
	for i := range asset.Channels {
		if asset.Channels[i].Source.Kind != SourceAggregate {
			continue
		}
		characteristic := strings.TrimSpace(asset.Channels[i].CharacteristicId)
		//an aggregate without one is already reported by checkChannel, and it
		//sums nothing, so it collides with nothing either
		if characteristic == "" {
			continue
		}
		if _, taken := owner[characteristic]; !taken {
			owner[characteristic] = i
		}
	}
	if len(owner) == 0 {
		return
	}
	for i := range asset.Channels {
		//an actuator publishes no reading of its own, so it is not a second
		//value of the same quantity, and the aggregate above does not sum it
		//either
		if asset.Channels[i].Direction != Sensor {
			continue
		}
		characteristic := strings.TrimSpace(asset.Channels[i].CharacteristicId)
		if characteristic == "" {
			continue
		}
		if first, taken := owner[characteristic]; !taken || first == i {
			continue
		}
		this.fail(fmt.Sprintf("%s.channels[%d]", path, i),
			"this asset already carries an aggregate over characteristic %q, and two channels of the same quantity on one asset are indistinguishable to whoever reads them: an aggregate above this asset sums both, so the whole sub-tree below it is counted twice. Model the meter's own share as a sub-metered asset of its own below this one, which needs no device of its own",
			characteristic)
	}
}

// checkSubmeterCycles finds cycles in the sub-metering tree. parents maps an
// asset id to the reference naming the asset that meters it too, already
// filtered to refs whose target exists and stays within its site, since a
// missing or out-of-site target was reported above and reporting it again as a
// cycle would be confusing.
//
// A cycle here cannot hang the runtime - the aggregate source reads each
// channel's last published value rather than recursing (lib/runtime,
// executeAggregate) - but it does produce two totals that each include the
// other's previous value, so the sum grows without bound and looks like a
// plausible reading the whole way. That is worse than an error and worth
// refusing here.
//
// Standard three-color depth first search, iterated so every asset gets a
// chance to start a walk: white (unseen), gray (on the current walk's path),
// black (fully explored, already reported if it was ever part of a cycle).
func (this *validator) checkSubmeterCycles(parents map[string]submeterRef) {
	const white, gray, black = 0, 1, 2
	color := map[string]int{}

	var walk func(id string, path []string)
	walk = func(id string, path []string) {
		switch color[id] {
		case black:
			return
		case gray:
			// id is already on this path: the suffix starting at its first
			// occurrence is the cycle, everything before it just leads into
			// one and is not itself part of it.
			start := 0
			for i, member := range path {
				if member == id {
					start = i
					break
				}
			}
			cycle := append(append([]string{}, path[start:]...), id)
			for _, member := range path[start:] {
				// every member of the cycle points at the next one, so every
				// one of them has a reference here to report at
				this.fail(parents[member].path, "submetered_by forms a cycle: %s", strings.Join(cycle, " -> "))
			}
			return
		}
		color[id] = gray
		path = append(path, id)
		if next, ok := parents[id]; ok {
			walk(next.target, path)
		}
		color[id] = black
	}

	// sorted, not ranged: the map order would decide which member a walk
	// starts at, and with it the wording of the cycle message - the same
	// broken document would report "a -> b -> a" on one save and
	// "b -> a -> b" on the next.
	ids := make([]string, 0, len(parents))
	for id := range parents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white {
			walk(id, nil)
		}
	}
}

// timelineSlot is the uniqueness key of a dated change: one target may carry at
// most one value per instant, since which of two would apply follows from
// nothing the document says.
type timelineSlot struct {
	target TimelineTarget
	atUnix int64
}

// checkTimeline refuses a dated change that could not do what it reads like: an
// instant the simulation cannot compare on, a target nothing in this document
// has, a field the named source does not carry, or two values for one instant.
//
// Everything here is about the document being executable. A change that lies in
// the future is deliberately fine - a planned measure is the case the timeline
// exists for.
func (this *validator) checkTimeline(env Environment) {
	if len(env.Timeline) > MaxTimelineChanges {
		//refused without walking the entries: the list is untrusted input, and
		//reporting ten thousand problems about a document that is refused anyway
		//is the same denial of service in the response
		this.fail("timeline", "a timeline may carry at most %d changes, got %d", MaxTimelineChanges, len(env.Timeline))
		return
	}
	seen := map[timelineSlot]int{}
	for i := range env.Timeline {
		change := env.Timeline[i]
		path := fmt.Sprintf("timeline[%d]", i)
		this.checkTimelineAt(path+".at", change.At)
		target, err := ParseTimelineTarget(change.Target)
		if err != nil {
			this.fail(path+".target", "%s", err.Error())
			continue
		}
		this.checkTimelineTarget(path, env, target, change.Value)
		slot := timelineSlot{target: target, atUnix: change.At.Unix()}
		if previous, taken := seen[slot]; taken {
			this.fail(path, "timeline[%d] already changes %q at that instant, and which of the two applies follows from nothing the document says",
				previous, change.Target)
			continue
		}
		seen[slot] = i
	}
}

// checkTimelineAt refuses an instant the simulation could not compare against.
func (this *validator) checkTimelineAt(path string, at time.Time) {
	switch {
	case at.IsZero():
		this.fail(path, "must be set, as an RFC3339 timestamp")
	case at.Nanosecond() != 0:
		this.fail(path, "must be a whole second: every clock decision of the simulation is made on the second grid and the store truncates to milliseconds, so a fraction here would be one instant in the document and another one after a round trip")
	case at.Before(minTimelineTime) || at.After(maxTimelineTime):
		this.fail(path, "must lie between %s and %s", minTimelineTime.Format(time.RFC3339), maxTimelineTime.Format(time.RFC3339))
	}
}

// checkTimelineTarget resolves one target against the document and checks the
// value against the field it lands on.
func (this *validator) checkTimelineTarget(path string, env Environment, target TimelineTarget, value float64) {
	targetPath := path + ".target"
	switch target.Kind {
	case TimelineContext:
		if _, declared := env.Context[target.Ref]; !declared {
			this.fail(targetPath, "the context key %q is not declared in context; declare it there with its initial value, so the document shows where the dated value starts from", target.Ref)
			return
		}
		if _, driven := env.ContextSources[target.Ref]; driven {
			this.fail(targetPath, "the context key %q is driven by a context source, which writes it on every one of its ticks and would overwrite the dated value; change the parameters of that source instead", target.Ref)
			return
		}
		this.checkTimelineValue(path+".value", target.Field, value)
	case TimelineContextSource:
		source, declared := env.ContextSources[target.Ref]
		if !declared {
			this.fail(targetPath, "no context source of this environment writes the key %q", target.Ref)
			return
		}
		this.checkTimelineSourceField(path, target, source, value)
	case TimelineChannel:
		channel, declared := this.channelByIds[target.Ref]
		if !declared {
			this.fail(targetPath, "the referenced channel %q does not exist in this environment", target.Ref)
			return
		}
		switch target.Field {
		case TimelineStateValue, TimelineStateSpread, TimelineGateThreshold:
			if this.checkTimelineScheduleField(targetPath, target, channel) {
				this.checkTimelineValue(path+".value", target.Field, value)
			}
		default:
			this.checkTimelineSourceField(path, target, channel.Source, value)
		}
	}
}

// checkTimelineSourceField checks the three fields a channel and a context source
// have in common against the source that is actually declared there.
func (this *validator) checkTimelineSourceField(path string, target TimelineTarget, source Source, value float64) {
	targetPath := path + ".target"
	switch target.Field {
	case TimelineProfileBase, TimelineProfileSpread:
		if source.Kind != SourceProfile || source.Profile == nil {
			this.fail(targetPath, "the %s %q is driven by a %q source, so it carries no profile to change", timelineSubject(target.Kind), target.Ref, source.Kind)
			return
		}
	case TimelineDatasetScale:
		if source.Kind != SourceDataset || source.Dataset == nil {
			this.fail(targetPath, "the %s %q is driven by a %q source, so it carries no dataset to change", timelineSubject(target.Kind), target.Ref, source.Kind)
			return
		}
		if source.Dataset.Cumulative {
			//scale multiplies the reading, and a cumulative replay's reading
			//contains every loop it has already counted: scaling it from an
			//instant on would restate the whole meter rather than bend the curve
			//from there, which is a step where the author asked for a kink
			this.fail(targetPath, "the dataset of the %s %q is cumulative, and scaling a meter reading from an instant on would multiply everything it has already counted instead of bending the curve from there; model the change as a second channel", timelineSubject(target.Kind), target.Ref)
			return
		}
	}
	this.checkTimelineValue(path+".value", target.Field, value)
}

// checkTimelineScheduleField checks a target that names a schedule, and reports
// whether the value below it is still worth checking.
func (this *validator) checkTimelineScheduleField(targetPath string, target TimelineTarget, channel Channel) bool {
	if channel.Source.Kind != SourceSchedule || channel.Source.Schedule == nil {
		this.fail(targetPath, "the channel %q is driven by a %q source, so it runs no schedule to change", target.Ref, channel.Source.Kind)
		return false
	}
	schedule := channel.Source.Schedule
	if target.Field == TimelineGateThreshold {
		if schedule.Gate == nil {
			this.fail(targetPath, "the schedule of channel %q has no gate, so it has no threshold to change", target.Ref)
			return false
		}
		return true
	}
	for i := range schedule.States {
		if schedule.States[i].Name == target.State {
			return true
		}
	}
	this.fail(targetPath, "the schedule of channel %q has no state named %q", target.Ref, target.State)
	return false
}

// checkTimelineValue applies the rule of the field the change lands on: a spread
// is a percentage and cannot be negative, everything else only has to be a
// number that can be compared and stored.
func (this *validator) checkTimelineValue(path string, field TimelineField, value float64) {
	switch field {
	case TimelineProfileSpread, TimelineStateSpread:
		this.checkThreshold(path, value)
	default:
		this.checkFinite(path, value)
	}
}

func (this *validator) checkChannel(path string, channel Channel) {
	this.nodes++
	if strings.TrimSpace(channel.Name) == "" {
		this.fail(path+".name", "must not be empty")
	}
	if channel.Direction != Sensor && channel.Direction != Actuator {
		this.fail(path+".direction", "must be %q or %q, got %q", Sensor, Actuator, channel.Direction)
	}
	this.claimId(path+".id", channel.Id)
	//the first channel of a duplicated id keeps the entry, the way assetSites
	//does: claimId already reports the duplicate, and letting the second one win
	//would report a source kind nobody referenced on top of it
	if _, taken := this.channelByIds[channel.Id]; channel.Id != "" && !taken {
		this.channelByIds[channel.Id] = channel
	}
	if channel.Source.IntervalSeconds < 0 {
		this.fail(path+".source.interval_seconds", "must not be negative")
	}
	if channel.IntervalSeconds < 0 {
		this.fail(path+".interval_seconds", "must not be negative")
	}
	if channel.Direction == Actuator && channel.IntervalSeconds > 0 {
		this.fail(path+".interval_seconds", "an actuator is driven from outside and must not have an interval")
	}
	if channel.PublishOnChange != nil {
		this.checkPublishOnChange(path, channel)
	}
	this.checkFaults(path, channel)
	this.checkSource(path+".source", channel.Source)
	if channel.Source.Kind == SourceProfile && (channel.Direction != Sensor || channel.IntervalSeconds <= 0) {
		this.fail(path, "a profile source computes when the channel publishes, so the channel must be a sensor with an interval")
	}
	if channel.Source.Kind == SourceDataset && (channel.Direction != Sensor || channel.IntervalSeconds <= 0) {
		this.fail(path, "a dataset source replays when the channel publishes, so the channel must be a sensor with an interval")
	}
	if channel.Source.Kind == SourceFormula && (channel.Direction != Sensor || channel.IntervalSeconds <= 0) {
		this.fail(path, "a formula computes when the channel publishes, so the channel must be a sensor with an interval")
	}
	if channel.Source.Kind == SourceSchedule && (channel.Direction != Sensor || channel.IntervalSeconds <= 0) {
		this.fail(path, "a schedule computes when the channel publishes, so the channel must be a sensor with an interval")
	}
	if channel.Source.Kind == SourceAggregate {
		if channel.Direction != Sensor || channel.IntervalSeconds <= 0 {
			this.fail(path, "an aggregate sums when the channel publishes, so the channel must be a sensor with an interval")
		}
		//the characteristic is what picks the channels to sum out of the
		//sub-metered assets, so without one the aggregate has no defined set of
		//inputs at all - it would silently sum nothing
		if strings.TrimSpace(channel.CharacteristicId) == "" {
			this.fail(path+".characteristic_id", "an aggregate sums the channels of the sub-metered assets that carry the same characteristic, so it must name one")
		}
	}
}

// checkPublishOnChange refuses a trigger that cannot do what it promises: one
// that never fires, whose value is never looked at, or on a channel that
// publishes nothing at all. A document carrying one of them looks like event
// driven metering in the editor and is a plain ticker in the data.
func (this *validator) checkPublishOnChange(path string, channel Channel) {
	trigger := channel.PublishOnChange
	triggerPath := path + ".publish_on_change"

	if channel.Direction != Sensor {
		this.fail(triggerPath, "only a sensor publishes readings of its own, so only a sensor can publish them on change")
	}
	if channel.IntervalSeconds <= 0 {
		this.fail(path+".interval_seconds", "a channel publishing on change needs an interval as its heartbeat: it is the longest silence the channel allows, and without it a value that stops moving would never be sent again")
	}
	this.checkThreshold(triggerPath+".absolute", trigger.Absolute)
	this.checkThreshold(triggerPath+".relative", trigger.Relative)
	//NaN fails both comparisons, so a threshold that is not a number counts as
	//unset here as well as in the runtime
	if !(trigger.Absolute > 0) && !(trigger.Relative > 0) {
		this.fail(triggerPath, "at least one of absolute and relative must be greater than zero, otherwise nothing ever counts as a change and only the heartbeat publishes")
	}

	// exactly one evaluation cadence: the source's own interval, or this field.
	evaluate := trigger.EvaluateIntervalSeconds
	switch {
	case evaluate < 0:
		this.fail(triggerPath+".evaluate_interval_seconds", "must not be negative")
	case channel.Source.IntervalSeconds > 0 && evaluate > 0:
		this.fail(triggerPath+".evaluate_interval_seconds", "the source of this channel already carries an interval, and that is when the value is computed and therefore when it can be compared; a second cadence here would be a contradiction, so leave it at zero")
	case channel.Source.IntervalSeconds <= 0 && evaluate == 0:
		this.fail(triggerPath+".evaluate_interval_seconds", "a change can only be noticed when the value is computed, so a channel whose source has no interval of its own needs one here")
	}

	// and the value has to be looked at at least as often as the heartbeat
	// fires, or the trigger could never be the reason for a publish.
	evaluateEvery := evaluate
	cadencePath := triggerPath + ".evaluate_interval_seconds"
	if evaluateEvery <= 0 {
		evaluateEvery = channel.Source.IntervalSeconds
		cadencePath = path + ".source.interval_seconds"
	}
	if evaluateEvery > 0 && channel.IntervalSeconds > 0 && evaluateEvery > channel.IntervalSeconds {
		this.fail(cadencePath, "the value would be computed every %d seconds while the heartbeat fires every %d, so the heartbeat would always be first and the trigger would never publish anything; evaluate at least as often as the heartbeat",
			evaluateEvery, channel.IntervalSeconds)
	}
}

// checkFaults refuses an injected fault that could not do what it reads like: a
// defect that never occurs, one drawn faster than the channel is evaluated, one
// whose occurrence cannot be found again from the instant alone, or one on a
// channel that publishes nothing to disturb. Overlapping windows are deliberately
// allowed - they compose in document order.
func (this *validator) checkFaults(path string, channel Channel) {
	if len(channel.Faults) == 0 {
		return
	}
	faultsPath := path + ".faults"
	if len(channel.Faults) > MaxChannelFaults {
		//refused without walking the list, the way the timeline is: an untrusted
		//document must not be able to turn one refusal into thousands
		this.fail(faultsPath, "a channel may carry at most %d faults, got %d", MaxChannelFaults, len(channel.Faults))
		return
	}
	//reported once for the channel and not once per fault, and the walk goes on:
	//everything else about the faults is still worth naming in the same round trip
	if channel.Direction != Sensor || channel.IntervalSeconds <= 0 {
		this.fail(faultsPath, "a fault disturbs a reading, so it needs a sensor channel with an interval to disturb")
	}
	step := channelStepSeconds(channel)
	//the instant is the whole identity of the offset a meter exchange stores, so
	//two of them at one instant would share it and the second would restart the
	//register on the first one's offset
	exchanged := map[int64]int{}
	for i := range channel.Faults {
		path := fmt.Sprintf("%s[%d]", faultsPath, i)
		this.checkFault(path, channel, channel.Faults[i], step)
		fault := channel.Faults[i]
		if fault.Kind != FaultMeterExchange || fault.From.IsZero() {
			continue
		}
		if previous, taken := exchanged[fault.From.Unix()]; taken {
			this.fail(path+".from", "faults[%d] already exchanges the meter of this channel at that instant, and the two would restart one register from one stored offset; a meter is exchanged once per moment", previous)
			continue
		}
		exchanged[fault.From.Unix()] = i
	}
}

// checkFault applies the rules of one fault. The kind decides all of them, so an
// unknown one stops here rather than producing a list of complaints about fields
// nobody can say whether they belong.
func (this *validator) checkFault(path string, channel Channel, fault Fault, stepSeconds int64) {
	if !validFaultKind(fault.Kind) {
		this.fail(path+".kind", "unknown fault kind %q, expected one of %v", fault.Kind, faultKinds())
		return
	}
	windowed := !fault.From.IsZero() || !fault.To.IsZero()
	rated := fault.PerHour != 0 || fault.DurationSeconds != 0
	switch {
	case windowed && rated:
		this.fail(path, "a fault is either dated - from and to - or drawn at a rate - per_hour and duration_seconds - and this one carries both, so which of the two decides when it occurs follows from nothing the document says")
		return
	case !windowed && !rated:
		this.fail(path, "a fault needs either a window (from, and to unless it is a meter exchange) or a rate (per_hour and duration_seconds), otherwise it never occurs at all")
		return
	}
	if windowed {
		this.checkFaultWindow(path, fault)
	} else {
		if fault.Kind == FaultMeterExchange {
			this.fail(path+".per_hour", "a meter exchange happens at one instant and is not drawn at a rate; declare it with from alone")
		}
		this.checkFaultRate(path, fault, stepSeconds)
	}
	this.checkFaultFields(path, channel, fault)
}

// checkFaultWindow applies the two dated-change rules to a window: an instant the
// simulation can compare on, and a span that is not empty. A meter exchange is
// the one kind with no end - the new register keeps counting.
func (this *validator) checkFaultWindow(path string, fault Fault) {
	this.checkFaultAt(path+".from", fault.From)
	if fault.Kind == FaultMeterExchange {
		if !fault.To.IsZero() {
			this.fail(path+".to", "a meter exchange is one instant: the new register keeps counting from that moment on, so there is nothing for to to end")
		}
		return
	}
	this.checkFaultAt(path+".to", fault.To)
	if fault.From.IsZero() || fault.To.IsZero() {
		return
	}
	if !fault.From.Before(fault.To) {
		this.fail(path+".to", "must lie after from: to is exclusive, so an empty window is a fault that never occurs")
	}
}

// checkFaultAt is checkTimelineAt for a fault instant, and deliberately the same
// rule: both are compared through Unix() on the second grid, and the store
// truncates to milliseconds either way.
func (this *validator) checkFaultAt(path string, at time.Time) {
	switch {
	case at.IsZero():
		this.fail(path, "must be set, as an RFC3339 timestamp")
	case at.Nanosecond() != 0:
		this.fail(path, "must be a whole second: every clock decision of the simulation is made on the second grid and the store truncates to milliseconds, so a fraction here would be one instant in the document and another one after a round trip")
	case at.Before(minTimelineTime) || at.After(maxTimelineTime):
		this.fail(path, "must lie between %s and %s", minTimelineTime.Format(time.RFC3339), maxTimelineTime.Format(time.RFC3339))
	}
}

// checkFaultRate applies the rules a drawn occurrence has to satisfy to be
// reproducible from the instant alone: one draw per evaluation step decides
// whether an occurrence begins there, and a running one is found by looking back
// over the steps it can still cover.
func (this *validator) checkFaultRate(path string, fault Fault, stepSeconds int64) {
	switch {
	case math.IsNaN(fault.PerHour) || math.IsInf(fault.PerHour, 0):
		this.fail(path+".per_hour", "must be a finite number")
	case !(fault.PerHour > 0):
		this.fail(path+".per_hour", "must be greater than zero, otherwise the fault never occurs")
	}
	if fault.DurationSeconds < 1 {
		this.fail(path+".duration_seconds", "must be at least one second: an occurrence of no length is never observed")
		return
	}
	if stepSeconds <= 0 {
		return
	}
	//ceil in integers, and by division rather than by (duration+step-1)/step: an
	//absurd duration from a hand written document would overflow the sum
	lookback := fault.DurationSeconds / stepSeconds
	if fault.DurationSeconds%stepSeconds != 0 {
		lookback++
	}
	if lookback > MaxFaultLookbackSlots {
		this.fail(path+".duration_seconds", "an occurrence of %d seconds spans %d evaluation steps of %d seconds each, and a running one is found by looking back at most %d steps; shorten it, evaluate less often, or declare it as a window with from and to",
			fault.DurationSeconds, lookback, stepSeconds, MaxFaultLookbackSlots)
	}
	if fault.PerHour > 0 && fault.PerHour*float64(stepSeconds)/3600 > 1 {
		this.fail(path+".per_hour", "at %v occurrences per hour and one evaluation every %d seconds more than one occurrence would begin per step, which a single draw per step cannot express; lower the rate or evaluate more often",
			fault.PerHour, stepSeconds)
	}
}

// checkFaultFields checks the two numbers a fault carries against the kind that
// reads them. A field the kind ignores is refused rather than dropped: it is a
// defect the author described and the simulation would not produce.
func (this *validator) checkFaultFields(path string, channel Channel, fault Fault) {
	if fault.Kind == FaultSpike {
		switch {
		case math.IsNaN(fault.Factor) || math.IsInf(fault.Factor, 0):
			this.fail(path+".factor", "must be a finite number")
		case fault.Factor == 1:
			this.fail(path+".factor", "a factor of 1 leaves the reading as it is, so the spike would be invisible in the series; a factor of 0 is the sensor that reads nothing and is allowed")
		}
	} else if fault.Factor != 0 {
		this.fail(path+".factor", "only a spike scales the reading, and a %q fault would ignore this field", fault.Kind)
	}

	if fault.Kind != FaultMeterExchange {
		if fault.ResetTo != 0 {
			this.fail(path+".reset_to", "only a meter exchange restarts a register, and a %q fault would ignore this field", fault.Kind)
		}
		return
	}
	switch {
	case math.IsNaN(fault.ResetTo) || math.IsInf(fault.ResetTo, 0):
		this.fail(path+".reset_to", "must be a finite number")
	case fault.ResetTo < 0:
		this.fail(path+".reset_to", "must not be negative: it is the reading the new register starts at, and a meter does not count below zero")
	}
	if !CumulativeSource(channel.Source) {
		this.fail(path+".kind", "a meter exchange restarts a register, so it only applies to a channel whose reading counts up: a profile or a dataset with cumulative set")
	}
}

// checkThreshold rejects what would silently disable a threshold. NaN and
// infinity are what a generated document produces from a division, and neither
// compares the way its author expects: NaN is never greater than anything, and
// an infinite threshold is never exceeded.
func (this *validator) checkThreshold(path string, value float64) {
	switch {
	case math.IsNaN(value) || math.IsInf(value, 0):
		this.fail(path, "must be a finite number")
	case value < 0:
		this.fail(path, "must not be negative")
	}
}

func (this *validator) checkSource(path string, source Source) {
	set := []string{}
	if source.Script != nil {
		set = append(set, string(SourceScript))
	}
	if source.Profile != nil {
		set = append(set, string(SourceProfile))
	}
	if source.Dataset != nil {
		set = append(set, string(SourceDataset))
	}
	if source.Formula != nil {
		set = append(set, string(SourceFormula))
	}
	if source.Schedule != nil {
		set = append(set, string(SourceSchedule))
	}
	if len(set) > 1 {
		this.fail(path, "only one source variant may be set, found %v", set)
	}

	switch source.Kind {
	case SourceScript:
		if source.Script == nil {
			this.fail(path+".script", "must be set when kind is %q", SourceScript)
		} else if strings.TrimSpace(source.Script.Code) == "" {
			this.fail(path+".script.code", "must not be empty")
		}
	case SourceProfile:
		this.checkProfile(path, source)
	case SourceDataset:
		this.checkDataset(path, source)
	case SourceFormula:
		this.checkFormula(path, source)
	case SourceSchedule:
		this.checkSchedule(path, source)
	case SourceAggregate:
		this.checkAggregate(path, source, set)
	case "":
		this.fail(path+".kind", "must be set")
	default:
		this.fail(path+".kind", "unknown source kind %q", source.Kind)
	}
}

// checkAggregate: an aggregate has no variant of its own, so the check is what
// must NOT be there. The "only one variant" rule above does not cover this: a
// document with kind aggregate and exactly one foreign variant set passes it,
// and would be stored with a configuration nothing ever reads.
func (this *validator) checkAggregate(path string, source Source, set []string) {
	if len(set) > 0 {
		this.fail(path, "an aggregate has no configuration of its own, its inputs are the assets sub-metering this one, so remove the %v it carries", set)
	}
	if source.IntervalSeconds != 0 {
		this.fail(path+".interval_seconds", "an aggregate sums when the channel publishes and has no own interval")
	}
}

// checkSchedule refuses a programme that could not run the way it reads. Every
// rule here is a document that looks like a declared machine cycle in the
// editor and is something else in the data: a cycle without states, a step of
// no length, a gate on a key nobody writes, or a state key that lands on top of
// a value another channel of the same asset already stores there.
func (this *validator) checkSchedule(path string, source Source) {
	if source.Schedule == nil {
		this.fail(path+".schedule", "must be set when kind is %q", SourceSchedule)
		return
	}
	if source.IntervalSeconds != 0 {
		this.fail(path+".interval_seconds", "a schedule computes when the channel publishes and has no own interval")
	}
	schedule := source.Schedule
	schedulePath := path + ".schedule"

	//the state key is the whole point of the source: without it the running
	//state is an anonymous number again, which is what a script already was
	this.checkScheduleKey(schedulePath+".state_key", schedule.StateKey,
		"it is where the name of the running state is written, and a schedule nobody can read the state of is a profile with extra steps")

	if len(schedule.States) == 0 {
		this.fail(schedulePath+".states", "a schedule needs at least one state, otherwise there is no programme to run")
	}
	if len(schedule.States) > MaxScheduleStates {
		this.fail(schedulePath+".states", "a schedule may have at most %d states, got %d", MaxScheduleStates, len(schedule.States))
	}

	if schedule.Gate != nil {
		gatePath := schedulePath + ".gate"
		switch {
		case strings.TrimSpace(schedule.Gate.ContextKey) == "":
			this.fail(gatePath+".context_key", "a gate must name the context key it waits for, otherwise the schedule reads 0 and never starts")
		case schedule.Gate.ContextKey != strings.TrimSpace(schedule.Gate.ContextKey):
			this.fail(gatePath+".context_key", "must not begin or end with whitespace: the runtime looks the key up in the context exactly as it stands, so %q is a gate that never finds the calendar the editor shows it waiting for", schedule.Gate.ContextKey)
		default:
			//checked in the second pass: the key may be driven by a context
			//source declared at the top of a document whose zones are walked here
			this.gateRefs = append(this.gateRefs, channelRef{path: gatePath + ".context_key", id: schedule.Gate.ContextKey})
		}
		//a threshold may be negative - a gate on a temperature is a legitimate
		//shape - but it has to be comparable at all
		this.checkFinite(gatePath+".threshold", schedule.Gate.Threshold)
	}

	names := map[string]int{}
	for i := range schedule.States {
		state := schedule.States[i]
		statePath := fmt.Sprintf("%s.states[%d]", schedulePath, i)
		name := state.Name
		switch {
		case strings.TrimSpace(name) == "":
			this.fail(statePath+".name", "must not be empty: the name is what the state key carries and what a formula and a dashboard read")
		case name != strings.TrimSpace(name):
			//the name is written into the asset state verbatim, so it is the
			//string every comparison downstream is made against
			this.fail(statePath+".name", "must not begin or end with whitespace: %q is the value a formula and a dashboard would have to compare against, and it reads as %q everywhere a human looks at it", name, strings.TrimSpace(name))
		case schedule.Gate != nil && name == ScheduleClosedState:
			this.fail(statePath+".name", "%q is the name a gated schedule writes while its gate is closed, so a state of that name could not be told apart from the machine standing still", ScheduleClosedState)
		default:
			if previous, taken := names[name]; taken {
				this.fail(statePath+".name", "duplicate state name %q, already used by states[%d]: the name is the only thing a reader has to tell two steps apart", name, previous)
			} else {
				names[name] = i
			}
		}
		switch {
		case state.DurationSeconds <= 0:
			this.fail(statePath+".duration_seconds", "must be greater than zero, a step of no length is never reached")
		case state.DurationSeconds > MaxScheduleDurationSeconds:
			this.fail(statePath+".duration_seconds", "must be at most %d seconds (a year)", MaxScheduleDurationSeconds)
		}
		//100 percent would allow a drawn duration of zero, and the runtime would
		//have to invent a floor the document never mentions
		if !(state.DurationSpreadPercent >= 0 && state.DurationSpreadPercent < 100) {
			this.fail(statePath+".duration_spread_percent", "must be at least 0 and less than 100, otherwise a cycle could draw a step of no length at all")
		}
		this.checkThreshold(statePath+".spread_percent", state.SpreadPercent)
		this.checkFinite(statePath+".value", state.Value)
		for _, key := range sortedWriteKeys(state.StateWrites) {
			this.checkScheduleKey(fmt.Sprintf("%s.state_writes.%s", statePath, key), key,
				"a state write with no key writes nothing")
			this.checkFinite(fmt.Sprintf("%s.state_writes.%s", statePath, key), state.StateWrites[key])
		}
	}
}

// checkScheduleKey applies the state key rules to a key a schedule writes.
// damage says what an empty one costs, which differs per field.
func (this *validator) checkScheduleKey(path string, key string, damage string) {
	if strings.TrimSpace(key) == "" {
		this.fail(path, "must not be empty: %s", damage)
		return
	}
	//the key is written into the asset state exactly as it stands here, while a
	//formula input, a dashboard tile and the state endpoint all name it trimmed:
	//a stray space is a value nothing that reads the document can address
	if key != strings.TrimSpace(key) {
		this.fail(path, "must not begin or end with whitespace: the asset state would carry %q, which is not the key %q anything reading it would name", key, strings.TrimSpace(key))
	}
	//the same rule checkStates applies to every other state key: mongodb reads
	//both characters as structure rather than as a name
	if strings.ContainsAny(key, ".$") {
		this.fail(path, "a state key must not contain '.' or '$'")
	}
}

// checkFinite rejects a number that cannot be compared or stored. It is the
// checkThreshold rule without the sign: NaN loses every comparison it takes
// part in, an infinity cannot be marshalled, and both are what a generated
// document produces from a division.
func (this *validator) checkFinite(path string, value float64) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		this.fail(path, "must be a finite number")
	}
}

// checkScheduleKeys refuses two writers of one asset state key.
//
// The asset state map is flat and shared: a cumulative profile stores its
// meter reading under its own channel id, and every schedule of the asset
// writes its state name and declared values there too. A collision is not ill
// formed - the last writer of the tick simply wins - but the loser's value
// silently stops moving or jumps between a count and a string, which is worth
// refusing at store time instead.
//
// Reported at the later channel: the first claim on a key keeps it, the same
// way checkAggregateOverlap reports at the channel that collides.
func (this *validator) checkScheduleKeys(path string, asset Asset) {
	channelIds := map[string]bool{}
	for i := range asset.Channels {
		if asset.Channels[i].Id != "" {
			channelIds[asset.Channels[i].Id] = true
		}
	}
	claimed := map[string]int{}
	for i := range asset.Channels {
		channel := asset.Channels[i]
		if channel.Source.Kind != SourceSchedule || channel.Source.Schedule == nil {
			continue
		}
		schedule := channel.Source.Schedule
		channelPath := fmt.Sprintf("%s.channels[%d].source.schedule", path, i)

		//sorted and deduplicated: a key declared by several states is the union
		//semantics working as intended and not a collision, and a map iteration
		//would order the problems of one document differently on every save
		writes := map[string]bool{}
		for j := range schedule.States {
			for key := range schedule.States[j].StateWrites {
				writes[key] = true
			}
		}
		keys := []string{}
		for key := range writes {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		if writes[schedule.StateKey] {
			this.fail(channelPath+".state_key",
				"the state key %q is also written as a state write of this schedule, so the name of the running state would be overwritten by a number on every evaluation",
				schedule.StateKey)
		}
		if schedule.StateKey != "" {
			keys = append([]string{schedule.StateKey}, keys...)
		}

		for _, key := range keys {
			if channelIds[key] {
				this.fail(channelPath,
					"this schedule writes the asset state key %q, which is the channel id of a channel of the same asset: a cumulative profile stores its meter reading under exactly that key, so the two would overwrite each other",
					key)
				continue
			}
			if previous, taken := claimed[key]; taken {
				this.fail(channelPath,
					"this schedule writes the asset state key %q, which the schedule of channels[%d] already writes: whichever of them evaluates last would win, and neither document says which",
					key, previous)
				continue
			}
			claimed[key] = i
		}
	}
}

// sortedKeys is how a map is walked when the order decides the order of the
// reported problems: the same document has to produce the same message list on
// every save.
func sortedWriteKeys(values map[string]float64) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// checkStates rejects values that cannot survive the round trip through bson
// and json. NaN and infinity were previously silently rewritten to zero, which
// hid the modelling error that produced them.
// checkProfile: a profile computes when the channel publishes, so a channel
// that never publishes would be stored and silently produce nothing - which is
// exactly what refusing the other source kinds is meant to prevent.
func (this *validator) checkProfile(path string, source Source) {
	if source.Profile == nil {
		this.fail(path+".profile", "must be set when kind is %q", SourceProfile)
		return
	}
	if source.IntervalSeconds != 0 {
		this.fail(path+".interval_seconds", "a profile computes when the channel publishes and has no own interval")
	}
	p := source.Profile
	if len(p.HourFactors) != 0 && len(p.HourFactors) != 24 {
		this.fail(path+".profile.hour_factors", "must have 24 entries or be empty, got %d", len(p.HourFactors))
	}
	if len(p.WeekdayFactors) != 0 && len(p.WeekdayFactors) != 7 {
		this.fail(path+".profile.weekday_factors", "must have 7 entries (starting at monday) or be empty, got %d", len(p.WeekdayFactors))
	}
	if p.SpreadPercent < 0 {
		this.fail(path+".profile.spread_percent", "must not be negative")
	}
}

// checkDataset: only uploaded files play today. The platform and endpoint
// origins stay refused like the formula kind - accepting one would store a
// channel that silently never produces a value.
func (this *validator) checkDataset(path string, source Source) {
	if source.IntervalSeconds != 0 {
		this.fail(path+".interval_seconds", "a dataset replays when the channel publishes and has no own interval")
	}
	this.checkDatasetFields(path, source)
}

// checkDatasetFields is the interval-free part: a channel dataset replays on
// the publish tick, a context dataset on its own interval, the fields are the
// same either way.
func (this *validator) checkDatasetFields(path string, source Source) {
	if source.Dataset == nil {
		this.fail(path+".dataset", "must be set when kind is %q", SourceDataset)
		return
	}
	d := source.Dataset
	switch d.Origin {
	case OriginFile:
		if strings.TrimSpace(d.Ref) == "" {
			this.fail(path+".dataset.ref", "must name the uploaded dataset")
		}
		if d.Window != "" {
			this.fail(path+".dataset.window", "an uploaded dataset is replayed whole, the window only applies to a platform timeseries")
		}
	case OriginPlatform:
		if strings.TrimSpace(d.Ref) == "" {
			this.fail(path+".dataset.ref", "must name the platform device")
		}
		if strings.TrimSpace(d.ServiceRef) == "" {
			this.fail(path+".dataset.service_ref", "must name the service of the device")
		}
		if strings.TrimSpace(d.Column) == "" {
			this.fail(path+".dataset.column", "must name the path of the output variable, e.g. \"value\"")
		}
		if _, err := ParseWindow(d.Window); err != nil {
			this.fail(path+".dataset.window", "%s", err.Error())
		}
	case OriginEndpoint:
		this.fail(path+".dataset.origin", "origin %q is part of the format but not executed yet", d.Origin)
	default:
		this.fail(path+".dataset.origin", "unknown origin %q", d.Origin)
	}
	switch d.Resample {
	case ResampleHold, ResampleLinear, ResampleDistribute:
	case "":
		this.fail(path+".dataset.resample", "must be %q, %q or %q", ResampleHold, ResampleLinear, ResampleDistribute)
	default:
		this.fail(path+".dataset.resample", "unknown resample mode %q", d.Resample)
	}
	switch d.Anchor {
	case AnchorLoop, AnchorOriginal:
	case "":
		this.fail(path+".dataset.anchor", "must be %q or %q", AnchorLoop, AnchorOriginal)
	default:
		this.fail(path+".dataset.anchor", "unknown anchor mode %q", d.Anchor)
	}
}

// checkContextSource: a context source ticks on its own interval and writes
// one key; only the declarative kinds run there - a script has no channel
// scope to live in, and a formula reading the context it writes would be a
// cycle nobody can reason about.
func (this *validator) checkContextSource(path string, key string, source Source) {
	if strings.TrimSpace(key) == "" {
		this.fail(path, "the context key must not be empty")
	}
	switch source.Kind {
	case SourceProfile:
		if source.Profile == nil {
			this.fail(path+".profile", "must be set when kind is %q", SourceProfile)
			return
		}
		p := source.Profile
		if len(p.HourFactors) != 0 && len(p.HourFactors) != 24 {
			this.fail(path+".profile.hour_factors", "must have 24 entries or be empty, got %d", len(p.HourFactors))
		}
		if len(p.WeekdayFactors) != 0 && len(p.WeekdayFactors) != 7 {
			this.fail(path+".profile.weekday_factors", "must have 7 entries (starting at monday) or be empty, got %d", len(p.WeekdayFactors))
		}
		if p.SpreadPercent < 0 {
			this.fail(path+".profile.spread_percent", "must not be negative")
		}
	case SourceDataset:
		this.checkDatasetFields(path, source)
	case SourceScript, SourceFormula, SourceAggregate, SourceSchedule:
		//named rather than left to the unknown-kind default: these kinds exist,
		//they are simply not available here, and "unknown" would send their
		//author looking for a typo. An aggregate and a schedule both need an
		//asset that a context key has none of, and a schedule's gate reading
		//the context while it writes it would also be a cycle.
		this.fail(path+".kind", "source kind %q is not supported for context sources", source.Kind)
	case "":
		this.fail(path+".kind", "must be set")
	default:
		this.fail(path+".kind", "unknown source kind %q", source.Kind)
	}
	if source.IntervalSeconds <= 0 {
		this.fail(path+".interval_seconds", "a context source has no publish tick to piggyback on, it needs its own interval")
	}
}

// checkFormula compiles the expression and trial-runs it with every input at
// zero, so a broken formula is refused at store time with a message instead of
// failing on every tick. Channel references are collected for the second pass,
// because a formula may name a channel defined later in the document.
func (this *validator) checkFormula(path string, source Source) {
	if source.Formula == nil {
		this.fail(path+".formula", "must be set when kind is %q", SourceFormula)
		return
	}
	if source.IntervalSeconds != 0 {
		this.fail(path+".interval_seconds", "a formula computes when the channel publishes and has no own interval")
	}
	if _, err := formula.Compile(source.Formula.Expression, source.Formula.Inputs); err != nil {
		this.fail(path+".formula", "%s", err.Error())
		return
	}
	for _, ref := range source.Formula.Inputs {
		if id, ok := strings.CutPrefix(ref, formula.RefChannel); ok {
			this.channelRefs = append(this.channelRefs, channelRef{path: path + ".formula.inputs", id: id})
		}
	}
}

func (this *validator) checkStates(path string, states map[string]interface{}) {
	for key, value := range states {
		if key == "" {
			this.fail(path, "state keys must not be empty")
			continue
		}
		if strings.ContainsAny(key, ".$") {
			this.fail(path+"."+key, "state keys must not contain '.' or '$'")
		}
		if f, ok := asFloat(value); ok {
			if math.IsNaN(f) {
				this.fail(path+"."+key, "must be a number, got NaN")
			} else if math.IsInf(f, 0) {
				this.fail(path+"."+key, "must be finite, got infinity")
			}
		}
	}
}

func asFloat(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	}
	return 0, false
}

func validEnvironmentType(t EnvironmentType) bool {
	for _, known := range environmentTypes() {
		if t == known {
			return true
		}
	}
	return false
}

func environmentTypes() []EnvironmentType {
	return []EnvironmentType{IndustrialSite, OfficeBuilding, ApartmentBuilding, SingleFamilyHome, Apartment}
}

func validZoneType(t ZoneType) bool {
	for _, known := range zoneTypes() {
		if t == known {
			return true
		}
	}
	return false
}

func zoneTypes() []ZoneType {
	return []ZoneType{ZoneSite, ZoneBuilding, ZoneFloor, ZoneUnit, ZoneHall, ZoneRoom}
}

func validAssetKind(k AssetKind) bool {
	for _, known := range assetKinds() {
		if k == known {
			return true
		}
	}
	return false
}

func assetKinds() []AssetKind {
	return []AssetKind{AssetMeter, AssetInverter, AssetMachine, AssetSensor, AssetActuator}
}
