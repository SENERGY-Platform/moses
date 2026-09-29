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
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/dop251/goja"
)

// A console call at a disabled level converts nothing: a getter on its argument
// is never read.
func TestAConsoleCallAtADisabledLevelReadsNothing(t *testing.T) {
	previous := util.Logger
	util.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	t.Cleanup(func() { util.Logger = previous })

	vm := goja.New()
	if err := vm.Set("console", scriptConsole(&jsguard.SinkGuard{})); err != nil {
		t.Fatal(err)
	}
	v, err := vm.RunString(`
		var reads = 0, o = {};
		Object.defineProperty(o, 'g', {enumerable: true, get: function () { reads++; return 1; }});
		console.log(o); console.debug(o); console.warn(o); console.error(o);
		reads`)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() != 0 {
		t.Fatalf("a disabled console call read the argument %d times", v.ToInteger())
	}
}

// The rendered console line is bounded across all arguments, not per argument.
func TestAConsoleCallIsBoundedInTotal(t *testing.T) {
	vm := goja.New()
	big := vm.ToValue(strings.Repeat("x", 10<<20))
	line := joinScriptArgs([]goja.Value{big, big, big, big})
	if len(line) > maxConsoleBytes+len(" [truncated]") {
		t.Fatalf("the line holds %d bytes, the bound is %d", len(line), maxConsoleBytes)
	}
	if !strings.HasSuffix(line, " [truncated]") {
		t.Fatal("a truncated line has to say so")
	}
}

// Keys count toward the string budget: many objects sharing one huge key are
// refused instead of copying the key once per object, for send, set and console.
func TestSharedHugeKeysAreRefused(t *testing.T) {
	vm := goja.New()
	v, err := vm.RunString(`var k = 'x'.repeat(1 << 20), arr = [];
		for (var i = 0; i < 20; i++) { var o = {}; o[k] = 1; arr.push(o); } arr`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertScriptValue(v, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes); err == nil {
		t.Fatal("20 MB of keys passed the 16 MiB budget")
	}
	if line := joinScriptArgs([]goja.Value{v}); !strings.HasPrefix(line, "[unloggable value") {
		t.Fatalf("console rendered the value, %d bytes", len(line))
	}
}

// The console formatter writes what fmt.Sprint wrote and stops at the limit
// without building the whole rendering first.
func TestConsoleFormatterIsBoundedAndMatchesSprint(t *testing.T) {
	sample := []interface{}{nil, true, int64(-3), 1.5e21, 0.1, "s", []interface{}{},
		map[string]interface{}{"b": []interface{}{int64(1), "x"}, "a": map[string]interface{}{}}}
	var b strings.Builder
	writeBounded(&b, sample, maxConsoleBytes)
	if want := fmt.Sprint(sample); b.String() != want {
		t.Fatalf("got %q, fmt.Sprint gives %q", b.String(), want)
	}

	key := strings.Repeat("k", 1<<20)
	wide := make([]interface{}, 300)
	for i := range wide {
		wide[i] = map[string]interface{}{key: int64(i)}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var bounded strings.Builder
	writeBounded(&bounded, wide, 1<<20)
	runtime.ReadMemStats(&after)
	if bounded.Len() > 1<<20+16 {
		t.Fatalf("the output holds %d bytes past a 1 MiB limit", bounded.Len())
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("formatting allocated %d MB for a 1 MiB limit", allocated>>20)
	}
}
