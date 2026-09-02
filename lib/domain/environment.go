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

// Package domain holds the environment model: the definition of a simulated
// site, building or apartment and everything it contains.
//
// An environment is one document and carries no live measurements - those live
// in RuntimeState, so ticking a channel does not rewrite the definition.
package domain

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

type EnvironmentType string

const (
	IndustrialSite    EnvironmentType = "industrial_site"
	OfficeBuilding    EnvironmentType = "office_building"
	ApartmentBuilding EnvironmentType = "apartment_building"
	SingleFamilyHome  EnvironmentType = "single_family_home"
	Apartment         EnvironmentType = "apartment"
)

type ZoneType string

const (
	ZoneSite     ZoneType = "site"
	ZoneBuilding ZoneType = "building"
	ZoneFloor    ZoneType = "floor"
	ZoneUnit     ZoneType = "unit"
	ZoneHall     ZoneType = "hall"
	ZoneRoom     ZoneType = "room"
)

type AssetKind string

const (
	AssetMeter    AssetKind = "meter"
	AssetInverter AssetKind = "inverter"
	AssetMachine  AssetKind = "machine"
	AssetSensor   AssetKind = "sensor"
	AssetActuator AssetKind = "actuator"
)

type Direction string

const (
	Sensor   Direction = "sensor"
	Actuator Direction = "actuator"
)

// Environment is the definition of one simulated site, building or apartment.
type Environment struct {
	Id   string          `json:"id" bson:"id"`
	Name string          `json:"name" bson:"name"`
	Type EnvironmentType `json:"type" bson:"type"`

	// Owner is the creator's user id, decided by the server from the token and
	// kept on update, so a value sent in a request body is ignored.
	Owner string `json:"owner,omitempty" bson:"owner"`

	// Version is counted by the server: every successful write increments it,
	// and a write is refused unless the client's version still matches the
	// stored one - this is what keeps two editors from overwriting each other's
	// devices. Zero means the client does not take part; a document written
	// before this field existed reads as zero too.
	Version int64 `json:"version" bson:"version"`

	// ExternalGraphRef is the id of the graph this environment is mirrored as in
	// the device-repository, so other applications can consume a simulated site
	// like a real one.
	//
	// The server decides and enforces this value; a client-sent value is
	// ignored, since the whole document is sent on every update and an echoed or
	// invented ref would let one environment overwrite another's graph. See
	// reconcileGraphRef in lib/api.
	ExternalGraphRef string `json:"external_graph_ref" bson:"external_graph_ref"`

	// Every stochastic source derives from Seed, so the same environment and
	// clock produce the same values.
	Seed int64 `json:"seed" bson:"seed"`

	// ContextSources drive context keys over time: outdoor temperature follows
	// a day cycle, irradiance follows the sun. Without a source a context key
	// keeps its initial value until somebody sets it by hand, which makes the
	// context look inert. Keyed by the context key the source writes.
	ContextSources map[string]Source `json:"context_sources,omitempty" bson:"context_sources,omitempty"`

	// Context is the shared surroundings every zone below can read: outdoor
	// temperature, irradiation, calendar. Initial values only - unless the
	// timeline governs the key, see Timeline.
	Context map[string]interface{} `json:"context" bson:"context"`

	// Timeline carries the dated changes of this environment: a source parameter
	// or a context value that takes effect at an instant, so a measure with a
	// start date is one document with a step in it rather than two documents.
	// Empty is the ordinary case and the behaviour of every document stored
	// before this field existed. See docs/dated-changes.md.
	Timeline []DatedChange `json:"timeline,omitempty" bson:"timeline,omitempty"`

	Zones []Zone `json:"zones" bson:"zones"`
}

// Zone is a recursive node: site, building, floor, unit, hall and room are the
// same entity with a different type, so depth is data rather than schema.
type Zone struct {
	Id   string   `json:"id" bson:"id"`
	Name string   `json:"name" bson:"name"`
	Type ZoneType `json:"type" bson:"type"`

	// Tags carry what the fixed type list deliberately does not, so that a new
	// kind of space does not require a new enum value.
	Tags []string `json:"tags" bson:"tags"`

	// TimeConstants makes a state value follow a set point instead of jumping to
	// it, in seconds per state key - the thermal inertia of a space. A key with
	// no time constant is set at once, which is what every stored document does.
	TimeConstants map[string]int64 `json:"time_constants,omitempty" bson:"time_constants,omitempty"`

	// InitialStates seeds the runtime state at start. Live values are not here.
	InitialStates map[string]interface{} `json:"initial_states" bson:"initial_states"`

	Zones  []Zone  `json:"zones" bson:"zones"`
	Assets []Asset `json:"assets" bson:"assets"`
}

