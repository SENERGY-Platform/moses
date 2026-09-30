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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/platformhttp"
)

// A platform that accepts the connection and never answers is the same 502 as
// one that cannot be read, and it ends with the client timeout, well before
// the check's own deadline.
func TestAHangingPlatformIs502AfterTheClientTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	store := newFakeEnvironments()
	catalog := devices.NewCatalog(server.URL, server.URL, "moses", platformhttp.NewClients(200*time.Millisecond))
	router := testRouterWithAll(store, newFakeShares(), catalog, newFakeGraphMirror(), &recordingNotifier{}, newFakePermissions())

	//encoded here, so the request in the goroutine below has nothing left to fail on
	body, err := json.Marshal(checkedEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/environments/validate"},
		{http.MethodPost, "/environments"},
	} {
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- do(t, router, route.method, route.path, "user-a", json.RawMessage(body)) }()
		select {
		case resp := <-answered:
			if resp.Code != http.StatusBadGateway {
				t.Errorf("%s %s: expected 502, got %d: %s", route.method, route.path, resp.Code, resp.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s %s: still waiting for the platform after 5s", route.method, route.path)
		}
	}
}
