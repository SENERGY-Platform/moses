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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// The parity test is what the whole feature promises: a dated change is a pure
// function of the instant, so the live simulation, a backfill and a history run
// put the step at the same place. It is asserted on the bits, not within an
// epsilon - three paths that agree to six decimals are three paths that will
// disagree somewhere else.

const (
	parityStep    = int64(600)
	paritySeed    = int64(4711)
	parityChannel = "ch-prof"
	parityHold    = "ch-hold"
	parityDist    = "ch-dist"
)

// parityPoints is a series long enough to loop inside the window, so the replay
// is doing something rather than holding one sample throughout.
func parityPoints() []dataset.Point {
	return []dataset.Point{
		{Unix: 0, Value: 10},
		{Unix: 1800, Value: 20},
		{Unix: 3600, Value: 30},
		{Unix: 5400, Value: 40},
	}
}

// parityDocument is one site carrying every source kind a dated change reaches:
// a cumulative profile whose base and spread both step, and two replays whose
// scale steps, one holding and one distributing.
func parityDocument(id string) (domain.Environment, map[string][]dataset.Point) {
	profile := profileChannel(parityChannel, serviceRefOf(id)+"-prof", parityStep,
		domain.ProfileSource{Base: 120, SpreadPercent: 20, Cumulative: true})
	hold := datasetChannel(id, domain.DatasetSource{
		Origin: domain.OriginFile, Ref: "d1", Resample: domain.ResampleHold, Anchor: domain.AnchorLoop})
	hold.Id, hold.Name = parityHold, parityHold
	hold.ExternalRef = serviceRefOf(id) + "-hold"
	hold.IntervalSeconds = parityStep
	distribute := datasetChannel(id, domain.DatasetSource{
		Origin: domain.OriginFile, Ref: "d1", Resample: domain.ResampleDistribute, Anchor: domain.AnchorLoop})
	distribute.Id, distribute.Name = parityDist, parityDist
	distribute.ExternalRef = serviceRefOf(id) + "-dist"
	distribute.IntervalSeconds = parityStep

	def := testEnvironment(id, profile, hold, distribute)
	def.Seed = paritySeed
	def.Timeline = []domain.DatedChange{
		datedChange("channel."+parityChannel+".profile.base", timelineKnick, 360),
		datedChange("channel."+parityChannel+".profile.spread_percent", timelineKnick, 5),
		datedChange("channel."+parityHold+".dataset.scale", timelineKnick, 2),
		datedChange("channel."+parityDist+".dataset.scale", timelineKnick, 2),
	}
	series := map[string][]dataset.Point{parityHold: parityPoints(), parityDist: parityPoints()}
	return def, series
}

// parityReference computes the three series from the pure functions alone, with
// the parameter switch written out by hand: it uses no timeline index at all, so
// it is an independent answer rather than the implementation asserting on itself.
func parityReference(def domain.Environment, from time.Time, steps int64) map[string][]float64 {
	result := map[string][]float64{parityChannel: {}, parityHold: {}, parityDist: {}}
	counter := float64(0)
	for i := int64(0); i < steps; i++ {
		at := from.Add(time.Duration(i*parityStep) * time.Second).In(time.Local)
		after := !at.Before(timelineKnick)

		profile := domain.ProfileSource{Base: 120, SpreadPercent: 20, Cumulative: true}
		if after {
			profile.Base, profile.SpreadPercent = 360, 5
		}
		counter += profileValue(profile, def.Seed, parityChannel, parityStep, at) * float64(parityStep) / 3600
		result[parityChannel] = append(result[parityChannel], counter)

		for id, mode := range map[string]domain.ResampleMode{parityHold: domain.ResampleHold, parityDist: domain.ResampleDistribute} {
			replay := domain.DatasetSource{Origin: domain.OriginFile, Ref: "d1", Resample: mode, Anchor: domain.AnchorLoop}
			if after {
				replay.Scale = 2
			}
			value, playable := replayValue(replay, parityPoints(), from.Unix(), at, parityStep)
			if !playable {
				continue
			}
			result[id] = append(result[id], value)
		}
	}
	return result
}

// parityHistory runs the engine over the window and returns what each channel
// published.
func parityHistory(t *testing.T, def domain.Environment, series map[string][]dataset.Point, from time.Time, to time.Time) map[string][]float64 {
	t.Helper()
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, series)
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)
	if _, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil); err != nil {
		t.Fatalf("the history run failed: %v", err)
	}
	return map[string][]float64{
		parityChannel: historyValues(publisher, serviceRefOf(def.Id)+"-prof"),
		parityHold:    historyValues(publisher, serviceRefOf(def.Id)+"-hold"),
		parityDist:    historyValues(publisher, serviceRefOf(def.Id)+"-dist"),
	}
}

