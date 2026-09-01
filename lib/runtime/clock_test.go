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
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// clockT is an instant well in the past, so a test that asserts on it cannot
// accidentally agree with a wall clock reading.
var clockT = time.Date(2026, 3, 2, 6, 15, 0, 0, time.UTC)

// bindingFor indexes a definition and hands back its single ticking channel
// together with an environment that has never run.
func bindingFor(t *testing.T, def domain.Environment, series map[string][]dataset.Point) (*generation, *environment, channelBinding) {
	t.Helper()
	gen := newGeneration(def, series)
	if len(gen.sensors) != 1 {
		t.Fatalf("expected exactly one ticking channel, got %d", len(gen.sensors))
	}
	env := &environment{id: def.Id, gen: gen, state: repo.RuntimeState{EnvironmentId: def.Id}}
	return gen, env, gen.sensors[0]
}

// A tick has one instant: every source of one dispatch resolves against the
// moment the caller took, not against a clock read per source. For a looping
// dataset that is the difference between an anchor and a replay that agree and
// two readings a second apart.
func TestATickResolvesEverySourceAgainstTheInstantItWasGiven(t *testing.T) {
	t.Run("a profile is computed at the tick instant", func(t *testing.T) {
		const id = "env-clock-profile"
		//a spread makes the value depend on the slot, so a different instant
		//would be a different number
		profile := flatProfile(230, 10)
		def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 30, profile))
		rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
		gen, env, binding := bindingFor(t, def, nil)

		var got interface{}
		rt.dispatch(env, gen, binding, nil, func(value interface{}) { got = value }, true, clockT)

		want := profileValue(profile, gen.def.Seed, "ch-1", binding.stepSeconds, clockT)
		if got != want {
			t.Errorf("expected the profile of the tick instant (%v), got %v", want, got)
		}
	})

	t.Run("a looping replay anchors and plays at the same instant", func(t *testing.T) {
		const id = "env-clock-replay"
		def := testEnvironment(id, datasetChannel(id, replaySource(domain.ResampleHold, domain.AnchorLoop)))
		rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
		gen, env, binding := bindingFor(t, def, map[string][]dataset.Point{"ch-1": replayPoints})

		var got interface{}
		rt.dispatch(env, gen, binding, nil, func(value interface{}) { got = value }, true, clockT)

		if anchor := env.state.Anchors["ch-1"]; anchor != clockT.Unix() {
			t.Errorf("expected the anchor to be the tick instant %d, got %d", clockT.Unix(), anchor)
		}
		//anchor and replay share the instant, so no time has elapsed and the
		//first sample plays; a second clock read would have started the loop
		//one second in
		if got != replayPoints[0].Value {
			t.Errorf("expected the first sample %v, got %v", replayPoints[0].Value, got)
		}
	})
}

// The change trigger books its publish at the instant the evaluation was driven
// by, not at a clock read of its own - which is what lets a run on a virtual
// clock keep the same bookkeeping. A value that is not finite goes out and
// still never becomes the comparison base.
func TestCovGateBooksThePublishAtTheInstantItWasGiven(t *testing.T) {
	const id = "env-clock-cov"
	channel := profileChannel("ch-1", serviceRefOf(id), 2, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 1}
	def := testEnvironment(id, channel)
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
	_, env, binding := bindingFor(t, def, nil)
	if binding.cov == nil {
		t.Fatal("expected a resolved change trigger")
	}

	env.mux.Lock()
	defer env.mux.Unlock()

	sent := []interface{}{}
	send := func(value interface{}) bool {
		sent = append(sent, value)
		return true
	}

	//nothing published yet, so the first finite value goes out without a
	//comparison
	if !rt.covGate(env, binding, &faultRun{}, 230.0, false, clockT, send) {
		t.Fatal("expected the first value to be published")
	}
	booked := env.state.LastPublished["ch-1"]
	if booked.Value != 230.0 || booked.AtUnix != clockT.Unix() {
		t.Errorf("expected 230 booked at %d, got %v at %d", clockT.Unix(), booked.Value, booked.AtUnix)
	}

	//a heartbeat carrying a value that is not finite: it is sent, and the
	//bookkeeping is left exactly as it was
	later := clockT.Add(time.Minute)
	if !rt.covGate(env, binding, &faultRun{}, math.NaN(), true, later, send) {
		t.Fatal("expected a forced publish to go out")
	}
	if again := env.state.LastPublished["ch-1"]; again != booked {
		t.Errorf("a value that is not finite must not become the comparison base, got %v at %d", again.Value, again.AtUnix)
	}
	if len(sent) != 2 {
		t.Errorf("expected two sends, got %d", len(sent))
	}
}

// publishAt is the timestamped counterpart of publishReporting: it has to reach
// the platform through PublishEventAt with the instant it was given, or the
// reading would be stamped with its arrival time.
func TestPublishAtCarriesTheInstantItWasGiven(t *testing.T) {
	const id = "env-clock-publish-at"
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 30, flatProfile(230, 0)))
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, publisher)
	_, env, binding := bindingFor(t, def, nil)

	sent, err := rt.publishAt(env, binding, 230.0, true, clockT)
	if !sent || err != nil {
		t.Fatalf("expected the reading to be published, got %v (%v)", sent, err)
	}
	events := publisher.all()
	if len(events) != 1 {
		t.Fatalf("expected one event, got %d", len(events))
	}
	if events[0].live {
		t.Error("publishAt has to go through the timestamped method, or the platform stamps the arrival time")
	}
	if !events[0].at.Equal(clockT) {
		t.Errorf("expected the event at %v, got %v", clockT, events[0].at)
	}
	if events[0].deviceRef != deviceRefOf(id) || events[0].serviceRef != serviceRefOf(id) || events[0].value != 230.0 {
		t.Errorf("unexpected event %+v", events[0])
	}

	//the reference checks of the shared body still apply: a channel with
	//nowhere to publish to sends nothing and says so
	nowhere := binding
	nowhere.channel.ExternalRef = ""
	sent, err = rt.publishAt(env, nowhere, 230.0, true, clockT)
	if sent {
		t.Error("a channel without a platform service must not report a publish")
	}
	if err != nil {
		t.Errorf("a channel with nowhere to publish to is not a platform refusal, got %v", err)
	}
	if publisher.count() != 1 {
		t.Errorf("expected no further event, got %d", publisher.count())
	}

	//and a refused publish is a false, whether or not it is reported. The message
	//comes back with it, because the history run puts it into the status a caller
	//polls.
	publisher.failWith(errors.New("refused"))
	sent, err = rt.publishAt(env, binding, 230.0, false, clockT)
	if sent {
		t.Error("a refused publish must not report success")
	}
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("expected the platform's refusal to be handed back, got %v", err)
	}
}
