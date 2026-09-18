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

import "testing"

// fallbackChannel is an export dataset narrowed to one station, with a bound and
// a neighbouring station to fall back on - the shape the weather source of a
// demonstrator takes.
func fallbackChannel(mutate func(*Channel)) func(*Channel) {
	return exportDatasetChannel(func(c *Channel) {
		c.Source.Dataset.MaxGap = "2h"
		c.Source.Dataset.Filters = []DatasetFilter{{Column: "station_id", Value: "02932"}}
		c.Source.Dataset.Fallback = &DatasetFallback{
			Filters: []DatasetFilter{{Column: "station_id", Value: "01048"}},
		}
		if mutate != nil {
			mutate(c)
		}
	})
}

func TestValidateAcceptsAFallbackSeries(t *testing.T) {
	if err := Validate(profileEnvironment(fallbackChannel(nil))); err != nil {
		t.Errorf("a neighbouring station to fall back on has to be storable: %v", err)
	}
	//a fallback in a different export, which inherits neither ref nor column
	other := fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Ref = "export-2"
		c.Source.Dataset.Fallback.Column = "temperature_2m"
	})
	if err := Validate(profileEnvironment(other)); err != nil {
		t.Errorf("a fallback naming its own export and column has to be storable: %v", err)
	}
}

func TestValidateRefusesAFallbackWhereNothingCanBeSelected(t *testing.T) {
	//a file and a device service are one series already, so there is no second
	//one in them to fall back on
	expectProfileProblem(t, datasetChannel(func(c *Channel) {
		c.Source.Dataset.MaxGap = "2h"
		c.Source.Dataset.Fallback = &DatasetFallback{
			Filters: []DatasetFilter{{Column: "station_id", Value: "01048"}},
		}
	}), "only an export carries more than one series to fall back on")
	assertHasPath(t, Validate(profileEnvironment(datasetChannel(func(c *Channel) {
		c.Source.Dataset.MaxGap = "2h"
		c.Source.Dataset.Fallback = &DatasetFallback{
			Filters: []DatasetFilter{{Column: "station_id", Value: "01048"}},
		}
	}))), "zones[0].assets[0].channels[0].source.dataset.fallback")
}

func TestValidateRefusesAFallbackWithoutAMaxGap(t *testing.T) {
	//without a bound the resampling bridges every hole, so the substitute series
	//would be fetched and never read
	expectProfileProblem(t, fallbackChannel(func(c *Channel) { c.Source.Dataset.MaxGap = "" }), "needs max_gap")
	assertHasPath(t, Validate(profileEnvironment(fallbackChannel(func(c *Channel) {
		c.Source.Dataset.MaxGap = ""
	}))), "zones[0].assets[0].channels[0].source.dataset.fallback")
}

func TestValidateRefusesAFallbackThatSelectsTheSourcesOwnSeries(t *testing.T) {
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Filters = c.Source.Dataset.Filters
	}), "must select a different series")
	//the same filters in a different order still pick the same rows: they are
	//combined with and, so their order decides nothing
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Filters = []DatasetFilter{
			{Column: "station_id", Value: "02932"}, {Column: "sensor", Value: "temp"},
		}
		c.Source.Dataset.Fallback.Filters = []DatasetFilter{
			{Column: "sensor", Value: "temp"}, {Column: "station_id", Value: "02932"},
		}
	}), "must select a different series")
	//a fallback repeating the source's filters but naming another export or
	//another column is a different series and stays storable
	if err := Validate(profileEnvironment(fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Filters = c.Source.Dataset.Filters
		c.Source.Dataset.Fallback.Column = "temperature_2m"
	}))); err != nil {
		t.Errorf("a fallback on another column of the same rows has to be storable: %v", err)
	}
	assertHasPath(t, Validate(profileEnvironment(fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Filters = c.Source.Dataset.Filters
	}))), "zones[0].assets[0].channels[0].source.dataset.fallback.filters")
}