// parityBackfill runs the job's own channel loop over the same window. It is the
// real loop rather than a call to backfillValue, so the grid and the anchor are
// the job's and not the test's.
func parityBackfill(t *testing.T, def domain.Environment, series map[string][]dataset.Point, from time.Time, to time.Time) map[string][]float64 {
	t.Helper()
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, series)
	job := &backfillJob{done: make(chan struct{}), status: BackfillStatus{EnvironmentId: def.Id}}
	//one pool for every channel, as the job has it: with one per channel the
	//channels would never share a worker and the parity would not cover the pool
	pool := testPublishPool(t, rt)
	result := map[string][]float64{}
	for _, channel := range backfillChannels(def) {
		status := BackfillChannelStatus{ChannelId: channel.channel.Id}
		rt.runBackfillChannel(context.Background(), pool, job, gen, channel, series[channel.channel.Id], from, to, &status)
		if status.Failed > 0 {
			t.Fatalf("the backfill of %v failed %d readings: %v", channel.channel.Id, status.Failed, status.LastError)
		}
		result[channel.channel.Id] = historyValues(publisher, channel.channel.ExternalRef)
	}
	return result
}

// TestADatedChangeLandsAtTheSameInstantOnAllThreePaths is the claim the whole
// block is bought for. Backfill, history run and the pure reference are compared
// value for value, and the window straddles the knick so that a lookup that was
// off by one evaluation shows up as a whole reading.
func TestADatedChangeLandsAtTheSameInstantOnAllThreePaths(t *testing.T) {
	const id = "env-timeline-parity"
	def, series := parityDocument(id)
	from := timelineKnick.Add(-2 * time.Hour)
	to := timelineKnick.Add(2 * time.Hour)
	steps := backfillTicks(parityStep, from, to)
	if steps != 25 {
		t.Fatalf("the fixture is meant to be 25 steps of the grid, got %d", steps)
	}

	reference := parityReference(def, from, steps)
	history := parityHistory(t, def, series, from, to)
	backfill := parityBackfill(t, def, series, from, to)

	for _, channelId := range []string{parityChannel, parityHold, parityDist} {
		want := reference[channelId]
		if int64(len(want)) != steps {
			t.Fatalf("%v: the reference produced %d readings, expected %d", channelId, len(want), steps)
		}
		for name, got := range map[string][]float64{"the history run": history[channelId], "the backfill": backfill[channelId]} {
			if len(got) != len(want) {
				t.Fatalf("%v: %s published %d readings, the reference has %d", channelId, name, len(got), len(want))
			}
			for i := range want {
				//bit for bit: the three paths run the same arithmetic in the same
				//order, so anything else is a difference worth finding
				if got[i] != want[i] {
					at := from.Add(time.Duration(int64(i)*parityStep) * time.Second)
					t.Fatalf("%v: %s published %v at %v, the reference has %v", channelId, name, got[i], at, want[i])
				}
			}
		}
	}

	//and the step is really in there: without it the assertions above would hold
	//for a document with no timeline at all
	before := reference[parityHold][0]
	after := reference[parityHold][len(reference[parityHold])-1]
	if before == after {
		t.Error("the fixture produced no step, so the comparison proves nothing")
	}
}

// The same document without its timeline has to produce what it always did,
// which is the other half of the promise: the field is additive, and a document
// that carries none of it is not touched by any of this.
func TestADocumentWithoutATimelineIsUnchangedByTheFeature(t *testing.T) {
	const id = "env-timeline-parity-none"
	def, series := parityDocument(id)
	def.Timeline = nil
	from := timelineKnick.Add(-2 * time.Hour)
	to := timelineKnick.Add(2 * time.Hour)
	steps := backfillTicks(parityStep, from, to)

	gen := newGeneration(def, series)
	if gen.timeline != nil {
		t.Fatal("a document without a timeline has to carry no index")
	}

	history := parityHistory(t, def, series, from, to)
	backfill := parityBackfill(t, def, series, from, to)
	counter := float64(0)
	for i := int64(0); i < steps; i++ {
		at := from.Add(time.Duration(i*parityStep) * time.Second).In(time.Local)
		counter += profileValue(domain.ProfileSource{Base: 120, SpreadPercent: 20, Cumulative: true},
			def.Seed, parityChannel, parityStep, at) * float64(parityStep) / 3600
		if history[parityChannel][i] != counter || backfill[parityChannel][i] != counter {
			t.Fatalf("step %d: history %v, backfill %v, inline %v", i,
				history[parityChannel][i], backfill[parityChannel][i], counter)
		}
	}
}