// Asset is a device of the real world, bound one to one to a platform device.
type Asset struct {
	Id   string    `json:"id" bson:"id"`
	Name string    `json:"name" bson:"name"`
	Kind AssetKind `json:"kind" bson:"kind"`

	// Preserved verbatim across a migration: they keep the existing timeseries
	// in timescale attached to this asset.
	ExternalRef    string `json:"external_ref" bson:"external_ref"`
	ExternalTypeId string `json:"external_type_id" bson:"external_type_id"`

	// ExternalManaged tells a device moses created for this asset apart from one
	// the user picked and attached to it. Only a managed device is deleted again
	// when the asset or environment disappears; a picked device is platform
	// inventory and outlives the simulation.
	//
	// The server decides this, never the client, since the whole document is
	// sent on every update and a stale or invented flag would otherwise decide
	// whether a real device is deleted. See reconcileManagedFlags in lib/api.
	ExternalManaged bool `json:"external_managed" bson:"external_managed"`

	// SubmeteredBy names, by asset id, the asset whose device meters this one
	// too: what that asset reads already contains what this one draws or
	// produces. Empty is the ordinary case and means this asset attaches to its
	// zone in the mirrored graph, the behavior of every document stored before
	// this field existed.
	//
	// Unlike ExternalManaged and ExternalRef, this is authoring, not
	// reconciliation: nothing on the platform is read back to correct a wrong
	// value, so a bad value only misrepresents this simulation's own meter tree.
	SubmeteredBy string `json:"submetered_by,omitempty" bson:"submetered_by,omitempty"`

	InitialStates map[string]interface{} `json:"initial_states" bson:"initial_states"`

	Channels []Channel `json:"channels" bson:"channels"`
}

// Channel is a measuring point or a manipulated variable, carrying its unit so
// a value is never a bare number.
type Channel struct {
	Id        string    `json:"id" bson:"id"`
	Name      string    `json:"name" bson:"name"`
	Direction Direction `json:"direction" bson:"direction"`

	// ExternalRef is the platform service id this channel publishes to.
	ExternalRef string `json:"external_ref" bson:"external_ref"`

	// From the device type's content variable. Unit is denormalised so an
	// exported document stays readable without resolving the characteristic.
	CharacteristicId string `json:"characteristic_id" bson:"characteristic_id"`
	Unit             string `json:"unit" bson:"unit"`

	// IntervalSeconds is how often a sensor channel emits. Zero means the
	// channel is only driven from outside, which is the normal case for actuators.
	//
	// With PublishOnChange set it is the heartbeat instead: the longest silence
	// the channel allows, counted from the last publish whatever its reason was.
	IntervalSeconds int64 `json:"interval_seconds" bson:"interval_seconds"`

	// PublishOnChange makes the channel send when its value moves, independently
	// of the clock - the way real metering hardware behaves, and what a
	// ticker-only simulation cannot reproduce. Nil is the ordinary case and means
	// the channel publishes on IntervalSeconds alone, which is what every
	// document stored before this field existed does.
	PublishOnChange *ChangeTrigger `json:"publish_on_change,omitempty" bson:"publish_on_change,omitempty"`

	// Faults are the defects injected into this channel's measurement: an
	// outage, a frozen reading, a spike, a meter exchange. Empty is the ordinary
	// case and the behaviour of every document stored before this field existed;
	// the simulation itself never sees them, see docs/injected-faults.md.
	Faults []Fault `json:"faults,omitempty" bson:"faults,omitempty"`

	Source Source `json:"source" bson:"source"`
}

