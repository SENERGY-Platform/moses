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
	"hash/fnv"
	"math"
	"sort"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/google/uuid"
)

// placeholderScript is inserted for a legacy service without code. An empty
// script is rejected by Validate, so the alternative would be a document that
// cannot be imported at all because of a channel that never produced a value in
// the legacy model either.
const placeholderScript = "// TODO: this channel has no source yet. the legacy service it was migrated from had no code."

// legacyPathPrefix marks a problem that points into the legacy world document
// rather than into the converted environment. Change routines only exist there,
// so there is no path in the new document that could be marked instead.
const legacyPathPrefix = "legacy:"

// FromLegacyWorld converts a legacy World into an Environment.
//
// The conversion is lossy in exactly one place, and that place is reported
// rather than hidden: the legacy model attaches change routines to worlds, rooms
// and devices, while the new model drives values only through a channel's
// Source. Every routine that cannot be mapped is returned as a Problem naming
// its path, its id and its interval. The caller must keep the legacy world
// document until those routines have been re-modelled as channel sources: their
// javascript is not carried into the result.
//
// Everything else is carried over, and the two fields that must never change -
// Asset.ExternalRef and Asset.ExternalTypeId, plus Channel.ExternalRef - are
// copied verbatim, because they are what keeps the platform devices and their
// timeseries attached to this environment.
//
// The returned problems are ordered by traversal, not by severity. Paths point
// into the converted document, except for those prefixed with "legacy:", which
// point into the input.
//
// The conversion does not validate: a caller that intends to store the result
// must run Validate on it. Problems and validation errors are different things.
// A legacy world with a name, at least one room and a device type on every
// device converts into a document that passes Validate; a world that lacks one
// of those produces a Problem here and a validation error there, because
// neither a device type nor a zone can be invented without guessing.
//
// The error return is reserved for a caller mistake (an unknown envType); a
// problem with the data is reported through problems.
func FromLegacyWorld(world state.World, envType EnvironmentType) (Environment, []Problem, error) {
	if !validEnvironmentType(envType) {
		return Environment{}, nil, fmt.Errorf("unknown environment type %q, expected one of %v", envType, environmentTypes())
	}
	c := &converter{ids: map[string]string{}}

	result := Environment{
		Id:   c.id("id", world.Id, ""),
		Name: c.name("name", world.Name, "unnamed environment"),
		Type: envType,
		// the owner is carried over: a migration must not change who owns the data
		Owner:   world.Owner,
		Seed:    seedFromId(world.Id),
		Context: copyStates(world.States),
		Zones:   []Zone{},
	}

	c.reportRoutines("", "world", world.ChangeRoutines)

	for _, roomKey := range sortedByName(world.Rooms, func(room *state.Room) string {
		if room == nil {
			return ""
		}
		return room.Name
	}) {
		room := world.Rooms[roomKey]
		if room == nil {
			// defensive: a nil entry is broken data, not a reason to lose the rest
			c.note(fmt.Sprintf("rooms[%s]", roomKey), true, "the legacy room is null and was skipped")
			continue
		}
		result.Zones = append(result.Zones, c.zone(fmt.Sprintf("zones[%d]", len(result.Zones)), roomKey, *room))
	}
	if len(result.Zones) == 0 {
		c.note("zones", false, "the legacy world has no rooms, so the environment has no zone; add at least one zone before storing it")
	}

	return result, c.problems, nil
}

// converter carries the problems found so far and the ids already handed out.
type converter struct {
	problems []Problem
	// ids maps an already used id to the path that uses it. Validate rejects a
	// duplicate id document wide, and the runtime indexes nodes by id, so a
	// duplicate in the legacy data must be resolved here rather than passed on.
	ids map[string]string
}

