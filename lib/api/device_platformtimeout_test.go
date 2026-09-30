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

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/gin-gonic/gin"
)

// legacyPersistence keeps nothing; the legacy device endpoints only need a store that accepts.
type legacyPersistence struct{}

func (legacyPersistence) PersistWorld(world state.World) error              { return nil }
func (legacyPersistence) PersistTemplate(templ state.RoutineTemplate) error { return nil }
func (legacyPersistence) LoadWorlds() (map[string]*state.World, error)      { return nil, nil }
func (legacyPersistence) GetTemplate(id string) (state.RoutineTemplate, error) {
	return state.RoutineTemplate{}, nil
}
func (legacyPersistence) GetTemplates() ([]state.RoutineTemplate, error) { return nil, nil }
func (legacyPersistence) DeleteWorld(id string) error                    { return nil }
func (legacyPersistence) DeleteTemplate(id string) error                 { return nil }

type silentConnectionLog struct{}

func (silentConnectionLog) LogDeviceConnect(id string) error    { return nil }
func (silentConnectionLog) LogDeviceDisconnect(id string) error { return nil }
func (silentConnectionLog) LogHubConnect(gateway string) error  { return nil }
func (silentConnectionLog) LogHubDisconnect(id string) error    { return nil }

// legacyDeviceRouter serves the legacy device endpoints over one world of user-a
// with room r and device d, against a device-manager at managerUrl.
func legacyDeviceRouter(t *testing.T, managerUrl string) *gin.Engine {
	t.Helper()
	msg := state.WorldMsg{
		Id: "w", Name: "world", Owner: "user-a", States: map[string]interface{}{},
		Rooms: map[string]state.RoomMsg{"r": {Id: "r", Name: "room", States: map[string]interface{}{},
			Devices: map[string]state.DeviceMsg{"d": {Id: "d", Name: "device", ExternalRef: "external-d", States: map[string]interface{}{}}}}},
	}
	world, err := msg.ToModel()
	if err != nil {
		t.Fatal(err)
	}
	repo := &state.StateRepo{
		Worlds:      map[string]*state.World{"w": &world},
		Persistence: legacyPersistence{},
		StateLogger: silentConnectionLog{},
		Config:      config.Config{JsTimeout: time.Second, DeviceManagerUrl: managerUrl, PlatformHttpTimeout: 200 * time.Millisecond},
	}
	repo.Start()
	t.Cleanup(repo.Shutdown)
	router := gin.New()
	DeviceEndpoints(repo.Config, repo, router)
	return router
}

// On the legacy endpoints a device-manager read that never answers is what any
// other device-manager failure is, a 500 on a create; a write slower than the
// read timeout still completes, so the delete answers 200.
func TestTheLegacyDeviceEndpointsBoundReadsAndWaitForSlowWrites(t *testing.T) {
	release := make(chan struct{})
	deleted := make(chan string, 1)
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			time.Sleep(400 * time.Millisecond)
			//a client that gave up has closed the connection by now
			if r.Context().Err() != nil {
				deleted <- "abandoned by moses"
				return
			}
			deleted <- r.Method + " " + r.URL.Path
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		manager.Close()
	})
	router := legacyDeviceRouter(t, manager.URL)

	create, err := json.Marshal(state.CreateDeviceByTypeRequest{Name: "typed", DeviceTypeId: "dt", Room: "r"})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		method, path string
		body         interface{}
		want         int
	}{
		{http.MethodPost, "/device/bydevicetype", json.RawMessage(create), http.StatusInternalServerError},
		{http.MethodDelete, "/device/d", nil, http.StatusOK},
	} {
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- do(t, router, call.method, call.path, "user-a", call.body) }()
		select {
		case resp := <-answered:
			if resp.Code != call.want {
				t.Errorf("%s %s: expected %d, got %d: %s", call.method, call.path, call.want, resp.Code, resp.Body.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s %s: still waiting for the device-manager after 5s", call.method, call.path)
		}
	}
	select {
	case request := <-deleted:
		if request != "DELETE /devices/external-d" {
			t.Errorf("expected the platform device to be deleted and waited for, got %q", request)
		}
	default:
		t.Error("the delete never reached the device-manager")
	}
}
