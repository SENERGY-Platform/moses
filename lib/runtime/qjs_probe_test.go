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
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fastschema/qjs"
)

// TestQJSProbe prints how the qjs build behaves; spike only.
func TestQJSProbe(t *testing.T) {
	if os.Getenv("MOSES_QJS_PROBE") == "" {
		t.Skip("set MOSES_QJS_PROBE")
	}
	started := time.Now()
	rt, err := qjs.New(qjs.Option{CloseOnContextDone: true, MemoryLimit: 32 << 20, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first New (compile+instantiate) %v", time.Since(started))
	started = time.Now()
	rt2, err := qjs.New(qjs.Option{CloseOnContextDone: true, MemoryLimit: 32 << 20, CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("second New %v, mem %d bytes", time.Since(started), rt2.Mem().Size())
	ctx := rt.Context()
	t.Logf("int 7 raw %#x, int -5 raw %#x", ctx.Call("QJS_NewInt32", ctx.Raw(), 7).Raw(), ctx.Call("QJS_NewInt32", ctx.Raw(), uint64(uint32(0xfffffffb))).Raw())
	f := ctx.Call("QJS_NewFloat64", ctx.Raw(), math.Float64bits(1.5)).Raw()
	t.Logf("float 1.5 raw %#x bits %#x", f, math.Float64bits(1.5))
	t.Logf("undefined raw %#x null raw %#x", ctx.Call("JS_NewUndefined").Raw(), ctx.Call("JS_NewNull").Raw())
	run := func(name, code string, timeout time.Duration, then ...string) {
		r, err := qjs.New(qjs.Option{CloseOnContextDone: true, MemoryLimit: 32 << 20, CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		c, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		r.Context().Context = c
		began := time.Now()
		func() {
			defer func() {
				if p := recover(); p != nil {
					s := ""
					if e, ok := p.(error); ok {
						s = e.Error()
					}
					if i := strings.Index(s, "\n"); i >= 0 {
						s = s[:i]
					}
					t.Logf("%s: PANIC after %v: %s (mem %d)", name, time.Since(began), s, r.Mem().Size())
				}
			}()
			v, err := r.Eval("probe.js", qjs.Code(code))
			if err != nil {
				s := err.Error()
				if len(s) > 200 {
					s = s[:200]
				}
				t.Logf("%s: error after %v: %s (mem %d)", name, time.Since(began), s, r.Mem().Size())
				return
			}
			t.Logf("%s: value %q after %v (mem %d)", name, v.String(), time.Since(began), r.Mem().Size())
			for _, next := range then {
				w, err := r.Eval("probe2.js", qjs.Code(next))
				if err != nil {
					t.Logf("%s then: error %v", name, err)
					continue
				}
				t.Logf("%s then: %q", name, w.String())
			}
		}()
	}
	run("globals", "Reflect.ownKeys(globalThis).map(String).join(' ')", time.Second)
	for _, n := range []int{100, 200, 300, 400, 600, 800, 1000, 1500, 2000, 3000} {
		run(fmt.Sprintf("depth %d", n), fmt.Sprintf("function f(n){ return n <= 0 ? 0 : f(n-1)+1 } f(%d)", n), 3*time.Second)
	}
}

// TestQJSWasmHeapBaseRefs dumps the functions that reference the heap base.
func TestQJSWasmHeapBaseRefs(t *testing.T) {
	if os.Getenv("MOSES_QJS_PROBE") == "" {
		t.Skip("set MOSES_QJS_PROBE")
	}
	module, err := qjsOriginalWasm()
	if err != nil {
		t.Fatal(err)
	}
	sections, _ := wasmSections(module)
	for _, s := range sections {
		if s.id != 10 {
			continue
		}
		n, j, _ := wasmReadULEB(s.body, 0)
		for f := uint64(0); f < n; f++ {
			size, start, _ := wasmReadULEB(s.body, j)
			body := s.body[start : start+int(size)]
			j = start + int(size)
			if bytes.Contains(body, []byte{0x41, 0xa0, 0x8c, 0x0a}) {
				t.Logf("function %d size %d: %x", f, len(body), body[:min(len(body), 400)])
			}
		}
	}
	for _, p := range []qjsPatch{{maxPages: 1024}, {maxPages: 1024, checkStack: true}} {
		patched, err := patchQJSWasm(module, p)
		t.Logf("patch %+v: %d bytes, err %v", p, len(patched), err)
	}
}

// TestQJSProbeCallCosts times JS-side call shapes inside one eval, spike only.
func TestQJSProbeCallCosts(t *testing.T) {
	if os.Getenv("MOSES_QJS_PROBE") == "" {
		t.Skip("set MOSES_QJS_PROBE")
	}
	r, err := qjs.New(qjs.Option{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fn := r.Context().Function(func(this *qjs.This) (*qjs.Value, error) { return this.Context().NewInt32(0), nil })
	r.Context().Global().SetPropertyStr("hostfn", fn)
	code := `
	var N = 100000, out = [];
	function g(a, b, c, d, e, f, h) { return 0 }
	var wrap = function (...args) { return g.call(this, 1, 2, 3, undefined, ...args) };
	var m = new Map([['plant_cycle', 1], ['holiday', 2]]);
	var f64 = new Float64Array(1);
	function time(name, fn) { var t0 = Date.now(); fn(); out.push(name + ' ' + ((Date.now() - t0) * 1e6 / N).toFixed(0) + 'ns'); }
	time('empty loop', function () { for (var i = 0; i < N; i++) {} });
	time('direct call', function () { for (var i = 0; i < N; i++) g(1, 2, 3) });
	time('wrapper call', function () { for (var i = 0; i < N; i++) wrap(1, 2, 3) });
	time('map get', function () { for (var i = 0; i < N; i++) m.get('holiday') });
	time('f64 write+read', function () { for (var i = 0; i < N; i++) { f64[0] = i; f64[0] } });
	time('host call', function () { for (var i = 0; i < N; i++) hostfn(1, 2, 3) });
	time('object+closures x10', function () { for (var i = 0; i < N / 10; i++) { for (var k = 0; k < 10; k++) { var o = {get: function () {}, set: function () {}} } } });
	out.join('; ')`
	v, err := r.Eval("cost.js", qjs.Code(code))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(v.String())
}
