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

// SPIKE (SNRGY-4817 variant C): the containment matrix. Each adversarial script
// runs in a re-executed copy of the test binary, so a fatal crash kills only
// that child; the parent reports how the child died and whether a second,
// well-behaved environment in the same process kept running to completion.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// qjsContainmentCases are the paths docs/script-limits.md keeps fatal on goja,
// plus a hanging httpGet; the value is the channel body.
var qjsContainmentCases = map[string]string{
	"native-tostring": `var o = {}; o.toString = String.prototype.trim; String(o); moses.service.send(1)`,
	"deep-recursion":  `function f(n){ return f(n+1)+1 } f(0)`,
	"infinite-loop":   `for(;;){}`,
	"memory-bomb":     `var a=[]; for(;;) a.push(new Array(1e5).fill(1))`,
	"regex-backtrack": `/(a+)+$/.test('a'.repeat(50)+'b'); moses.service.send(1)`,
	"proxy-chain":     `var p={}; for(var i=0;i<600000;i++) p=new Proxy(p,{}); p.x=1`,
	"proto-chain":     `var o={}; for(var i=0;i<2000000;i++) o=Object.create(o); o.x=1`,
	"yield-chain":     `function* g(n){ if(n>0) yield* g(n-1); else yield 1 } g(400000).next().value`,
	"json-reviver":    `var d='['.repeat(900000)+']'.repeat(900000); var i=false; JSON.parse('[]', function(k,v){ if(!i){i=true;this[k]=JSON.parse(d);} return v})`,
	"hanging-httpget": `httpGet('http://127.0.0.1:9')`,
	"string-bomb":     `var s='x'; for(;;) s=s+s`,
}

// TestQJSContainmentChild is the re-executed child: it runs the case named by
// MOSES_CONTAINMENT_CASE alongside a healthy environment and prints a line the
// parent reads, then the case, so a fatal crash cuts the output off after it.
func TestQJSContainmentChild(t *testing.T) {
	name := os.Getenv("MOSES_CONTAINMENT_CASE")
	if name == "" {
		t.Skip("child only")
	}
	code := qjsContainmentCases[name]
	if code == "" {
		t.Fatalf("unknown case %q", name)
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	//a second environment that must keep working while the first misbehaves
	healthy := newContainmentRuntime(t, "env-healthy", `moses.service.send(42)`)
	first := healthy.runOnce()
	fmt.Fprintf(out, "HEALTHY-BEFORE %v\n", first)
	out.Flush()

	victim := newContainmentRuntime(t, "env-victim", code)
	done := make(chan string, 1)
	go func() {
		err := victim.runOnce()
		done <- fmt.Sprintf("%v", err)
	}()
	select {
	case result := <-done:
		fmt.Fprintf(out, "VICTIM-RETURNED %s\n", strings.ReplaceAll(result, "\n", " "))
	case <-time.After(20 * time.Second):
		fmt.Fprintf(out, "VICTIM-HUNG\n")
	}
	out.Flush()

	//the same healthy environment again: its instance and the process must be intact
	second := healthy.runOnce()
	fmt.Fprintf(out, "HEALTHY-AFTER %v sent=%d\n", second, len(healthy.sent))
	out.Flush()
}

// containmentRuntime is one environment on the QuickJS path.
type containmentRuntime struct {
	rt      *Runtime
	env     *environment
	gen     *generation
	binding channelBinding
	sent    []interface{}
}

func newContainmentRuntime(t *testing.T, id string, code string) *containmentRuntime {
	def := testEnvironment(id, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(id), code))
	gen := newGeneration(def, nil)
	return &containmentRuntime{rt: &Runtime{jsTimeout: 2 * time.Second}, env: &environment{id: id}, gen: gen, binding: gen.sensors[0]}
}

func (this *containmentRuntime) runOnce() error {
	if this.binding.script == nil {
		return this.binding.scriptErr
	}
	return this.rt.executeQJS(this.env, this.gen, this.binding, nil, func(v interface{}) { this.sent = append(this.sent, v) }, time.Now())
}

// TestQJSContainment re-execs the child once per case and reports the outcome.
func TestQJSContainment(t *testing.T) {
	if os.Getenv("MOSES_SCRIPT_ENGINE") != "qjs" {
		t.Skip("set MOSES_SCRIPT_ENGINE=qjs")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(qjsContainmentCases))
	for name := range qjsContainmentCases {
		names = append(names, name)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			began := time.Now()
			cmd := exec.Command(self, "-test.run", "TestQJSContainmentChild", "-test.v")
			cmd.Env = append(os.Environ(), "MOSES_CONTAINMENT_CASE="+name, "MOSES_SCRIPT_ENGINE=qjs")
			raw, _ := cmd.CombinedOutput()
			elapsed := time.Since(began)
			text := string(raw)
			before := strings.Contains(text, "HEALTHY-BEFORE <nil>")
			after := extractLine(text, "HEALTHY-AFTER")
			victim := extractLine(text, "VICTIM")
			survived := after != ""
			t.Logf("case %-16s time %-10v process-survived=%v victim=[%s] healthy-after=[%s]",
				name, elapsed.Round(time.Millisecond), survived, victim, after)
			if !before {
				t.Errorf("the healthy environment did not run before the case: %s", tail(text))
			}
		})
	}
}

func extractLine(text string, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func tail(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, " | ")
}
