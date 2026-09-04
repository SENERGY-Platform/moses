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

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// An injected fault is a pure function of the seed and the instant, so a history
// run and a backfill of one window have to produce the same series, bit for bit,
// as a reference that resolves nothing. Two shapes are in the fixture on purpose,
// because they are where the two ways of getting this wrong show up:
//
//   - an outage over a heartbeat firing, which is where the backfill has to
//     mirror the timer the live runner resets whether or not anything went out;
//   - a channel whose evaluation cadence differs from its publish interval, which
//     is where a drawn occurrence placed on the publish grid drifts away.

const (
	//600 second heartbeat, 60 second evaluation: the two are deliberately
	//different numbers
	faultCovStep      = int64(60)
	faultPublishStep  = int64(600)
	faultParitySeed   = int64(90210)
	faultCovChannel   = "fault-cov"
	faultPlainChannel = "fault-plain"
	faultShapeChannel = "fault-shape"

	//a threshold no single evaluation of the profile below can cross, so the
	//channel publishes on its heartbeat and the heartbeat is observable at all
	faultCovThreshold = 50.0

	//the drawn outage of the cov channel
	faultRatePerHour  = 1.2
	faultRateDuration = int64(300)

	//the window every path is run over
	faultParitySpan = 4 * time.Hour
)

var faultParityFrom = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// the four windows of the fixture, all on whole seconds and all inside the run
var (
	//covers exactly the heartbeat that is due 1200 seconds into the window
	faultOutageFrom = faultParityFrom.Add(1200 * time.Second)
	faultOutageTo   = faultParityFrom.Add(1260 * time.Second)

	faultExchangeAt = faultParityFrom.Add(2 * time.Hour)

	faultSpikeFrom = faultParityFrom.Add(3600 * time.Second)
	faultSpikeTo   = faultParityFrom.Add(5400 * time.Second)

	faultFrozenFrom = faultParityFrom.Add(9000 * time.Second)
	faultFrozenTo   = faultParityFrom.Add(10800 * time.Second)
)

func faultParityProfile() domain.ProfileSource {
	return domain.ProfileSource{Base: 120, SpreadPercent: 20, Cumulative: true}
}

func faultShapeProfile() domain.ProfileSource {
	return domain.ProfileSource{Base: 230, SpreadPercent: 10}
}

// faultParityDocument is one asset with three channels: a meter publishing on
// change whose evaluation cadence is not its publish interval and which carries
// both a dated and a drawn outage, a plain cumulative meter that is exchanged
// halfway through, and a plain instantaneous channel carrying a spike and a
// freeze.
func faultParityDocument(id string, withFaults bool) domain.Environment {
	cov := profileChannel(faultCovChannel, serviceRefOf(id)+"-cov", faultPublishStep, faultParityProfile())
	cov.PublishOnChange = &domain.ChangeTrigger{Absolute: faultCovThreshold, EvaluateIntervalSeconds: faultCovStep}

	plain := profileChannel(faultPlainChannel, serviceRefOf(id)+"-plain", faultPublishStep, faultParityProfile())
	shape := profileChannel(faultShapeChannel, serviceRefOf(id)+"-shape", faultPublishStep, faultShapeProfile())

	if withFaults {
		cov.Faults = []domain.Fault{
			{Kind: domain.FaultOutage, From: faultOutageFrom, To: faultOutageTo},
			{Kind: domain.FaultOutage, PerHour: faultRatePerHour, DurationSeconds: faultRateDuration},
		}
		plain.Faults = []domain.Fault{
			{Kind: domain.FaultMeterExchange, From: faultExchangeAt, ResetTo: 0},
		}
		shape.Faults = []domain.Fault{
			{Kind: domain.FaultSpike, From: faultSpikeFrom, To: faultSpikeTo, Factor: 12},
			{Kind: domain.FaultFrozen, From: faultFrozenFrom, To: faultFrozenTo},
		}
	}

	def := testEnvironment(id, cov, plain, shape)
	def.Seed = faultParitySeed
	return def
}