func (this *converter) note(path string, legacy bool, format string, args ...interface{}) {
	if legacy {
		path = legacyPathPrefix + path
	}
	this.problems = append(this.problems, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

// id picks the id of a converted node. The legacy id wins: it is a uuid and
// keeping it keeps every reference to the node stable. The legacy map key is the
// second choice, because the legacy runtime indexed nodes by exactly that key,
// which makes it the id the rest of the system already knows - falling back to a
// fresh uuid there would break a reference for no reason. Only if both are
// unusable is a new id generated, and that is reported, because it is the one
// case where a reference to the old id is lost.
func (this *converter) id(path string, legacyId string, mapKey string) string {
	collision, collidedWith := "", ""
	for _, candidate := range []string{strings.TrimSpace(legacyId), strings.TrimSpace(mapKey)} {
		if candidate == "" {
			continue
		}
		if previous, taken := this.ids[candidate]; taken {
			if collision == "" {
				collision, collidedWith = candidate, previous
			}
			continue
		}
		this.ids[candidate] = path
		if collision != "" {
			this.note(path, false, "the legacy id %v is already used by %v, so %v was used instead", collision, collidedWith, candidate)
		}
		return candidate
	}
	generated := uuid.NewString()
	this.ids[generated] = path
	if collision != "" {
		this.note(path, false, "the legacy id %v is already used by %v, so %v was generated instead", collision, collidedWith, generated)
	} else {
		this.note(path, false, "the legacy document has no id here, so %v was generated", generated)
	}
	return generated
}

// name substitutes a readable fallback for an empty or whitespace only name,
// because Validate rejects those and a nameless node is unusable in a ui.
func (this *converter) name(path string, legacyName string, fallback string) string {
	if strings.TrimSpace(legacyName) == "" {
		this.note(path, false, "the legacy name is empty, %q was substituted", fallback)
		return fallback
	}
	return legacyName
}

func (this *converter) zone(path string, mapKey string, room state.Room) Zone {
	result := Zone{
		Id:   this.id(path+".id", room.Id, mapKey),
		Name: this.name(path+".name", room.Name, "unnamed zone"),
		// the legacy model has no type information at all, and every world that
		// exists today is an industrial site, so every room becomes a hall. a
		// wrong but consistent default that the user corrects in one place beats
		// guessing per room from a name.
		Type:          ZoneHall,
		Tags:          []string{},
		InitialStates: copyStates(room.States),
		Zones:         []Zone{},
		Assets:        []Asset{},
	}

	this.reportRoutines(fmt.Sprintf("rooms[%s]", mapKey), "room", room.ChangeRoutines)

	for _, deviceKey := range sortedByName(room.Devices, func(device *state.Device) string {
		if device == nil {
			return ""
		}
		return device.Name
	}) {
		device := room.Devices[deviceKey]
		if device == nil {
			this.note(fmt.Sprintf("rooms[%s].devices[%s]", mapKey, deviceKey), true, "the legacy device is null and was skipped")
			continue
		}
		result.Assets = append(result.Assets, this.asset(fmt.Sprintf("%s.assets[%d]", path, len(result.Assets)), mapKey, deviceKey, *device))
	}
	return result
}

func (this *converter) asset(path string, roomKey string, mapKey string, device state.Device) Asset {
	result := Asset{
		Id:   this.id(path+".id", device.Id, mapKey),
		Name: this.name(path+".name", device.Name, "unnamed asset"),
		// the legacy model does not know what a device is, only that it exists.
		// machine is the default for the industrial sites that exist today and is
		// meant to be corrected by the user; nothing in the runtime depends on it.
		Kind: AssetMachine,
		// verbatim, never regenerated: these two are the link to the platform
		// device and its device type, and with them to the existing timeseries
		ExternalRef:    device.ExternalRef,
		ExternalTypeId: device.ExternalTypeId,
		InitialStates:  copyStates(device.States),
		Channels:       []Channel{},
	}

	if strings.TrimSpace(device.ExternalTypeId) == "" {
		this.note(path+".external_type_id", false, "the legacy device references no device type; it cannot be derived here and has to be set before the environment can be stored")
	}
	if strings.TrimSpace(device.ImageUrl) != "" {
		this.note(path, false, "the legacy device had the image %v, which the new model does not carry", device.ImageUrl)
	}

	for _, serviceKey := range sortedByName(device.Services, func(service state.Service) string { return service.Name }) {
		result.Channels = append(result.Channels, this.channel(fmt.Sprintf("%s.channels[%d]", path, len(result.Channels)), serviceKey, device.Services[serviceKey]))
	}

	//after the channels exist: a routine is attached to the channel that
	//publishes the state it writes, and keeps its own interval
	for _, key := range attachRoutines(result.Channels, device.ChangeRoutines) {
		routine := device.ChangeRoutines[key]
		id := routine.Id
		if id == "" {
			id = key
		}
		this.note(fmt.Sprintf("rooms[%s].devices[%s].change_routines[%s]", roomKey, mapKey, key), true,
			"the device change routine %v (interval %vs) writes no device state that a channel of this asset reads, so it could not be attached to one: its script was not migrated and has to be re-created as a channel source",
			id, routine.Interval)
	}
	return result
}

func (this *converter) channel(path string, mapKey string, service state.Service) Channel {
	interval := service.SensorInterval
	// the legacy model derived "is a sensor" from a positive interval, and that is
	// the only information available here
	direction := Actuator
	if interval > 0 {
		direction = Sensor
	}
	if interval < 0 {
		// a negative interval never ticked in the legacy runtime either, and
		// Validate rejects it. treating it as an actuator keeps the document
		// storable and visibly reports the change.
		this.note(path+".interval_seconds", false, "the legacy sensor_interval was negative (%d) and was reset to 0, the channel became an actuator", interval)
		interval = 0
	}

	code := service.Code
	if strings.TrimSpace(code) == "" {
		this.note(path+".source.script.code", false, "the legacy service has no code, a placeholder was inserted; the channel produces nothing until a source is defined")
		code = placeholderScript
	}

	return Channel{
		Id:        this.id(path+".id", service.Id, mapKey),
		Name:      this.name(path+".name", service.Name, "unnamed channel"),
		Direction: direction,
		// verbatim: this is the platform service the channel publishes to
		ExternalRef: service.ExternalRef,
		// TODO: CharacteristicId and Unit have to be resolved from the device
		// type's content variables via the device-repository. That is out of scope
		// here and deliberately left empty rather than guessed from the name.
		CharacteristicId: "",
		Unit:             "",
		IntervalSeconds:  interval,
		// every legacy service was javascript, so every channel becomes a script
		// source. the code is carried over verbatim and still uses the legacy
		// moses.* api, which the new runtime has to keep serving.
		Source: Source{Kind: SourceScript, Script: &ScriptSource{Code: code}},
	}
}

// reportRoutines reports the world and zone routines, which are not migrated by
// decision rather than by limitation: they evolve a zone state, and a zone has no
// channels. Zone level measurements are modelled as their own assets with their
// own channels instead of being translated.
func (this *converter) reportRoutines(pathPrefix string, scope string, routines map[string]state.ChangeRoutine) {
	path := "change_routines_not_migrated"
	if pathPrefix != "" {
		path = pathPrefix + ".change_routines_not_migrated"
	}
	keys := make([]string, 0, len(routines))
	for key := range routines {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		routine := routines[key]
		id := routine.Id
		if id == "" {
			id = key
		}
		this.note(fmt.Sprintf("%s[%s]", path, key), true,
			"the %v change routine %v (interval %vs) is deliberately not migrated: it evolves a %v state, and a %v has no channels. Model the measurement as an asset with its own channels instead",
			scope, id, routine.Interval, scope, scope)
	}
}

// sortedByName orders the keys of a legacy map by name and then by key. The
// legacy model stores rooms, devices and services in maps, whose iteration order
// is random: without a total order here, converting the same world twice would
// produce documents that differ in the order of their zones, assets and
// channels, and re-running the migration would look like a change.
func sortedByName[T any](in map[string]T, name func(T) string) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool {
		nameA, nameB := name(in[keys[a]]), name(in[keys[b]])
		if nameA != nameB {
			return nameA < nameB
		}
		return keys[a] < keys[b]
	})
	return keys
}

// copyStates copies a legacy state map deeply. The legacy runtime mutates its
// maps while it runs, under its own mutex; sharing one with the converted
// document would let a running simulation change a document that is being
// exported, and would race with it.
func copyStates(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = copyStateValue(value)
	}
	return out
}

func copyStateValue(in interface{}) interface{} {
	switch value := in.(type) {
	case map[string]interface{}:
		return copyStates(value)
	case []interface{}:
		out := make([]interface{}, len(value))
		for i := range value {
			out[i] = copyStateValue(value[i])
		}
		return out
	default:
		// everything else a state can hold is immutable (number, string, bool, nil)
		return in
	}
}

// seedFromId derives the seed from the world id instead of drawing a random one,
// so that converting the same world twice produces an identical document: a
// random or time based seed would make the migration unrepeatable and every
// re-run would look like a change. Different worlds still get different seeds.
func seedFromId(id string) int64 {
	hash := fnv.New64a()
	hash.Write([]byte(id))
	// masked to stay non negative: a seed is displayed and edited by users
	return int64(hash.Sum64() & math.MaxInt64)
}
