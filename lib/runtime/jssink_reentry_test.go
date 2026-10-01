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
)

// A getter that calls a sink while its holder is converted gets a throw from
// send and set and a skipped console call, so conversions never nest; without
// the guard each nested call starts another conversion of the whole value.
func TestASinkCalledFromAGetterDoesNotNest(t *testing.T) {
	const envId = "env-js-reentry"
	code := `
		var calls = 0, refused = 0, logged = 0;
		var root = {v: 1};
		Object.defineProperty(root, 'g', {enumerable: true, get: function () {
			calls++;
			if (calls < 50) {
				try { moses.service.send(root); } catch (e) { refused++; }
				try { moses.asset.state.set("inner", root); } catch (e) { refused++; }
			}
			return 1;
		}});
		moses.service.send(root);
		var box = {};
		Object.defineProperty(box, 'g', {enumerable: true, get: function () {
			logged++;
			if (logged < 50) console.warn(box);
			return 1;
		}});
		console.warn(box);
		moses.asset.state.set("outer", {calls: calls, refused: refused, logged: logged});
	`
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), code))
	gen := newGeneration(def, nil)
	binding := gen.sensors[0]
	if binding.script == nil {
		t.Fatalf("expected the script to compile, got %v", binding.scriptErr)
	}
	env := &environment{id: envId}
	var sent []interface{}
	rt := &Runtime{jsTimeout: 5 * time.Second}
	api := rt.jsApi(env, gen, binding, nil, func(value interface{}) { sent = append(sent, value) }, time.Now())
	if err := runScriptInBraked(nil, nil, binding.script, api, rt.jsTimeout, nil, nil, "", "", &env.sink, nil); err != nil {
		t.Fatalf("expected the run to succeed, got %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("expected the outer send only, got %d sends", len(sent))
	}
	got, _ := env.state.Assets[testAssetId]["outer"].(map[string]interface{})
	//nested integers keep goja's int64, as the old export path did
	want := map[string]interface{}{"calls": int64(1), "refused": int64(2), "logged": int64(1)}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %#v, want %v (all: %#v)", key, got[key], value, got)
		}
	}
	if _, stored := env.state.Assets[testAssetId]["inner"]; stored {
		t.Error("the nested set was stored")
	}
	if env.sink.Enter() {
		env.sink.Leave()
	} else {
		t.Error("the guard is still held after the run")
	}
}
