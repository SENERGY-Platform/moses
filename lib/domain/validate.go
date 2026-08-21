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
	v := &validator{ids: map[string]string{}}

	if strings.TrimSpace(env.Name) == "" {
		v.fail("name", "must not be empty")
	}
	if !validEnvironmentType(env.Type) {
		v.fail("type", "unknown environment type %q, expected one of %v", env.Type, environmentTypes())
	}
	v.claimId("id", env.Id)
	v.checkStates("context", env.Context)

	if len(env.Zones) == 0 {
		v.fail("zones", "an environment needs at least one zone")
	}
	for i := range env.Zones {
		v.checkZone(fmt.Sprintf("zones[%d]", i), env.Zones[i], 1)
	}

	if v.nodes > MaxNodes {
		v.fail("", "environment has %d nodes, the limit is %d", v.nodes, MaxNodes)
	}

	if len(v.problems) == 0 {
		return nil
	}
	sort.SliceStable(v.problems, func(a, b int) bool { return v.problems[a].Path < v.problems[b].Path })
	return &ValidationError{Problems: v.problems}
}

func (this *validator) checkZone(path string, zone Zone, depth int) {
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

	for i := range zone.Zones {
		this.checkZone(fmt.Sprintf("%s.zones[%d]", path, i), zone.Zones[i], depth+1)
	}
	for i := range zone.Assets {
		this.checkAsset(fmt.Sprintf("%s.assets[%d]", path, i), zone.Assets[i])
	}
}

func (this *validator) checkAsset(path string, asset Asset) {
	this.nodes++
	if strings.TrimSpace(asset.Name) == "" {
		this.fail(path+".name", "must not be empty")
	}
	if !validAssetKind(asset.Kind) {
		this.fail(path+".kind", "unknown asset kind %q, expected one of %v", asset.Kind, assetKinds())
	}
	this.claimId(path+".id", asset.Id)
	// external_ref may be empty: the platform device is created by the server
	// when an asset is new. external_type_id however is a choice only the
	// author can make, and every channel's semantics derive from it.
	if strings.TrimSpace(asset.ExternalTypeId) == "" {
		this.fail(path+".external_type_id", "must reference a device type")
	}
	this.checkStates(path+".initial_states", asset.InitialStates)

	for i := range asset.Channels {
		this.checkChannel(fmt.Sprintf("%s.channels[%d]", path, i), asset.Channels[i])
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
	case SourceProfile, SourceDataset, SourceFormula:
		// the document format carries these from the start so that exports stay
		// stable, but nothing executes them yet. accepting one here would store
		// a channel that silently never produces a value.
		this.fail(path+".kind", "source kind %q is part of the format but not executed yet", source.Kind)
	case "":
		this.fail(path+".kind", "must be set")
	default:
		this.fail(path+".kind", "unknown source kind %q", source.Kind)
	}
}

// checkStates rejects values that cannot survive the round trip through bson
// and json. NaN and infinity were previously silently rewritten to zero, which
// hid the modelling error that produced them.
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