// ChangeTrigger is what counts as a change between two heartbeats. The two
// thresholds are ORed: whichever one is exceeded first sends the value, and a
// threshold left at zero is an unused one rather than a threshold of zero.
//
// Both are compared against the value last *published*, not against the value
// last computed, so a slow drift accumulates until it crosses the threshold
// instead of being lost one sub-threshold step at a time.
type ChangeTrigger struct {
	// Absolute is a deviation in the channel's own unit.
	Absolute float64 `json:"absolute,omitempty" bson:"absolute,omitempty"`

	// Relative is a fraction of the last published value: 0.05 is five percent.
	// It is compared by multiplying the threshold rather than dividing the
	// deviation through it, so a last published value of 0 is a base every
	// deviation exceeds instead of a division by zero.
	Relative float64 `json:"relative,omitempty" bson:"relative,omitempty"`

	// EvaluateIntervalSeconds is how often the value is computed and compared.
	// It sits in the trigger and not on the channel because a third interval
	// there would be representable without a trigger and mean nothing. A
	// channel whose source carries its own interval must leave this at zero:
	// one channel has exactly one evaluation cadence.
	EvaluateIntervalSeconds int64 `json:"evaluate_interval_seconds,omitempty" bson:"evaluate_interval_seconds,omitempty,truncate"`
}

type SourceKind string

const (
	// SourceScript is user supplied javascript. Implemented.
	SourceScript SourceKind = "script"
	// SourceProfile is a declarative load profile over day, week and season.
	SourceProfile SourceKind = "profile"
	// SourceDataset replays a real timeseries.
	SourceDataset SourceKind = "dataset"
	// SourceFormula derives a value from other channels and the context.
	SourceFormula SourceKind = "formula"
	// SourceAggregate is the sum, over every asset whose SubmeteredBy points at
	// the asset of this channel, of that asset's channel carrying the same
	// CharacteristicId. It is configurationless: the whole configuration is the
	// meter tree, so a channel added below is summed without editing anything
	// here - which is the difference to a formula naming its inputs one by one.
	SourceAggregate SourceKind = "aggregate"
	// SourceSchedule cycles through named states, each with a duration and a
	// value: the machine programme of a plant, declared as data instead of
	// written as a script.
	SourceSchedule SourceKind = "schedule"
)

// Source is what drives a channel; exactly one variant matches Kind, with
// SourceAggregate the one kind that has no variant at all. All variants are in
// the format from the start so it does not change when the declarative sources
// land. Validation refuses a kind whose variant is missing rather than
// accepting a document that produces nothing.
type Source struct {
	Kind SourceKind `json:"kind" bson:"kind"`

	// IntervalSeconds is how often the source computes, which is not the same as
	// how often the channel publishes. Zero means it computes when the channel
	// publishes. A legacy world evolved its state on one interval and read it out
	// on another, and folding the two together would change the simulation.
	IntervalSeconds int64 `json:"interval_seconds,omitempty" bson:"interval_seconds,omitempty,truncate"`

	Script   *ScriptSource   `json:"script,omitempty" bson:"script,omitempty"`
	Profile  *ProfileSource  `json:"profile,omitempty" bson:"profile,omitempty"`
	Dataset  *DatasetSource  `json:"dataset,omitempty" bson:"dataset,omitempty"`
	Formula  *FormulaSource  `json:"formula,omitempty" bson:"formula,omitempty"`
	Schedule *ScheduleSource `json:"schedule,omitempty" bson:"schedule,omitempty"`
}

type ScriptSource struct {
	Code string `json:"code" bson:"code"`
}

// ProfileSource is a base value with per-hour and per-weekday factors, plus a
// spread drawn from the environment seed so repeated runs match.
type ProfileSource struct {
	Base float64 `json:"base" bson:"base"`
	// HourFactors has 24 entries, WeekdayFactors 7 starting at monday.
	HourFactors    []float64 `json:"hour_factors" bson:"hour_factors"`
	WeekdayFactors []float64 `json:"weekday_factors" bson:"weekday_factors"`
	// SpreadPercent is the random variation around the resulting value.
	SpreadPercent float64 `json:"spread_percent" bson:"spread_percent"`
	// Cumulative turns the profile into a meter reading that keeps counting up.
	Cumulative bool `json:"cumulative" bson:"cumulative"`
}

type DatasetOrigin string

const (
	// OriginPlatform reads a timeseries of a real platform device.
	OriginPlatform DatasetOrigin = "platform"
	// OriginFile reads an uploaded dataset, referenced by id.
	OriginFile DatasetOrigin = "file"
	// OriginEndpoint polls an allow-listed http endpoint.
	OriginEndpoint DatasetOrigin = "endpoint"
)

