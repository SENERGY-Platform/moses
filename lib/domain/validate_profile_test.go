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
	"strings"
	"testing"
)

func profileEnvironment(mutate func(*Channel)) Environment {
	channel := Channel{
		Id: "ch-1", Name: "meter", Direction: Sensor, IntervalSeconds: 60,
		Source: Source{Kind: SourceProfile, Profile: &ProfileSource{Base: 100}},
	}
	if mutate != nil {
		mutate(&channel)
	}
	return Environment{
		Id: "e1", Name: "Werk", Type: IndustrialSite, Owner: "o",
		Zones: []Zone{{Id: "z1", Name: "Halle", Type: ZoneHall,
			Assets: []Asset{{Id: "a1", Name: "Zähler", Kind: AssetMeter,
				ExternalTypeId: "urn:infai:ses:device-type:x",
				Channels:       []Channel{channel}}}}},
	}
}

func TestValidateAcceptsAProfileSource(t *testing.T) {
	if err := Validate(profileEnvironment(nil)); err != nil {
		t.Errorf("a valid profile has to be storable now: %v", err)
	}
}

func expectProfileProblem(t *testing.T, mutate func(*Channel), fragment string) {
	t.Helper()
	err := Validate(profileEnvironment(mutate))
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Errorf("expected a problem mentioning %q, got %v", fragment, err)
	}
}

func TestValidateRefusesBrokenProfiles(t *testing.T) {
	expectProfileProblem(t, func(c *Channel) { c.Source.Profile = nil }, "must be set")
	expectProfileProblem(t, func(c *Channel) { c.Source.Profile.HourFactors = make([]float64, 23) }, "24 entries")
	expectProfileProblem(t, func(c *Channel) { c.Source.Profile.WeekdayFactors = make([]float64, 8) }, "7 entries")
	expectProfileProblem(t, func(c *Channel) { c.Source.Profile.SpreadPercent = -1 }, "must not be negative")
	expectProfileProblem(t, func(c *Channel) { c.Source.IntervalSeconds = 5 }, "no own interval")
	expectProfileProblem(t, func(c *Channel) { c.IntervalSeconds = 0 }, "must be a sensor with an interval")
	expectProfileProblem(t, func(c *Channel) { c.Direction = Actuator; c.IntervalSeconds = 0 }, "must be a sensor with an interval")
}

// The origins and kinds that nothing executes stay refused - accepting one
// would store a channel that silently produces nothing.
func TestValidateStillRefusesWhatNothingExecutes(t *testing.T) {
	err := Validate(profileEnvironment(func(c *Channel) {
		c.Source = Source{Kind: SourceDataset, Dataset: &DatasetSource{
			Origin: OriginPlatform, Ref: "x", Resample: ResampleHold, Anchor: AnchorLoop,
		}}
	}))
	if err == nil || !strings.Contains(err.Error(), "not executed yet") {
		t.Errorf("the platform origin has to stay refused, got %v", err)
	}
}

func datasetChannel(mutate func(*Channel)) func(*Channel) {
	return func(c *Channel) {
		c.Source = Source{Kind: SourceDataset, Dataset: &DatasetSource{
			Origin: OriginFile, Ref: "d1", Resample: ResampleHold, Anchor: AnchorLoop,
		}}
		if mutate != nil {
			mutate(c)
		}
	}
}

func TestValidateAcceptsAFileDataset(t *testing.T) {
	if err := Validate(profileEnvironment(datasetChannel(nil))); err != nil {
		t.Errorf("a file dataset has to be storable now: %v", err)
	}
}

func TestValidateRefusesBrokenDatasets(t *testing.T) {
	expectProfileProblem(t, datasetChannel(func(c *Channel) { c.Source.Dataset.Ref = " " }), "must name the uploaded dataset")
	expectProfileProblem(t, datasetChannel(func(c *Channel) { c.Source.Dataset.Resample = "" }), "resample")
	expectProfileProblem(t, datasetChannel(func(c *Channel) { c.Source.Dataset.Anchor = "immer" }), "unknown anchor")
	expectProfileProblem(t, datasetChannel(func(c *Channel) { c.Source.IntervalSeconds = 5 }), "no own interval")
	expectProfileProblem(t, datasetChannel(func(c *Channel) { c.IntervalSeconds = 0 }), "must be a sensor with an interval")
}
