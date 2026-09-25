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
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/gin-gonic/gin"
)

// bodyLimitRouter serves every route that decodes a JSON body, the legacy ones
// with no state repo behind them: each reads its body before touching the store.
func bodyLimitRouter(t *testing.T) *gin.Engine {
	t.Helper()
	store := newFakeEnvironments()
	env := minimalEnvironment()
	env.Id, env.Owner = "env-1", "owner"
	if _, err := store.Put(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	router := testRouter(store)
	CatalogEndpoints(config.Config{}, nil, router)
	for _, legacy := range []func(config.Config, *state.StateRepo, gin.IRouter){
		WorldEndpoints, RoomEndpoints, DeviceEndpoints, ServiceEndpoints, ChangeroutineEndpoints, TemplateEndpoints,
	} {
		legacy(config.Config{}, nil, router)
	}
	return router
}

func sendRaw(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	request.Header.Set("Authorization", adminTokenFor("admin"))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// TestEveryJSONBodyIsLimited pins 413 for a body one byte past its route's limit
// on every route that decodes JSON.
func TestEveryJSONBodyIsLimited(t *testing.T) {
	router := bodyLimitRouter(t)
	routes := []struct {
		method, path string
		limit        int
	}{
		{http.MethodPut, "/environments/env-1", maxDocumentBytes},
		{http.MethodPost, "/environments", maxDocumentBytes},
		{http.MethodPatch, "/environments/env-1/state", maxDocumentBytes},
		{http.MethodPost, "/environments/env-1/history", maxRequestBytes},
		{http.MethodPost, "/environments/env-1/backfill", maxRequestBytes},
		{http.MethodPost, "/devices", maxRequestBytes},
		{http.MethodPut, "/world", maxDocumentBytes},
		{http.MethodPost, "/world", maxDocumentBytes},
		{http.MethodPut, "/room", maxDocumentBytes},
		{http.MethodPost, "/room", maxDocumentBytes},
		{http.MethodPut, "/device", maxDocumentBytes},
		{http.MethodPost, "/device", maxDocumentBytes},
		{http.MethodPost, "/device/bydevicetype", maxRequestBytes},
		{http.MethodPut, "/service", maxRequestBytes},
		{http.MethodPost, "/service", maxRequestBytes},
		{http.MethodPut, "/changeroutine", maxRequestBytes},
		{http.MethodPost, "/changeroutine", maxRequestBytes},
		{http.MethodPut, "/routinetemplate", maxRequestBytes},
		{http.MethodPost, "/routinetemplate", maxRequestBytes},
		{http.MethodPut, "/usetemplate", maxRequestBytes},
		{http.MethodPost, "/usetemplate", maxRequestBytes},
	}
	huge := map[int]string{}
	for _, r := range routes {
		if _, ok := huge[r.limit]; !ok {
			huge[r.limit] = `{"name":"` + strings.Repeat("a", r.limit) + `"}`
		}
		recorder := sendRaw(router, r.method, r.path, huge[r.limit])
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s %s: expected 413 over %d bytes, got %d: %.120s", r.method, r.path, r.limit, recorder.Code, recorder.Body.String())
		}
	}
}

// TestABodyUnderTheLimitIsRead keeps the limit from refusing ordinary documents:
// a body far larger than the Musterwerke environment still reaches validation.
func TestABodyUnderTheLimitIsRead(t *testing.T) {
	router := testRouter(newFakeEnvironments())
	env := minimalEnvironment()
	env.Name = strings.Repeat("a", 1<<20)
	recorder := do(t, router, http.MethodPut, "/environments/env-1", "owner", env)
	if recorder.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a body under the limit was refused as too large: %s", recorder.Body.String())
	}
	recorder = sendRaw(router, http.MethodPut, "/environments/env-2", "{")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unreadable body answered %d, want 400", recorder.Code)
	}
}
