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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

func formulaChannel(envId string, expression string, inputs map[string]string) domain.Channel {
	return domain.Channel{
		Id: "ch-formula", Name: "derived", Direction: domain.Sensor,
		ExternalRef: serviceRefOf(envId) + "-formula", IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceFormula, Formula: &domain.FormulaSource{
			Expression: expression, Inputs: inputs,
		}},
	}
}

// The case the formula source exists for: grid consumption derived from two
// channels that write no state at all - their values only exist as the last
// thing each channel produced.
func TestAFormulaDerivesFromTwoProfileChannels(t *testing.T) {
	load := profileChannel("ch-load", serviceRefOf("env-grid")+"-load", 1, flatProfile(10, 0))
	pv := profileChannel("ch-pv", serviceRefOf("env-grid")+"-pv", 1, flatProfile(4, 0))
	grid := formulaChannel("env-grid", "load - pv", map[string]string{
		"load": "channel.ch-load", "pv": "channel.ch-pv",
	})
	env := testEnvironment("env-grid", load, pv, grid)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	derived := func() bool {
		for _, event := range publisher.all() {
			if event.serviceRef == serviceRefOf("env-grid")+"-formula" && event.value == 6.0 {
				return true
			}
		}
		return false
	}
	if !waitFor(6*time.Second, derived) {
		t.Fatalf("the formula never published load-pv=6, events: %v", publisher.all())
	}
}

func TestAFormulaReadsContextAndReactsToTheStateApi(t *testing.T) {
	channel := formulaChannel("env-ctx", "outdoor + 1", map[string]string{"outdoor": "context.outdoor"})
	env := testEnvironment("env-ctx", channel)
	publisher := &fakePublisher{}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	//nothing has set the context yet: a missing key counts as 0
	if !waitFor(4*time.Second, func() bool { return lastValue(publisher) == 1.0 }) {
		t.Fatalf("expected 1 while the context is unset, got %v", lastValue(publisher))
	}
	if err := rt.SetState("env-ctx", repo.StateChange{Context: map[string]interface{}{"outdoor": -5.0}}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(4*time.Second, func() bool { return lastValue(publisher) == -4.0 }) {
		t.Errorf("the formula never saw the context change, got %v", lastValue(publisher))
	}
}

func TestAFormulaReadsAssetState(t *testing.T) {
	//the script channel evolves the asset state; the formula flags a threshold
	script := scriptChannel("ch-src", domain.Sensor, 1, serviceRefOf("env-flag")+"-src", `
		var n = moses.device.state.get("n") + 10;
		moses.device.state.set("n", n);
		moses.service.send(n);
	`)
	flag := formulaChannel("env-flag", "n > 25", map[string]string{"n": "asset.n"})
	env := testEnvironment("env-flag", script, flag)
	publisher := &fakePublisher{}
	startRuntime(t, testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), publisher)

	flagged := func(want float64) func() bool {
		return func() bool {
			for _, event := range publisher.all() {
				if event.serviceRef == serviceRefOf("env-flag")+"-formula" && event.value == want {
					return true
				}
			}
			return false
		}
	}
	if !waitFor(6*time.Second, flagged(0)) {
		t.Fatal("the flag never published 0 below the threshold")
	}
	if !waitFor(6*time.Second, flagged(1)) {
		t.Fatal("the flag never published 1 above the threshold")
	}
}
