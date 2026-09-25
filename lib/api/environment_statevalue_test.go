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

package api

import (
	"net/http"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// TestPatchStateRefusesAValuePastTheStateBounds: a state change meets the bounds
// a script's state.set meets, and one that does not never reaches the runtime.
func TestPatchStateRefusesAValuePastTheStateBounds(t *testing.T) {
	store := newFakeEnvironments()
	notifier := &recordingNotifier{}
	router := testRouterWithNotifier(store, notifier)
	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("unable to create the environment: %d %s", resp.Code, resp.Body.String())
	}
	deep := interface{}(1.0)
	for i := 0; i <= jsguard.MaxStateDepth; i++ {
		deep = []interface{}{deep}
	}
	change := repo.StateChange{Zones: map[string]map[string]interface{}{"zone-1": {"deep": deep}}}
	resp := do(t, router, "PATCH", "/environments/env-1/state", "user-a", change)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(notifier.changes) != 0 {
		t.Fatalf("a refused change reached the runtime: %d", len(notifier.changes))
	}
}
