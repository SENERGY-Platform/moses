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

// TestAHistoryRunOfScriptChannelsReusesVMsSafely drives a real history run whose
// channels are scripts, so the reused vms run while the sharded publish pool
// works alongside. Under -race it is the check that a vm is only ever touched
// under env.mux. It also pins that the run still produces readings.
func TestAHistoryRunOfScriptChannelsReusesVMsSafely(t *testing.T) {
	id := "env-hist-scripts"
	var channels []domain.Channel
	for _, c := range []struct{ id, code string }{
		{"ch-a", `var s = moses.asset.state; var v = (s.get('n') || 0) + 1; s.set('n', v); moses.service.send(v);`},
		{"ch-b", `var e = moses.environment.state; e.set('m', (e.get('m') || 0) + 2); moses.service.send(e.get('m'));`},
		{"ch-c", `moses.service.send(Math.round(Math.sin(moses.asset.state.get('n') || 0) * 100));`},
	} {
		ch := scriptChannel(c.id, domain.Sensor, 1, serviceRefOf(id), c.code)
		channels = append(channels, ch)
	}
	def := testEnvironment(id, channels...)
	publisher := &fakePublisher{}
	rt, env, gen := historyFixture(t, def, nil, publisher)

	const readings = 4000
	result := runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(readings*time.Second))
	if result.Published < readings {
		t.Fatalf("a script history run published %d of %d", result.Published, readings)
	}
}
