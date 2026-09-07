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

// This file runs the same channel scripts on goja and on otto and compares what
// they produce. It lives exactly as long as otto is in the build through
// lib/state/jsvm.go; when the legacy package dies, so does this test.

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/robertkrimen/otto"
)

// stubApi is the moses surface reduced to what a migrated script touches:
// get/set on three scopes, send, getRoom and getDevice. It is deliberately not
// the real jsApi - the point is to compare two engines against identical Go
// closures, without the normalisation jsNumber applies in between.
type stubApi struct {
	context map[string]interface{}
	zones   map[string]map[string]interface{}
	assets  map[string]map[string]interface{}
	sent    []interface{}
}

func newStubApi() *stubApi {
	return &stubApi{
		context: map[string]interface{}{},
		zones:   map[string]map[string]interface{}{"z-1": {}, "z-2": {}},
		assets:  map[string]map[string]interface{}{"a-1": {}, "a-2": {}},
	}
}

// stateOf mirrors jsStateApi, seeding included: a missing key reads as 0 and is
// written, which is one of the behaviours the two engines have to agree on.
func stateOf(states map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"get": func(field string) interface{} {
			value, ok := states[field]
			if !ok {
				states[field] = 0
				return 0
			}
			return value
		},
		"set": func(field string, value interface{}) {
			states[field] = value
		},
	}
}

func (this *stubApi) surface() map[string]interface{} {
	zoneApi := map[string]interface{}{
		"state": stateOf(this.zones["z-1"]),
		"getDevice": func(assetId string) map[string]interface{} {
			states, known := this.assets[assetId]
			if !known {
				return map[string]interface{}{}
			}
			return map[string]interface{}{"state": stateOf(states)}
		},
	}
	environmentApi := map[string]interface{}{
		"state": stateOf(this.context),
		"getRoom": func(zoneId string) map[string]interface{} {
			states, known := this.zones[zoneId]
			if !known {
				return map[string]interface{}{}
			}
			return map[string]interface{}{
				"state":     stateOf(states),
				"getDevice": zoneApi["getDevice"],
			}
		},
	}
	assetApi := map[string]interface{}{"state": stateOf(this.assets["a-1"])}
	channelApi := map[string]interface{}{
		"input": "in",
		"send":  func(value interface{}) { this.sent = append(this.sent, value) },
	}
	return map[string]interface{}{
		"world":       environmentApi,
		"room":        zoneApi,
		"device":      assetApi,
		"service":     channelApi,
		"environment": environmentApi,
		"zone":        zoneApi,
		"asset":       assetApi,
		"channel":     channelApi,
	}
}

// runOnOtto is the runner as it stood before goja, reduced to what a parity run
// needs: no timeout, because these scripts are a handful of statements.
func runOnOtto(code string, moses interface{}) error {
	vm := otto.New()
	if err := vm.Set("moses", moses); err != nil {
		return err
	}
	if err := vm.Set("httpGet", httpGet); err != nil {
		return err
	}
	_, err := vm.Run(code)
	return err
}