type ResampleMode string

const (
	// ResampleHold keeps the last value until the next sample. Correct for states.
	ResampleHold ResampleMode = "hold"
	// ResampleLinear interpolates between samples. Correct for temperatures.
	ResampleLinear ResampleMode = "linear"
	// ResampleDistribute spreads a sample across the interval. Correct for energy.
	ResampleDistribute ResampleMode = "distribute"
)

type AnchorMode string

const (
	// AnchorLoop replays relative to simulation start and repeats forever.
	// The default, and the only usable one for a permanently running site.
	AnchorLoop AnchorMode = "loop"
	// AnchorOriginal replays at the timestamps the data actually carries.
	AnchorOriginal AnchorMode = "original"
)

// DatasetSource replays a real timeseries. The mapping fields exist because a
// german energy export imported naively produces wrong values instead of
// failing: semicolon separated, comma decimal mark, local time without offset.
type DatasetSource struct {
	Origin DatasetOrigin `json:"origin" bson:"origin"`

	// Ref is a platform device id, an uploaded dataset id or a url, per Origin.
	Ref string `json:"ref" bson:"ref"`
	// ServiceRef selects the service when Origin is OriginPlatform.
	ServiceRef string `json:"service_ref,omitempty" bson:"service_ref,omitempty"`
	// Column selects the value column: for an uploaded dataset the column name
	// (empty means the first one), for a platform timeseries the path of the
	// output variable, e.g. "value" or "energy.value".
	Column string `json:"column,omitempty" bson:"column,omitempty"`

	// Window is how much of a platform timeseries is fetched, backwards from
	// the moment the environment starts: a duration like "36h", "7d", "4w" or
	// "1y". The fetched window is then replayed like an uploaded dataset and
	// only changes on the next reload.
	Window string `json:"window,omitempty" bson:"window,omitempty"`

	Resample ResampleMode `json:"resample" bson:"resample"`
	Anchor   AnchorMode   `json:"anchor" bson:"anchor"`
	// Scale multiplies every value, for adapting a foreign profile in size.
	// Zero means unscaled, so a document that omits it plays the data as is.
	Scale float64 `json:"scale,omitempty" bson:"scale,omitempty"`
	// A meter reading keeps counting across a loop boundary instead of jumping
	// back to the first value.
	Cumulative bool `json:"cumulative" bson:"cumulative"`
}

// FormulaSource derives a value from other channels and the environment context.
type FormulaSource struct {
	Expression string `json:"expression" bson:"expression"`
	// Inputs maps a name usable in Expression to a channel id or context key.
	Inputs map[string]string `json:"inputs" bson:"inputs"`
}

// ScheduleSource is a machine programme declared as data: a cycle of named
// states, each held for a duration and publishing a value of its own, with the
// name of the current state written into the asset state so a formula, the
// live state endpoint and a dashboard can all read what the plant is doing.
//
// It deliberately cannot react: there are no transitions or conditions per
// state, since a programme that depends on a measurement is a script. The one
// thing the outside world can do is start it, through Gate.
type ScheduleSource struct {
	// States are run in the order they are written, and the last one is followed
	// by the first again unless RunOnce says otherwise.
	States []ScheduleState `json:"states" bson:"states"`

	// StateKey is the asset state key the name of the current state is written
	// to. It is mandatory and explicit rather than derived from the channel id:
	// whoever reads it - a formula input, a dashboard tile - has to spell it out,
	// and a key nobody chose is a key nobody finds.
	StateKey string `json:"state_key" bson:"state_key"`

	// Gate, when set, is the switch that starts the programme. Nil means the
	// cycle runs for as long as the environment does.
	Gate *ScheduleGate `json:"gate,omitempty" bson:"gate,omitempty"`

	// RunOnce holds the last state instead of starting the cycle over. It is the
	// shape of a job rather than of a running plant: a forklift charging once per
	// shift stops charging and stays stopped. With a Gate every opening starts a
	// new single run.
	RunOnce bool `json:"run_once,omitempty" bson:"run_once,omitempty"`
}

