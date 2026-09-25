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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/effects"
	"github.com/gin-gonic/gin"
)

// effectsRouter wires every registered environment endpoint group, so the test
// also fails when the effects group is not registered at all.
func effectsRouter(store *fakeEnvironments) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	for _, endpoints := range environmentEndpoints {
		endpoints(config.Config{}, store, nil, nil, nil, nil, nil, router)
	}
	return router
}

func storeWithEffects(owner string) *fakeEnvironments {
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id, env.Owner = "env-1", owner
	env.Context = map[string]interface{}{"shift": 0.0}
	env.Zones[0].Id = "zone-1"
	env.Zones[0].Assets[0].Id = "asset-1"
	env.Zones[0].Assets[0].Channels[0].Id = "ch-1"
	env.Zones[0].Assets[0].Channels[0].Source.Script.Code = "moses.service.send(moses.environment.state.get('shift'));"
	store.stored["env-1"] = env
	return store
}

func TestTheEffectGraphOfAnOwnEnvironmentIsServed(t *testing.T) {
	resp := do(t, effectsRouter(storeWithEffects("user-a")), "GET", "/environments/env-1/effects", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	graph := effects.Graph{}
	if err := json.Unmarshal(resp.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	expected := effects.Edge{From: "context:shift", To: "asset:asset-1", Kind: effects.EdgeReads, Via: effects.ViaScript, Channel: "ch-1", Key: "shift", Count: 1}
	if len(graph.Edges) != 1 || graph.Edges[0] != expected {
		t.Errorf("expected the one context read, got %+v", graph.Edges)
	}
	if len(graph.Nodes) != 2 || graph.Nodes[0].Id != "asset:asset-1" || graph.Nodes[1].Id != "context:shift" {
		t.Errorf("expected the asset and the context key as nodes, got %+v", graph.Nodes)
	}
	//the contract has the three lists present even when empty
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(resp.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["unresolved"]) != "[]" {
		t.Errorf("expected an empty unresolved list, got %s", raw["unresolved"])
	}
}

// Access is exactly that of GET /environments/{id}: a foreign environment is
// as absent as a missing one, and an admin sees every environment.
func TestTheEffectGraphIsGuardedLikeTheEnvironment(t *testing.T) {
	router := effectsRouter(storeWithEffects("user-a"))
	for _, call := range []struct {
		id, user string
		status   int
	}{
		{"env-1", "user-b", http.StatusNotFound},
		{"nope", "user-a", http.StatusNotFound},
		//no token at all is unreadable, which requireUser answers with 400
		{"env-1", "", http.StatusBadRequest},
	} {
		path := "/environments/" + call.id + "/effects"
		resp := do(t, router, "GET", path, call.user, nil)
		if resp.Code != call.status {
			t.Errorf("GET %s as %q: expected %d, got %d: %s", path, call.user, call.status, resp.Code, resp.Body.String())
		}
		environment := do(t, router, "GET", "/environments/"+call.id, call.user, nil)
		if environment.Code != resp.Code {
			t.Errorf("GET %s as %q answers %d, the environment itself %d", path, call.user, resp.Code, environment.Code)
		}
	}
	if resp := doWithAuthorization(t, router, "GET", "/environments/env-1/effects", tokenWithPayload(`{"realm_access":{"roles":["user"]}}`)); resp.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a token without a subject, got %d: %s", resp.Code, resp.Body.String())
	}
	if resp := doAsAdmin(t, router, "GET", "/environments/env-1/effects", "admin-1"); resp.Code != http.StatusOK {
		t.Errorf("expected an admin to read a foreign graph, got %d: %s", resp.Code, resp.Body.String())
	}
}

// A document stored before validation knew a rule may still reach the
// endpoint; a script that does not even parse is an answer, not a 500.
func TestABrokenScriptStillAnswersWithTheGraph(t *testing.T) {
	store := storeWithEffects("user-a")
	env := store.stored["env-1"]
	env.Zones[0].Assets[0].Channels = append(env.Zones[0].Assets[0].Channels, domain.Channel{
		Id: "ch-2", Source: domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: "var = ;"}},
	})
	store.stored["env-1"] = env
	resp := do(t, effectsRouter(store), "GET", "/environments/env-1/effects", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	graph := effects.Graph{}
	if err := json.Unmarshal(resp.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	if len(graph.Unresolved) != 1 || graph.Unresolved[0].Channel != "ch-2" || len(graph.Edges) != 1 {
		t.Errorf("expected the parse error listed and the other script derived, got %+v", graph)
	}
}
