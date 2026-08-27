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
)

// MaxZoneDepth bounds how deep zones may nest. Four levels cover building,
// floor, unit and room; the limit exists so that a hand written or generated
// document cannot nest without bound.
const MaxZoneDepth = 8

// MaxNodes bounds zones plus assets plus channels in one environment. An
// imported document is untrusted input and must not be able to exhaust memory.
const MaxNodes = 10000

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

	// channelIds and channelRefs implement the second pass: a formula may
	// reference a channel defined later in the document, so the reference can
	// only be checked once the whole tree is indexed.
	channelIds  map[string]bool
	channelRefs []channelRef

	// assetSites and submeterRefs implement the same second pass for
	// submetered_by: the target asset may be defined later in the document or
	// in another zone entirely, and whether it shares a top level zone with
	// the reference is only known once every zone has been walked.
	//
	// assetSites maps an asset id to the index of the top level zone it lives
	// under, the same number every asset and reference below that zone
	// carries. The site is all the second pass needs from the target; where to
	// report a problem comes from the reference, which knows its own path.
	assetSites   map[string]int
	submeterRefs []submeterRef
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
	v := &validator{ids: map[string]string{}, channelIds: map[string]bool{}, assetSites: map[string]int{}}

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
		if !v.channelIds[ref.id] {
			v.fail(ref.path, "the referenced channel %q does not exist in this environment", ref.id)
		}
	}

	// submeterRefs: a target has to exist as an asset - a zone or channel id
	// is not a valid target even if the string happens to be one - and it has
	// to stay inside the reference's own top level zone. A meter tree is
	// modelled per site: a reference that leaves its top level zone is in
	// practice a misfiled asset, and refusing it puts that mistake in front of
	// the author instead of quietly building a tree across two sites.
	//
	// parents carries the whole reference, not just the target: the cycle pass
	// below reports at the path of the reference that closed the cycle, and
	// that path is already spelled out here rather than assembled a second
	// time from the target's own location.
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
	// claimId above, and the first asset under it keeps the entry there, so it
	// keeps the entry here too: letting the second one overwrite it would add
	// a second, misleading complaint about a site nobody referenced.
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
}

// checkSubmeterCycles finds cycles in the sub-metering tree. parents maps an
// asset id to the reference naming the asset that meters it too, already
// filtered to refs whose target exists and stays within its site - a missing
// or out-of-site target was reported above, and reporting it again as a cycle
// of its own would be confusing.
//
// A cycle here would never be walked by anything today: the graph mirror in
// lib/graphs is best effort and the repository rejects a graph containing a
// loop outright. Refusing it at validation time is still worth doing all the
// same - a stored cycle is a modelling mistake nothing else would ever
// surface.
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

func (this *validator) checkChannel(path string, channel Channel) {
	this.nodes++
	if strings.TrimSpace(channel.Name) == "" {
		this.fail(path+".name", "must not be empty")
	}
	if channel.Direction != Sensor && channel.Direction != Actuator {
		this.fail(path+".direction", "must be %q or %q, got %q", Sensor, Actuator, channel.Direction)
	}
	this.claimId(path+".id", channel.Id)
	if channel.Id != "" {
		this.channelIds[channel.Id] = true
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
	case "":
		this.fail(path+".kind", "must be set")
	default:
		this.fail(path+".kind", "unknown source kind %q", source.Kind)
	}
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
	case SourceScript, SourceFormula:
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
