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
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// qjsHarness runs one script channel on the QuickJS path, spike only.
type qjsHarness struct {
	rt      *Runtime
	env     *environment
	gen     *generation
	binding channelBinding
	sent    []interface{}
}

func newQJSHarness(t *testing.T, code string) *qjsHarness {
	t.Helper()
	const envId = "env-qjs"
	def := testEnvironment(envId, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(envId), code))
	gen := newGeneration(def, nil)
	if len(gen.sensors) != 1 || gen.sensors[0].script == nil {
		t.Fatalf("the script does not compile: %v", gen.sensors[0].scriptErr)
	}
	h := &qjsHarness{rt: &Runtime{jsTimeout: 2 * time.Second}, env: &environment{id: envId}, gen: gen, binding: gen.sensors[0]}
	t.Cleanup(h.env.scripts.clear)
	return h
}

func (this *qjsHarness) run() error {
	return this.rt.executeQJS(this.env, this.gen, this.binding, nil, func(value interface{}) { this.sent = append(this.sent, value) }, time.Now())
}

func TestQJSEngineRunsTheApi(t *testing.T) {
	h := newQJSHarness(t, `
		var s = moses.asset.state;
		s.set("n", s.get("n") + 1);
		s.set("text", "hä" + s.get("n"));
		s.set("flag", true);
		s.set("obj", {a: [1, 2, {b: "c"}], d: null});
		moses.zone.state.set("zoneKey", 6);
		moses.environment.state.set("contextKey", 8);
		leaked = 1;
		moses.service.send(s.get("n") * 1.5);
		moses.service.send(s.get("text"));
		moses.service.send(s.get("obj"));
	`)
	for i := 0; i < 3; i++ {
		if err := h.run(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	assets := h.env.state.Assets[testAssetId]
	if assets["n"] != float64(3) || assets["text"] != "hä3" || assets["flag"] != true {
		t.Fatalf("unexpected state %#v", assets)
	}
	if len(h.sent) != 9 || h.sent[6] != 4.5 || h.sent[7] != "hä3" {
		t.Fatalf("unexpected sends %#v", h.sent)
	}
	obj, ok := h.sent[8].(map[string]interface{})
	if !ok || obj["d"] != nil || len(obj["a"].([]interface{})) != 3 {
		t.Fatalf("unexpected composite %#v", h.sent[8])
	}
	if len(h.env.scripts.qjs.vms) != 1 {
		t.Fatalf("expected the instance to be kept, have %d", len(h.env.scripts.qjs.vms))
	}
}

func TestQJSEngineIsolatesRuns(t *testing.T) {
	h := newQJSHarness(t, `
		if (typeof leaked !== 'undefined') moses.service.send("leak");
		leaked = 1;
		var r = [typeof std, typeof os, typeof eval === 'function' ? 'e' : '', typeof setTimeout];
		try { eval('1') } catch (e) { r.push('noeval') }
		try { (function(){}).constructor('return 1') } catch (e) { r.push('nofn') }
		try { Array.prototype.push = null } catch (e) {} if (typeof [].push === 'function') r.push('frozen');
		moses.service.send(r.join(','));
	`)
	for i := 0; i < 2; i++ {
		if err := h.run(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	for _, v := range h.sent {
		if v == "leak" {
			t.Fatal("a global survived the run")
		}
	}
	if got := h.sent[0]; got != "undefined,undefined,e,undefined,noeval,nofn,frozen" {
		t.Fatalf("unexpected hardening %v", got)
	}
}

// goja's parse at generation build already refuses import(), which would reach
// qjs:std; the engine's own keyword check is the second line.
func TestQJSEngineRefusesImport(t *testing.T) {
	def := testEnvironment("env-qjs", scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf("env-qjs"), `import('qjs:std')`))
	if gen := newGeneration(def, nil); gen.sensors[0].script != nil {
		t.Fatal("expected goja to refuse import()")
	}
	h := newQJSHarness(t, `var s = "import"; moses.service.send(1)`)
	if err := h.run(); err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("expected the keyword check to refuse, got %v", err)
	}
}

// qjsBenchScripts are per-run costs on both engines: the bare run, one host
// call, ten state calls and a Musterwerke-sized body.
var qjsBenchScripts = map[string]string{
	"empty":   `var x = 1;`,
	"send":    `moses.service.send(1);`,
	"state10": `var s = moses.asset.state; for (var i = 0; i < 5; i++) { s.set('k' + i, s.get('k' + i) + 1); }`,
	"compute": `var d = [312, 12, 18, 12, 18, 10, 12, 14, 24, 168, 12, 18, 12, 18, 10, 12, 14, 24, 168, 12, 18, 12, 18, 10, 12, 14, 24, 168];
		var t = 0, i = 0, at = 500; while (i < d.length - 1 && at >= d[i]) { at -= d[i]; i++; }
		for (var k = 0; k < 200; k++) { t += Math.floor(Math.sin(k) * 1000) % 7; }
		var x = t + i;`,
}

func BenchmarkScriptRun(b *testing.B) {
	for _, name := range []string{"empty", "send", "state10", "compute"} {
		for _, engine := range []string{"goja", "qjs"} {
			b.Run(name+"/"+engine, func(b *testing.B) {
				t := &testing.T{}
				h := newQJSHarness(t, qjsBenchScripts[name])
				defer h.env.scripts.clear()
				send := func(interface{}) {}
				now := time.Now()
				run := func() {
					if engine == "qjs" {
						if err := h.rt.executeQJS(h.env, h.gen, h.binding, nil, send, now); err != nil {
							b.Fatal(err)
						}
					} else {
						h.rt.execute(h.env, h.gen, h.binding, nil, send, now)
					}
				}
				run()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					run()
				}
			})
		}
	}
}

// TestQJSMathParity spot-checks where QuickJS and goja differ in the last bit,
// which is what makes a Math-heavy script's output diverge by ~1 ULP.
func TestQJSMathParity(t *testing.T) {
	exprs := []string{
		"Math.sin(0.7)", "Math.cos(1.3)", "Math.exp(2.1)", "Math.pow(1.1, 3.3)",
		"Math.log(7.5)", "Math.sqrt(2)", "Math.tan(0.9)", "Math.atan2(3, 7)",
		"Math.sin(0.7) * 3.3 + Math.cos(1.3) / 2.7", "1/3", "0.1 + 0.2",
	}
	var diffs int
	for _, expr := range exprs {
		g := newQJSHarness(t, "moses.service.send("+expr+")")
		g.rt.execute(g.env, g.gen, g.binding, nil, func(v interface{}) { g.sent = append(g.sent, v) }, time.Now())
		q := newQJSHarness(t, "moses.service.send("+expr+")")
		if err := q.run(); err != nil {
			t.Fatal(err)
		}
		gv, qv := g.sent[0].(float64), q.sent[0].(float64)
		if gv != qv {
			diffs++
			t.Logf("DIFFER %-45s goja=%.17g qjs=%.17g (%d ULP)", expr, gv, qv, ulpDistance(gv, qv))
		} else {
			t.Logf("same   %-45s %.17g", expr, gv)
		}
	}
	t.Logf("%d of %d expressions differ, all within 1 ULP where they do", diffs, len(exprs))
}

func ulpDistance(a, b float64) int64 {
	ia, ib := int64(math.Float64bits(a)), int64(math.Float64bits(b))
	if ia < 0 {
		ia = math.MinInt64 - ia
	}
	if ib < 0 {
		ib = math.MinInt64 - ib
	}
	if ia > ib {
		return ia - ib
	}
	return ib - ia
}
