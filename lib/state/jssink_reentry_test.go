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

package state

import (
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/robertkrimen/otto"
)

// The otto sinks share the world's guard: a getter that calls send or set while
// its holder is converted is dropped instead of starting a nested conversion.
func TestAnOttoSinkCalledFromAGetterDoesNotNest(t *testing.T) {
	world := &World{Id: "w", States: map[string]interface{}{}}
	responses := 0
	repo := &StateRepo{Persistence: notFoundPersistence{}}
	api := repo.getJsCommandApi(world, &Room{}, &Device{}, nil, func(interface{}) { responses++ })
	code := `
		var calls = 0;
		var root = {v: 1};
		Object.defineProperty(root, 'g', {enumerable: true, get: function () {
			calls++;
			if (calls < 50) {
				moses.service.send(root);
				moses.world.state.set("inner", root);
			}
			return 1;
		}});
		moses.service.send(root);
		if (calls !== 1) throw new Error("the getter ran " + calls + " times");
	`
	if err := run(code, api, 5*time.Second, nil, nil, "w", "s"); err != nil {
		t.Fatal(err)
	}
	if responses != 1 {
		t.Fatalf("expected the outer send only, got %d", responses)
	}
	if _, stored := world.States["inner"]; stored {
		t.Error("the nested set was stored")
	}
	if !world.sink.Enter() {
		t.Error("the guard is still held after the run")
	}
}

// Keys count toward the string budget, so many objects sharing one huge key are
// refused instead of copying the key once per object.
func TestOttoSharedHugeKeysAreRefused(t *testing.T) {
	vm := otto.New()
	v, err := vm.Run(`var k = 'x'; while (k.length < (1 << 20)) k += k; var arr = [];
		for (var i = 0; i < 20; i++) { var o = {}; o[k] = 1; arr.push(o); } arr`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertOttoValue(v, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes); err == nil {
		t.Fatal("20 MB of keys passed the 16 MiB budget")
	}
}
