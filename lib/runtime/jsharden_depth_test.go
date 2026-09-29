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

	"github.com/SENERGY-Platform/moses/lib/jsguard"
)

// A shallow value passes every reimplemented builtin unchanged, so ordinary
// behaviour is unchanged (broad native parity is in jsbuiltin_parity_test.go).
func TestShallowValuePassesEveryGuardedNativePath(t *testing.T) {
	code := `
		var a = [3, 1, 2, {x: [4, 5]}];
		var out = [a.join('-'), a.slice(0,3).sort().join(','), [[1],[2]].flat().join(''),
			JSON.stringify([1,2,3]), String([9,8])];
		moses.record(out.join('|'));
	`
	program, err := compileScript("shallow", code)
	if err != nil {
		t.Fatal(err)
	}
	var got interface{}
	api := map[string]interface{}{"record": func(v interface{}) { got = v }}
	if err := runScript(program, api, 5*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	want := "3-1-2-[object Object]|1,2,3|12|[1,2,3]|9,8"
	if s, _ := got.(string); s != want {
		t.Fatalf("guarded paths changed a shallow value:\n got %q\nwant %q", s, want)
	}
}

// A deep object handed to moses.service.send or state.set would overflow the Go
// stack inside goja's exporter, which runs before any guard on the exported
// value. Taking the argument as a goja.Value and refusing it first closes that:
// send drops it (nothing is published) and set throws, caught here.
func TestDeepValueIsRefusedAtTheGoApiBoundary(t *testing.T) {
	build := "var a = {}; for (var i = 0; i < " + itoa(jsguard.MaxNativeWalkDepth+50) + "; i++) a = {n: a};\n"

	_, sent := scriptRun(t, "env-deep-send", build+"moses.service.send(a);")
	if len(sent) != 0 {
		t.Fatalf("a deep value reached the publish path: %v", sent)
	}

	env, _ := scriptRun(t, "env-deep-set", build+
		"var threw = false; try { moses.device.state.set('k', a); } catch (e) { threw = true; }\n"+
		"moses.device.state.set('threw', threw ? 1 : 0);")
	if env.state.Assets[testAssetId]["threw"] != 1.0 {
		t.Fatalf("state.set did not throw on a deep value: %#v", env.state.Assets[testAssetId])
	}
	if _, stored := env.state.Assets[testAssetId]["k"]; stored {
		t.Fatal("a deep value was stored")
	}
}