// sameJsValue compares two values as a script would see them. Numbers go
// through asFloat, because an integral javascript number reaches Go as int64 on
// one engine and as float64 on the other - which is exactly the difference
// jsNumber hides in production.
func sameJsValue(left interface{}, right interface{}) bool {
	leftNumber, leftNumeric := asFloat(left)
	rightNumber, rightNumeric := asFloat(right)
	if leftNumeric || rightNumeric {
		if !leftNumeric || !rightNumeric {
			return false
		}
		if math.IsNaN(leftNumber) && math.IsNaN(rightNumber) {
			return true
		}
		return leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}

func TestTheEnginesAgreeOnWhatAScriptProduces(t *testing.T) {
	scripts := map[string]string{
		"arithmetic with defaults on missing keys": `
			var n = (moses.device.state.get("n") || 0) + 1;
			var f = (moses.device.state.get("f") || 0) + 0.1;
			moses.device.state.set("n", n);
			moses.device.state.set("f", f);
			moses.service.send(n * 3 - f / 2);
		`,
		"for loop with concatenated keys": `
			var names = ["a", "b", "c"];
			var total = 0;
			for (var i = 0; i < names.length; i++) {
				var key = "slot-" + names[i];
				moses.device.state.set(key, i * 2);
				total += moses.device.state.get(key);
			}
			moses.device.state.set("total", total);
			moses.service.send(total);
		`,
		"math min and max": `
			var low = Math.min(3, 4, 2.5);
			var high = Math.max(3, 4, 2.5);
			moses.device.state.set("low", low);
			moses.device.state.set("high", high);
			moses.service.send(high - low);
			moses.service.send(Math.round(Math.max(1, 2) / 3 * 100) / 100);
		`,
		"comparison chain with strict inequality": `
			var mode = moses.world.state.get("mode");
			var changed = mode !== "run" && mode !== 0 ? 1 : 0;
			moses.world.state.set("mode", "run");
			moses.service.send(changed);
			moses.service.send(moses.world.state.get("mode") !== "run" ? "wrong" : "right");
		`,
		"a missing state key is seeded to zero": `
			moses.service.send(moses.device.state.get("never-written"));
			moses.service.send(moses.room.state.get("never-written-zone"));
			moses.service.send(moses.world.state.get("never-written-context"));
		`,
		"typeof a missing property": `
			moses.service.send(typeof moses.device.nothingHere);
			moses.service.send(typeof moses.nothingHere);
			moses.service.send(typeof moses.world.getRoom("z-9").state);
			moses.service.send(typeof moses.room.getDevice("a-2").state);
		`,
		"a string reaches send unchanged": `
			moses.device.state.set("label", "hall-" + 2);
			moses.service.send("value:" + moses.device.state.get("label"));
		`,
		"cross scope reads through getRoom and getDevice": `
			moses.world.getRoom("z-2").state.set("v", 4);
			moses.room.getDevice("a-2").state.set("w", moses.world.getRoom("z-2").state.get("v") + 1);
			moses.service.send(moses.room.getDevice("a-2").state.get("w"));
		`,
	}

	for name, code := range scripts {
		t.Run(name, func(t *testing.T) {
			gojaApi := newStubApi()
			program, err := compileScript(name, code)
			if err != nil {
				t.Fatalf("goja refused to compile the script: %v", err)
			}
			if err := runScript(program, gojaApi.surface(), 5*time.Second, nil); err != nil {
				t.Fatalf("goja failed to run the script: %v", err)
			}

			ottoApi := newStubApi()
			if err := runOnOtto(code, ottoApi.surface()); err != nil {
				t.Fatalf("otto failed to run the script: %v", err)
			}

			if len(gojaApi.sent) == 0 {
				t.Fatal("the script sent nothing, which would make the comparison vacuous")
			}
			if len(gojaApi.sent) != len(ottoApi.sent) {
				t.Fatalf("goja sent %#v, otto sent %#v", gojaApi.sent, ottoApi.sent)
			}
			for i := range gojaApi.sent {
				if !sameJsValue(gojaApi.sent[i], ottoApi.sent[i]) {
					t.Errorf("send %d: goja %#v (%T), otto %#v (%T)",
						i, gojaApi.sent[i], gojaApi.sent[i], ottoApi.sent[i], ottoApi.sent[i])
				}
			}
			compareStates(t, "context", gojaApi.context, ottoApi.context)
			for id := range gojaApi.zones {
				compareStates(t, "zone "+id, gojaApi.zones[id], ottoApi.zones[id])
			}
			for id := range gojaApi.assets {
				compareStates(t, "asset "+id, gojaApi.assets[id], ottoApi.assets[id])
			}
		})
	}
}

func compareStates(t *testing.T, scope string, left map[string]interface{}, right map[string]interface{}) {
	t.Helper()
	if len(left) != len(right) {
		t.Errorf("%s: goja wrote %#v, otto wrote %#v", scope, left, right)
		return
	}
	for key, value := range left {
		other, exists := right[key]
		if !exists {
			t.Errorf("%s: goja wrote %q, otto did not", scope, key)
			continue
		}
		if !sameJsValue(value, other) {
			t.Errorf("%s[%q]: goja %#v (%T), otto %#v (%T)", scope, key, value, value, other, other)
		}
	}
}
