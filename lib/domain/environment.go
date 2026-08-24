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
// An environment is one document. It is what a user imports, exports and edits,
// and it contains no live measurements: the values a running simulation produces
// live separately (ref RuntimeState), so that ticking a channel does not rewrite
// the definition. That split is what keeps the document exportable and small
// while allowing frequent state updates.
package domain

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

	// Owner is never serialised to json: it is taken from the caller's token,
	// so that importing a document cannot transfer ownership.
	Owner string `json:"-" bson:"owner"`

	// Seed makes a run reproducible. Every stochastic source derives from it,
	// so the same environment and the same clock produce the same values.
	Seed int64 `json:"seed" bson:"seed"`

	// Context is the shared surroundings every zone below can read: outdoor
	// temperature, irradiation, calendar. Initial values only.
	Context map[string]interface{} `json:"context" bson:"context"`

	Zones []Zone `json:"zones" bson:"zones"`
}

// Zone is a recursive node: site, building, floor, unit, hall and room are the
// same entity with a different type. A metal workshop needs one level below the
// site, an apartment building needs four, and neither needs a schema change.
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

	// ExternalRef is the platform device id and ExternalTypeId its device type.
	// Both are preserved verbatim across a migration: they are what keeps the
	// existing timeseries in timescale attached to this asset.
	ExternalRef    string `json:"external_ref" bson:"external_ref"`
	ExternalTypeId string `json:"external_type_id" bson:"external_type_id"`

	InitialStates map[string]interface{} `json:"initial_states" bson:"initial_states"`

	Channels []Channel `json:"channels" bson:"channels"`
}

// Channel is a measuring point or a manipulated variable. Unlike the service it
// replaces it carries its unit, so a value is never just a bare number.
type Channel struct {
	Id        string    `json:"id" bson:"id"`
	Name      string    `json:"name" bson:"name"`
	Direction Direction `json:"direction" bson:"direction"`

	// ExternalRef is the platform service id this channel publishes to.
	ExternalRef string `json:"external_ref" bson:"external_ref"`

	// CharacteristicId and Unit come from the device type's content variable.
	// Unit is denormalised on purpose: it is display information and must stay
	// readable in an exported document without resolving the characteristic.
	CharacteristicId string `json:"characteristic_id" bson:"characteristic_id"`
	Unit             string `json:"unit" bson:"unit"`

	// IntervalSeconds is how often a sensor channel emits. Zero means the
	// channel is only driven from outside, which is the normal case for actuators.
	IntervalSeconds int64 `json:"interval_seconds" bson:"interval_seconds"`

	Source Source `json:"source" bson:"source"`
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
)

// Source is what drives a channel. Exactly one variant matches Kind.
//
// All four kinds are part of the document format from the start so that the
// export format does not change when the declarative sources are implemented.
// Only SourceScript is executed today; validation rejects the others with an
// explicit "not yet supported" rather than accepting a document that would
// silently produce nothing.
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

// ProfileSource is a declarative shape rather than a script: a base value with
// per-hour and per-weekday factors, plus a spread that is drawn from the
// environment seed so that repeated runs match.
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
// naive import of a german energy export silently produces wrong values rather
// than failing: semicolon separated, comma as decimal mark, local time without
// an offset.
type DatasetSource struct {
	Origin DatasetOrigin `json:"origin" bson:"origin"`

	// Ref is a platform device id, an uploaded dataset id or a url, per Origin.
	Ref string `json:"ref" bson:"ref"`
	// ServiceRef selects the service when Origin is OriginPlatform.
	ServiceRef string `json:"service_ref,omitempty" bson:"service_ref,omitempty"`

	Resample ResampleMode `json:"resample" bson:"resample"`
	Anchor   AnchorMode   `json:"anchor" bson:"anchor"`
	// Scale multiplies every value, for adapting a foreign profile in size.
	Scale float64 `json:"scale" bson:"scale"`
	// Cumulative marks the values as a meter reading, which must keep counting
	// across a loop boundary instead of jumping back to the first value.
	Cumulative bool `json:"cumulative" bson:"cumulative"`
}

// FormulaSource derives a value from other channels and the environment context.
type FormulaSource struct {
	Expression string `json:"expression" bson:"expression"`
	// Inputs maps a name usable in Expression to a channel id or context key.
	Inputs map[string]string `json:"inputs" bson:"inputs"`
}
