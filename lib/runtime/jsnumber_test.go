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
	"math"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// TestEveryNumberAScriptWritesArrivesAsFloat64 runs through the real jsApi and
// the real execute: whether the engine hands an integral number over as int64
// or as float64 must not reach the state maps, the persisted state or the
// payload.
func TestEveryNumberAScriptWritesArrivesAsFloat64(t *testing.T) {
	const envId = "env-js-number"
	code := `
		moses.asset.state.set("literal", 5);
		moses.asset.state.set("computed", Math.min(3, 4));
		moses.asset.state.set("fraction", 2.5);
		moses.asset.state.set("text", "hi");
		moses.asset.state.set("flag", true);
		moses.zone.state.set("zoneKey", 6);
		moses.environment.state.set("contextKey", 8);
		moses.service.send(7);
		moses.service.send(Math.max(1, 2));
		moses.service.send("done");
	`
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), code))
	if err := domain.Validate(def); err != nil {
		t.Fatalf("the document has to be a legal one: %v", err)
	}
	gen := newGeneration(def, nil)
	if len(gen.sensors) != 1 {
		t.Fatalf("expected exactly one runner, got %d", len(gen.sensors))
	}
	binding := gen.sensors[0]
	if binding.script == nil {
		t.Fatalf("expected the script to be compiled at generation build, got %v", binding.scriptErr)
	}

	env := &environment{id: envId}
	var sent []interface{}
	rt := &Runtime{jsTimeout: 5 * time.Second}
	rt.execute(env, gen, binding, nil, func(value interface{}) { sent = append(sent, value) }, time.Now())

	assets := env.state.Assets[testAssetId]
	for key, want := range map[string]float64{"literal": 5, "computed": 3, "fraction": 2.5} {
		value, ok := assets[key]
		if !ok {
			t.Fatalf("expected the script to have written %q, the map holds %#v", key, assets)
		}
		number, isFloat := value.(float64)
		if !isFloat {
			t.Errorf("expected %q to be a float64, got %T (%v)", key, value, value)
			continue
		}
		if number != want {
			t.Errorf("expected %q to be %v, got %v", key, want, number)
		}
	}
	//a non-number keeps its type: the normalisation is about numbers alone
	if text, ok := assets["text"].(string); !ok || text != "hi" {
		t.Errorf("expected the string to survive as a string, got %T (%v)", assets["text"], assets["text"])
	}
	if flag, ok := assets["flag"].(bool); !ok || flag != true {
		t.Errorf("expected the bool to survive as a bool, got %T (%v)", assets["flag"], assets["flag"])
	}
	if value, ok := env.state.Zones[testZoneId]["zoneKey"]; !ok {
		t.Error("expected the zone scope to have been written")
	} else if number, isFloat := value.(float64); !isFloat || number != 6 {
		t.Errorf("expected the zone value to be float64(6), got %T (%v)", value, value)
	}
	if value, ok := env.state.Context["contextKey"]; !ok {
		t.Error("expected the context scope to have been written")
	} else if number, isFloat := value.(float64); !isFloat || number != 8 {
		t.Errorf("expected the context value to be float64(8), got %T (%v)", value, value)
	}

	if len(sent) != 3 {
		t.Fatalf("expected three sends, got %#v", sent)
	}
	for i, want := range []float64{7, 2} {
		number, ok := sent[i].(float64)
		if !ok {
			t.Errorf("expected send %d to be a float64, got %T (%v)", i, sent[i], sent[i])
			continue
		}
		if number != want {
			t.Errorf("expected send %d to be %v, got %v", i, want, number)
		}
	}
	if text, ok := sent[2].(string); !ok || text != "done" {
		t.Errorf("expected the sent string to survive as a string, got %T (%v)", sent[2], sent[2])
	}
}

// TestASyntaxErrorStillFailsOnEveryRun: the compilation moved to generation
// build, so a channel whose code does not compile must keep failing at run time
// instead of handing a nil program to the vm.
func TestASyntaxErrorStillFailsOnEveryRun(t *testing.T) {
	const envId = "env-js-broken"
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), `this is not javascript`))
	gen := newGeneration(def, nil)
	if len(gen.sensors) != 1 {
		t.Fatalf("expected the channel to keep its runner, got %d", len(gen.sensors))
	}
	binding := gen.sensors[0]
	if binding.script != nil || binding.scriptErr == nil {
		t.Fatalf("expected a compile error and no program, got %v / %v", binding.script, binding.scriptErr)
	}
	env := &environment{id: envId}
	sends := 0
	rt := &Runtime{jsTimeout: time.Second}
	//twice: the failure is a property of the binding now, so it must not be
	//reported once and then forgotten
	for i := 0; i < 2; i++ {
		rt.execute(env, gen, binding, nil, func(value interface{}) { sends++ }, time.Now())
	}
	if sends != 0 {
		t.Errorf("expected a script that does not compile to send nothing, it sent %d values", sends)
	}
	if env.dirty {
		t.Error("expected no state change from a script that never ran")
	}
}