// faultParityReference computes the three series without resolving anything: the
// windows are compared by hand, the drawn occurrence is expanded over every step
// it could have begun in rather than searched back from the current one, and the
// bookkeeping of the change trigger is written out. Only the draw itself and the
// profile arithmetic are the shared pure functions, the way the timeline
// reference uses profileValue.
func faultParityReference(def domain.Environment, from time.Time, withFaults bool) map[string][]float64 {
	result := map[string][]float64{}
	to := from.Add(faultParitySpan)

	//the drawn outage of the cov channel, resolved by hand against the evaluation
	//step - which is the number the whole slot question is about
	probability := faultRatePerHour * float64(faultCovStep) / 3600
	drawnOutage := func(at time.Time) bool {
		if !withFaults {
			return false
		}
		slot := at.Unix() / faultCovStep
		//a wide, unconditional scan: every step that could still be running one,
		//without the early exit the implementation is allowed to take
		for source := slot - 20; source <= slot; source++ {
			if !faultBegan(def.Seed, faultCovChannel, 1, source, probability) {
				continue
			}
			if at.Unix() < source*faultCovStep+faultRateDuration {
				return true
			}
		}
		return false
	}

	// --- the channel publishing on change -----------------------------------
	published := []float64{}
	counter := float64(0)
	var lastPublished *float64
	lastPublishedAt := from.Unix()
	cov := covSettings{absolute: faultCovThreshold, evalSeconds: faultCovStep}
	for i := int64(0); i < backfillTicks(faultCovStep, from, to); i++ {
		at := from.Add(time.Duration(i*faultCovStep) * time.Second).In(time.Local)
		counter += profileValue(faultParityProfile(), def.Seed, faultCovChannel, faultCovStep, at) * float64(faultCovStep) / 3600
		value := counter

		dated := withFaults && !at.Before(faultOutageFrom) && at.Before(faultOutageTo)
		if dated || drawnOutage(at) {
			//nothing goes out, and the heartbeat that was due here is still spent:
			//the live runner resets its timer after the attempt whatever came of it
			if at.Unix()-lastPublishedAt >= faultPublishStep {
				lastPublishedAt = at.Unix()
			}
			continue
		}
		if !covSends(cov, lastPublished, value) && at.Unix()-lastPublishedAt < faultPublishStep {
			continue
		}
		published = append(published, value)
		lastPublishedAt = at.Unix()
		sent := value
		lastPublished = &sent
	}
	result[faultCovChannel] = published

	// --- the plain cumulative meter that is exchanged ------------------------
	published = []float64{}
	counter = 0
	exchanged := false
	offset := float64(0)
	for i := int64(0); i < backfillTicks(faultPublishStep, from, to); i++ {
		at := from.Add(time.Duration(i*faultPublishStep) * time.Second).In(time.Local)
		counter += profileValue(faultParityProfile(), def.Seed, faultPlainChannel, faultPublishStep, at) * float64(faultPublishStep) / 3600
		value := counter
		if withFaults && !at.Before(faultExchangeAt) {
			if !exchanged {
				//captured at the first reading at or after the exchange
				offset = 0 - value
				exchanged = true
			}
			value += offset
		}
		published = append(published, value)
	}
	result[faultPlainChannel] = published

	// --- the plain channel carrying a spike and a freeze ---------------------
	published = []float64{}
	holding := false
	held := float64(0)
	for i := int64(0); i < backfillTicks(faultPublishStep, from, to); i++ {
		at := from.Add(time.Duration(i*faultPublishStep) * time.Second).In(time.Local)
		value := profileValue(faultShapeProfile(), def.Seed, faultShapeChannel, faultPublishStep, at)
		if withFaults {
			if !at.Before(faultSpikeFrom) && at.Before(faultSpikeTo) {
				value *= 12
			}
			inFreeze := !at.Before(faultFrozenFrom) && at.Before(faultFrozenTo)
			switch {
			case inFreeze && !holding:
				holding, held = true, value
			case inFreeze:
				value = held
			default:
				holding = false
			}
		}
		published = append(published, value)
	}
	result[faultShapeChannel] = published

	return result
}

// faultParityRun runs the history engine over the window and hands back the
// publisher, so a caller can read either the values or the instants.
func faultParityRun(t *testing.T, def domain.Environment, from time.Time, to time.Time) *fakePublisher {
	t.Helper()
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)
	if _, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil); err != nil {
		t.Fatalf("the history run failed: %v", err)
	}
	return publisher
}

// faultParityJob runs the job's own channel loop over the same window. It is the
// real loop rather than a call to backfillValue, so the grid, the anchor and the
// fault bookkeeping are the job's and not the test's.
func faultParityJob(t *testing.T, def domain.Environment, from time.Time, to time.Time) *fakePublisher {
	t.Helper()
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	job := &backfillJob{done: make(chan struct{}), status: BackfillStatus{EnvironmentId: def.Id}}
	//one pool for every channel, as the job has it: with one per channel the
	//channels would never share a worker and the parity would not cover the pool
	pool := testPublishPool(t, rt)
	for _, channel := range backfillChannels(def) {
		status := BackfillChannelStatus{ChannelId: channel.channel.Id}
		rt.runBackfillChannel(context.Background(), pool, job, gen, channel, nil, from, to, &status)
		if status.Failed > 0 {
			t.Fatalf("the backfill of %v failed %d readings: %v", channel.channel.Id, status.Failed, status.LastError)
		}
	}
	return publisher
}

