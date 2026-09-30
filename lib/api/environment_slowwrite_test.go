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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/platformhttp"
)

// slowWrites delays every device-manager write of the double by delay.
type slowWrites struct {
	platform *platformDouble
	delay    time.Duration
}

func (this slowWrites) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		time.Sleep(this.delay)
	}
	this.platform.ServeHTTP(writer, request)
}

// A device-manager write slower than the read timeout still completes a save:
// the device may already exist on the platform, so giving up at the read
// timeout would leave it behind while the save answers 500.
func TestASlowDeviceManagerWriteStillProvisions(t *testing.T) {
	platform := newPlatformDouble()
	server := httptest.NewServer(slowWrites{platform: platform, delay: 400 * time.Millisecond})
	t.Cleanup(server.Close)
	catalog := devices.NewCatalog(server.URL, server.URL, "moses", platformhttp.NewClients(200*time.Millisecond))
	router := testRouterWithAll(newFakeEnvironments(), newFakeShares(), catalog, newFakeGraphMirror(), &recordingNotifier{}, newFakePermissions())

	resp := do(t, router, http.MethodPost, "/environments", "user-a", checkedEnvironment())
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	platform.mux.Lock()
	defer platform.mux.Unlock()
	if len(platform.created) != 1 {
		t.Errorf("the new asset has to get its device, got %v", platform.created)
	}
}