// scriptRun compiles and runs code against the real jsApi of a fresh
// environment and hands back what it sent, so a test can assert on the state
// maps and on the error of the run at once.
func scriptRun(t *testing.T, envId string, code string) (*environment, []interface{}) {
	t.Helper()
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), code))
	if err := domain.Validate(def); err != nil {
		t.Fatalf("the document has to be a legal one: %v", err)
	}
	gen := newGeneration(def, nil)
	if len(gen.sensors) != 1 {
		t.Fatalf("expected exactly one runner, got %d", len(gen.sensors))
	}
	binding := gen.sensors[0]
	if binding.script == nil {
		t.Fatalf("expected the script to compile, got %v", binding.scriptErr)
	}
	env := &environment{id: envId}
	var sent []interface{}
	rt := &Runtime{jsTimeout: 5 * time.Second}
	send := func(value interface{}) { sent = append(sent, value) }
	api := rt.jsApi(env, gen, binding, nil, send, time.Now())
	if err := runScript(binding.script, api, rt.jsTimeout, nil); err != nil {
		t.Fatalf("expected the script to run without error, got %v", err)
	}
	return env, sent
}

// TestAMissingArgumentIsNotAValue is a deliberate deviation from otto, which
// threw a RangeError and aborted the whole run: goja fills a missing argument
// with nil, so without the guard send would publish a null and set would
// persist a nil that no reader of the state can use.
func TestAMissingArgumentIsNotAValue(t *testing.T) {
	env, sent := scriptRun(t, "env-js-missing", `
		moses.service.send();
		moses.asset.state.set("x");
		moses.asset.state.set("nulled", null);
		moses.asset.state.set("undef", undefined);
		moses.zone.state.set("zx");
		moses.environment.state.set("cx");
	`)
	if len(sent) != 0 {
		t.Errorf("expected nothing to be sent, got %#v", sent)
	}
	for _, key := range []string{"x", "nulled", "undef"} {
		if value, exists := env.state.Assets[testAssetId][key]; exists {
			t.Errorf("expected %q to stay absent, the map holds %#v (%T)", key, value, value)
		}
	}
	if value, exists := env.state.Zones[testZoneId]["zx"]; exists {
		t.Errorf("expected the zone key to stay absent, it holds %#v (%T)", value, value)
	}
	if value, exists := env.state.Context["cx"]; exists {
		t.Errorf("expected the context key to stay absent, it holds %#v (%T)", value, value)
	}
	//nothing was written, so there is nothing to persist either
	if env.dirty {
		t.Error("expected a run that wrote no value to leave the environment clean")
	}
}

// TestExtraArgumentsAreDropped pins the other half of goja's calling
// convention: an argument too many is ignored rather than fatal, and the value
// still arrives.
func TestExtraArgumentsAreDropped(t *testing.T) {
	env, sent := scriptRun(t, "env-js-extra", `
		moses.asset.state.set("y", 1, 9);
		moses.service.send(2, 3);
	`)
	value, exists := env.state.Assets[testAssetId]["y"]
	if !exists {
		t.Fatalf("expected the value to be written, the map holds %#v", env.state.Assets[testAssetId])
	}
	if number, ok := value.(float64); !ok || number != 1 {
		t.Errorf("expected float64(1), got %#v (%T)", value, value)
	}
	if !env.dirty {
		t.Error("expected the write to mark the environment dirty")
	}
	if len(sent) != 1 {
		t.Fatalf("expected exactly one send, got %#v", sent)
	}
	if number, ok := sent[0].(float64); !ok || number != 2 {
		t.Errorf("expected the sent value to be float64(2), got %#v (%T)", sent[0], sent[0])
	}
}

func TestJsNumberNormalisesEveryNumberAndNothingElse(t *testing.T) {
	for _, value := range []interface{}{int(5), int32(5), int64(5), float32(5), float64(5)} {
		got, ok := jsNumber(value).(float64)
		if !ok || got != 5 {
			t.Errorf("expected %T(%v) to become float64(5), got %#v", value, value, jsNumber(value))
		}
	}
	//a javascript number is a float64 to begin with, so the largest integer a
	//script can hold survives the conversion exactly
	exact := int64(1) << 53
	if got, ok := jsNumber(exact).(float64); !ok || int64(got) != exact {
		t.Errorf("expected 2^53 to survive, got %#v", jsNumber(exact))
	}
	if got := jsNumber("text"); got != "text" {
		t.Errorf("expected a string to pass through, got %#v", got)
	}
	if got := jsNumber(true); got != true {
		t.Errorf("expected a bool to pass through, got %#v", got)
	}
	if got := jsNumber(nil); got != nil {
		t.Errorf("expected nil to pass through, got %#v", got)
	}
	if got, ok := jsNumber(math.Inf(1)).(float64); !ok || !math.IsInf(got, 1) {
		t.Errorf("expected +Inf to pass through as a float64, got %#v", jsNumber(math.Inf(1)))
	}
}
