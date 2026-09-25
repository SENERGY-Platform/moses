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

package domain

import (
	"testing"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
)

// TestValidateHoldsStateValuesToTheScriptBounds: a document's context and
// initial states meet the bounds a script's state.set meets, so the runtime
// never has to drop a stored value it cannot copy.
func TestValidateHoldsStateValuesToTheScriptBounds(t *testing.T) {
	deep := interface{}(1.0)
	for i := 0; i <= jsguard.MaxStateDepth; i++ {
		deep = []interface{}{deep}
	}
	wide := make([]interface{}, jsguard.MaxStateNodes)
	for i := range wide {
		wide[i] = 1.0
	}
	env := validEnvironment()
	env.Context["deep"] = deep
	env.Zones[0].InitialStates = map[string]interface{}{"wide": wide}
	paths := problemPaths(t, Validate(env))
	for _, want := range []string{"context.deep", "zones[0].initial_states.wide"} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a problem at %q, got %v", want, paths)
		}
	}
}