func faultParityValues(publisher *fakePublisher, envId string) map[string][]float64 {
	return map[string][]float64{
		faultCovChannel:   historyValues(publisher, serviceRefOf(envId)+"-cov"),
		faultPlainChannel: historyValues(publisher, serviceRefOf(envId)+"-plain"),
		faultShapeChannel: historyValues(publisher, serviceRefOf(envId)+"-shape"),
	}
}

func equalReadings(a []float64, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAnInjectedFaultLandsIdenticallyOnTheHistoryRunAndTheBackfill is the claim
// the block is bought for: the same document and window produce the same
// disturbed series on both reconstructing paths, and both match a reference that
// resolves nothing.
func TestAnInjectedFaultLandsIdenticallyOnTheHistoryRunAndTheBackfill(t *testing.T) {
	const id = "env-fault-parity"
	def := faultParityDocument(id, true)
	if err := domain.Validate(def); err != nil {
		t.Fatalf("the fixture has to be a storable document: %v", err)
	}
	from := faultParityFrom
	to := from.Add(faultParitySpan)

	reference := faultParityReference(def, from, true)
	history := faultParityValues(faultParityRun(t, def, from, to), id)
	backfill := faultParityValues(faultParityJob(t, def, from, to), id)

	for _, channelId := range []string{faultCovChannel, faultPlainChannel, faultShapeChannel} {
		want := reference[channelId]
		if len(want) == 0 {
			t.Fatalf("%v: the reference produced nothing", channelId)
		}
		for name, got := range map[string][]float64{"the history run": history[channelId], "the backfill": backfill[channelId]} {
			if len(got) != len(want) {
				t.Fatalf("%v: %s published %d readings, the reference has %d", channelId, name, len(got), len(want))
			}
			for i := range want {
				//bit for bit: the three run the same arithmetic in the same order,
				//so anything else is a difference worth finding
				if got[i] != want[i] {
					t.Fatalf("%v: %s published %v as reading %d, the reference has %v", channelId, name, got[i], i, want[i])
				}
			}
		}
	}

	//and the fixture really produces defects, or every assertion above would hold
	//for a document carrying no faults at all
	undisturbed := faultParityReference(faultParityDocument(id, false), from, false)
	for _, channelId := range []string{faultCovChannel, faultPlainChannel, faultShapeChannel} {
		if equalReadings(undisturbed[channelId], reference[channelId]) {
			t.Errorf("%v: the fixture produced no defect, so the comparison proves nothing", channelId)
		}
	}
}

// The heartbeat of the channel publishing on change has to be the reason for
// every publish of the undisturbed fixture, or the outage over a heartbeat firing
// below would never have been over one.
func TestTheFaultParityFixturePublishesOnItsHeartbeat(t *testing.T) {
	const id = "env-fault-heartbeat-shape"
	readings := faultParityReference(faultParityDocument(id, false), faultParityFrom, false)[faultCovChannel]
	//one publish per heartbeat over the window, plus the first evaluation
	want := int(faultParitySpan/time.Second/time.Duration(faultPublishStep)) + 1
	if len(readings) != want {
		t.Fatalf("the undisturbed cov channel is meant to publish %d times, once per heartbeat, got %d", want, len(readings))
	}
}

// An outage that falls on a heartbeat costs a full gap of silence on both paths:
// the timer is reset by the attempt, not by the publish. Asserted on the instants,
// because the values alone would not show which reading is missing.
//
// Without the mirror in the job this fails at 1260 seconds: the job would still
// consider the gap open and publish one evaluation after the outage, where the
// live runner and the history run wait out the rest of the heartbeat.
func TestAnOutageOnAHeartbeatCostsAFullGapOnBothPaths(t *testing.T) {
	const id = "env-fault-heartbeat"
	def := faultParityDocument(id, true)
	//only the dated outage, so the drawn one cannot move the instants
	def.Zones[0].Assets[0].Channels[0].Faults = []domain.Fault{
		{Kind: domain.FaultOutage, From: faultOutageFrom, To: faultOutageTo},
	}
	from := faultParityFrom
	to := from.Add(faultParitySpan)

	for name, publisher := range map[string]*fakePublisher{
		"the history run": faultParityRun(t, def, from, to),
		"the backfill":    faultParityJob(t, def, from, to),
	} {
		seconds := map[int64]bool{}
		for _, event := range publisher.backfilled(serviceRefOf(id) + "-cov") {
			seconds[event.at.Unix()-from.Unix()] = true
		}
		if !seconds[600] {
			t.Errorf("%s: the heartbeat before the outage still fires", name)
		}
		if seconds[1200] {
			t.Errorf("%s: the outage covers the heartbeat at 1200 seconds", name)
		}
		//the whole gap, not one evaluation later: the timer was reset by the
		//attempt the outage suppressed
		for offset := int64(1260); offset < 1800; offset += faultCovStep {
			if seconds[offset] {
				t.Errorf("%s: published at %d seconds, while the next heartbeat after the suppressed one is at 1800", name, offset)
			}
		}
		if !seconds[1800] {
			t.Errorf("%s: the next heartbeat after the outage is due at 1800 seconds", name)
		}
	}
}

// The other half of the promise: a document that carries no faults produces
// exactly what it always did, on both paths.
func TestADocumentWithoutFaultsIsUnchangedByTheFeature(t *testing.T) {
	const id = "env-fault-parity-none"
	def := faultParityDocument(id, false)
	from := faultParityFrom
	to := from.Add(faultParitySpan)

	gen := newGeneration(def, nil)
	for _, binding := range gen.sensors {
		if len(binding.faults.list) != 0 {
			t.Fatalf("%v: a document without faults has to resolve none", binding.channel.Id)
		}
	}

	reference := faultParityReference(def, from, false)
	history := faultParityValues(faultParityRun(t, def, from, to), id)
	backfill := faultParityValues(faultParityJob(t, def, from, to), id)
	for _, channelId := range []string{faultCovChannel, faultPlainChannel, faultShapeChannel} {
		for name, got := range map[string][]float64{"the history run": history[channelId], "the backfill": backfill[channelId]} {
			if !equalReadings(got, reference[channelId]) {
				t.Fatalf("%v: %s produced %d readings, the reference %d, and they differ",
					channelId, name, len(got), len(reference[channelId]))
			}
		}
	}
}

// A suppressed reading is silent, never failed: nothing was attempted, so nothing
// could be refused. The invariant published + silent + failed == steps has to
// hold on both paths, and both have to count the same numbers.
func TestASuppressedReadingIsBookedAsSilentOnBothPaths(t *testing.T) {
	const id = "env-fault-bookkeeping"
	def := faultParityDocument(id, true)
	from := faultParityFrom
	to := from.Add(faultParitySpan)
	steps := backfillTicks(faultCovStep, from, to)

	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	gen := newGeneration(def, nil)
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.resetForHistory()
	env.seed(gen, from)
	result, err := rt.runHistory(t.Context(), env, gen, from, to, keepTheWindow, nil)
	if err != nil {
		t.Fatalf("the history run failed: %v", err)
	}
	status := resultFor(t, result, faultCovChannel)
	if status.Failed != 0 {
		t.Errorf("a suppressed reading is never failed, got %d", status.Failed)
	}
	if status.Silent == 0 {
		t.Fatal("the fixture is meant to suppress readings")
	}
	if status.Published+status.Silent+status.Failed != steps {
		t.Errorf("published %d + silent %d + failed %d is not the %d steps of the grid",
			status.Published, status.Silent, status.Failed, steps)
	}

	job := &backfillJob{done: make(chan struct{}), status: BackfillStatus{EnvironmentId: def.Id}}
	pool := testPublishPool(t, rt)
	for _, channel := range backfillChannels(def) {
		if channel.channel.Id != faultCovChannel {
			continue
		}
		jobStatus := BackfillChannelStatus{ChannelId: channel.channel.Id}
		rt.runBackfillChannel(context.Background(), pool, job, gen, channel, nil, from, to, &jobStatus)
		if jobStatus.Failed != 0 {
			t.Errorf("a suppressed reading is never failed in the job either, got %d", jobStatus.Failed)
		}
		if jobStatus.Published != status.Published || jobStatus.Silent != status.Silent {
			t.Errorf("the job counted %d published and %d silent, the run %d and %d",
				jobStatus.Published, jobStatus.Silent, status.Published, status.Silent)
		}
		if jobStatus.Published+jobStatus.Silent+jobStatus.Failed != steps {
			t.Errorf("the job's three counters do not add up to the %d steps of the grid", steps)
		}
	}
}