// ScheduleState is one step of the programme.
type ScheduleState struct {
	// Name is what is written into the asset state while this step runs. It is
	// the reason the source exists, so it is not optional.
	Name string `json:"name" bson:"name"`

	// DurationSeconds is how long the step is held. DurationSpreadPercent varies
	// it per cycle, drawn from the environment seed, so a plant does not take
	// the same exact time to set up every cycle; it stays below 100 percent,
	// since a step of no length is not a step.
	DurationSeconds       int64   `json:"duration_seconds" bson:"duration_seconds"`
	DurationSpreadPercent float64 `json:"duration_spread_percent,omitempty" bson:"duration_spread_percent,omitempty"`

	// Value is what the channel publishes while the step runs, and
	// SpreadPercent varies it the way a profile's spread does - per time slot,
	// not per cycle, so that a channel publishing on change has something to see
	// inside a step that would otherwise be a perfectly flat line.
	Value         float64 `json:"value" bson:"value"`
	SpreadPercent float64 `json:"spread_percent,omitempty" bson:"spread_percent,omitempty"`

	// StateWrites are further asset state values this step declares - the air
	// demand of a machine that is running, the setpoint it asks of the hall -
	// read by formulas and by whoever reads the live state.
	//
	// The keys of every state of the schedule are written on every evaluation,
	// not only the current state's: a key another state declares is written as 0
	// here, so an air demand set while running does not still stand once the
	// machine is idle.
	StateWrites map[string]float64 `json:"state_writes,omitempty" bson:"state_writes,omitempty"`
}

// ScheduleGate starts the programme from a context key, which is what a shift
// calendar is: the cycle restarts at the first state every time the key rises
// above Threshold, so the morning peak is produced by the programme rather than
// hand-drawn into a profile.
//
// Threshold is exclusive - open means strictly greater - so the default of 0
// fits the 0/1 calendar every context source writes without anybody having to
// think about it.
type ScheduleGate struct {
	ContextKey string  `json:"context_key" bson:"context_key"`
	Threshold  float64 `json:"threshold,omitempty" bson:"threshold,omitempty"`
}

// ScheduleClosedState is the name written while a gate is closed. It is fixed
// rather than configurable because it has to mean the same thing in every
// document a dashboard reads, and it is refused as a state name of a gated
// schedule (see checkSchedule) so that "the machine is standing still" and "the
// machine is in a state its author called off" cannot look identical.
const ScheduleClosedState = "off"

// ParseWindow reads a replay window. time.ParseDuration covers h/m/s; days,
// weeks and years are worth having because "7d" or "1y" is how people think
// about load data. There is no month suffix: time.ParseDuration already
// claims 'm' for minutes, and a month is ambiguous in length anyway.
func ParseWindow(window string) (time.Duration, error) {
	trimmed := strings.TrimSpace(window)
	if trimmed == "" {
		return 0, fmt.Errorf("the window must not be empty")
	}
	multiplier := time.Duration(0)
	switch trimmed[len(trimmed)-1] {
	case 'd':
		multiplier = 24 * time.Hour
	case 'w':
		multiplier = 7 * 24 * time.Hour
	case 'y':
		multiplier = 365 * 24 * time.Hour
	}
	if multiplier > 0 {
		count, err := strconv.ParseFloat(trimmed[:len(trimmed)-1], 64)
		//written as "not greater than zero" rather than "<= 0": strconv reads
		//"nan" as a float, and a NaN compares false against every bound below,
		//so the negated form is what keeps it out
		if err != nil || !(count > 0) {
			return 0, fmt.Errorf("unreadable window %q", window)
		}
		//float64 carries far more range than int64, and the conversion of an
		//out of range float is undefined - "293y" lands on the most negative
		//duration there is, which turns end.Add(-window) into a query that
		//starts three centuries in the future. float64(math.MaxInt64) is
		//exactly 2^63, one above the largest duration, so the comparison has
		//to be inclusive to catch the value that sits on it.
		nanoseconds := count * float64(multiplier)
		if nanoseconds >= float64(math.MaxInt64) {
			return 0, fmt.Errorf("the window %q is longer than the clock can hold, about 292 years is the most", window)
		}
		duration := time.Duration(nanoseconds)
		//a count so small that it rounds away leaves no window at all; refused
		//here for the same reason the h/m/s branch below refuses "0h"
		if duration <= 0 {
			return 0, fmt.Errorf("unreadable window %q", window)
		}
		return duration, nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("unreadable window %q, use a duration like \"36h\", \"7d\", \"4w\" or \"1y\"", window)
	}
	return duration, nil
}
