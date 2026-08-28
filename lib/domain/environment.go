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

	// Never serialised to json: taken from the caller's token, so an import
	// cannot transfer ownership.
	Owner string `json:"-" bson:"owner"`

	// Version is counted by the server: every successful write increments it,
	// and a client sends back the version of the document it read. The write is
	// then refused unless the stored document is still that one, which is what
	// keeps two editors from overwriting each other - and, with them, from
	// deleting a device the winning document still publishes through.
	//
	// Zero means the client does not take part: the write goes through
	// unchecked, and the version is still incremented. A document written before
	// this field existed reads as zero too, so zero is never a version anybody
	// has to defend.
	Version int64 `json:"version" bson:"version"`

	// ExternalGraphRef is the id of the graph this environment is mirrored as in
	// the device-repository, so other applications can consume a simulated site
	// like a real one.
	//
	// The server decides this and enforces it, a value sent by a client does not
	// count: the whole document is sent on every update, so an echoed or invented
	// ref would let one environment overwrite the graph of another - which is
	// exactly what a copy of an export would do. See reconcileGraphRef in lib/api.
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
	// temperature, irradiation, calendar. Initial values only.
	Context map[string]interface{} `json:"context" bson:"context"`

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
	// it, in seconds per state key. This is the thermal inertia of a space: a
	// hall does not have a new temperature the moment one is set. A key with no
	// time constant is set at once, which is what every stored document does.
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
	// the user picked in an editor and attached to it. Only a managed device is
	// deleted again when the asset or the whole environment disappears - a picked
	// device is inventory of the platform and outlives the simulation that used
	// it, along with its timeseries.
	//
	// The server decides this, never the client: the whole document is sent on
	// every update, so a stale or invented flag would otherwise decide whether a
	// real device is deleted. See reconcileManagedFlags in lib/api.
	ExternalManaged bool `json:"external_managed" bson:"external_managed"`

	// SubmeteredBy names, by asset id, the asset whose device meters this one
	// too: what that asset reads already contains what this one draws or
	// produces on its own. Empty is the ordinary case and means this asset
	// attaches to its zone in the mirrored graph - the behavior of every
	// document stored before this field existed.
	//
	// Unlike ExternalManaged and ExternalRef, this is authoring, not
	// reconciliation: nothing on the platform is read back to correct a wrong
	// value. It only misrepresents this simulation's own meter tree, never a
	// resource outside it.
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

	// PublishOnChange makes the channel send when its value moves rather than
	// only when the clock says so. That is what real metering hardware does: an
	// Eltako meter sends every ten minutes and additionally on a step of
	// 0.1 kWh, so a series simulated on a ticker alone is either far finer than
	// the hardware or misses every transient.
	//
	// Nil is the ordinary case and means the channel publishes on
	// IntervalSeconds alone, which is what every document stored before this
	// field existed does.
	PublishOnChange *ChangeTrigger `json:"publish_on_change,omitempty" bson:"publish_on_change,omitempty"`

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
	// It is compared by multiplying the threshold with that value rather than
	// by dividing the deviation through it, which is what makes a last
	// published value of 0 a base every deviation exceeds - the reading a meter
	// starting from zero has to produce, instead of a division by zero.
	Relative float64 `json:"relative,omitempty" bson:"relative,omitempty"`

	// EvaluateIntervalSeconds is how often the value is computed and compared.
	// It sits in the trigger and not on the channel because a third interval on
	// the channel would be representable without a trigger and would mean
	// nothing there.
	//
	// A channel whose source carries its own interval is evaluated on that one
	// and must leave this at zero: one channel has exactly one evaluation
	// cadence, and two of them would be a contradiction nobody could resolve.
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

	Script  *ScriptSource  `json:"script,omitempty" bson:"script,omitempty"`
	Profile *ProfileSource `json:"profile,omitempty" bson:"profile,omitempty"`
	Dataset *DatasetSource `json:"dataset,omitempty" bson:"dataset,omitempty"`
	Formula *FormulaSource `json:"formula,omitempty" bson:"formula,omitempty"`
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
	// the moment the environment starts: a duration like "36h", "7d" or "4w".
	// The fetched window is then replayed like an uploaded dataset and only
	// changes on the next reload.
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

// ParseWindow reads a replay window. time.ParseDuration covers h/m/s; days and
// weeks are worth having because "7d" is how people think about load data.
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
	}
	if multiplier > 0 {
		count, err := strconv.ParseFloat(trimmed[:len(trimmed)-1], 64)
		if err != nil || count <= 0 {
			return 0, fmt.Errorf("unreadable window %q", window)
		}
		return time.Duration(count * float64(multiplier)), nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("unreadable window %q, use a duration like \"36h\", \"7d\" or \"4w\"", window)
	}
	return duration, nil
}