func TestValidateRefusesABrokenFallbackSelection(t *testing.T) {
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Filters = nil
	}), "must name the filters that pick the substitute series")
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Filters = []DatasetFilter{{Column: " station_id ", Value: "01048"}}
	}), "padded with whitespace")
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Filters = []DatasetFilter{{Column: "station_id", Value: " "}}
	}), "keeps nothing")
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Column = " "
	}), "must name the export's column")
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Ref = " "
	}), "must name the export")
	//the paths are per entry, the way the source's own filters report theirs
	assertHasPath(t, Validate(profileEnvironment(fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Fallback.Filters = []DatasetFilter{{Column: "station_id", Value: ""}}
	}))), "zones[0].assets[0].channels[0].source.dataset.fallback.filters[0].value")
}

func TestValidateRefusesAFallbackOnACumulativeDataset(t *testing.T) {
	//a meter's value is a count, and the neighbour counts its own: reading the
	//substitute inside a gap would step the channel to a foreign absolute value
	//and back out of it again
	expectProfileProblem(t, fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Cumulative = true
	}), "must not be combined with cumulative")
	assertHasPath(t, Validate(profileEnvironment(fallbackChannel(func(c *Channel) {
		c.Source.Dataset.Cumulative = true
	}))), "zones[0].assets[0].channels[0].source.dataset.fallback")
	//a cumulative dataset without a fallback stays storable, as it always was
	if err := Validate(profileEnvironment(exportDatasetChannel(func(c *Channel) {
		c.Source.Dataset.Cumulative = true
	}))); err != nil {
		t.Errorf("a cumulative dataset without a fallback has to stay storable: %v", err)
	}
}

// A context source's dataset runs through the same checks, so a fallback
// declared there is bound by them too.
func TestValidateRefusesAFallbackOfAContextSourceWithoutAMaxGap(t *testing.T) {
	source := Source{Kind: SourceDataset, IntervalSeconds: 300, Dataset: &DatasetSource{
		Origin: OriginExport, Ref: "export-1", Column: "temperature", Window: "7d",
		Resample: ResampleHold, Anchor: AnchorLoop,
		Filters:  []DatasetFilter{{Column: "station_id", Value: "02932"}},
		Fallback: &DatasetFallback{Filters: []DatasetFilter{{Column: "station_id", Value: "01048"}}},
	}}
	assertHasPath(t, Validate(contextSourceEnvironment("outdoor_temperature", source)),
		"context_sources.outdoor_temperature.dataset.fallback")
}

func TestFallbackSourceInheritsEverythingButTheSelection(t *testing.T) {
	source := DatasetSource{
		Origin: OriginExport, Ref: "export-1", Column: "temperature", Window: "7d",
		Resample: ResampleLinear, Anchor: AnchorOriginal, MaxGap: "2h",
		Follow: true, FollowEvery: "15m", Scale: 2,
		Filters:  []DatasetFilter{{Column: "station_id", Value: "02932"}},
		Fallback: &DatasetFallback{Filters: []DatasetFilter{{Column: "station_id", Value: "01048"}}},
	}
	substitute := source.FallbackSource()
	if substitute == nil {
		t.Fatal("a declared fallback has a source of its own")
	}
	if substitute.Ref != "export-1" || substitute.Column != "temperature" || substitute.Window != "7d" {
		t.Errorf("ref, column and window are inherited, got %+v", substitute)
	}
	if substitute.Resample != ResampleLinear || substitute.Anchor != AnchorOriginal || substitute.MaxGap != "2h" {
		t.Errorf("the reading terms are the source's, got %+v", substitute)
	}
	if !substitute.Follow || substitute.FollowEvery != "15m" || substitute.Scale != 2 {
		t.Errorf("follow and the scale are the source's, got %+v", substitute)
	}
	if len(substitute.Filters) != 1 || substitute.Filters[0].Value != "01048" {
		t.Errorf("the selection is the fallback's, got %+v", substitute.Filters)
	}
	if substitute.Fallback != nil {
		t.Error("a fallback of a fallback would be a chain, which there is none of")
	}
	//the source itself is untouched: the substitute is a copy
	if len(source.Filters) != 1 || source.Filters[0].Value != "02932" {
		t.Errorf("building the substitute must not move the source's own filters, got %+v", source.Filters)
	}
	if (DatasetSource{Origin: OriginExport}).FallbackSource() != nil {
		t.Error("a source without a fallback has no substitute source")
	}
}
