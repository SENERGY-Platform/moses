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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/gin-gonic/gin"
)

type fakeCatalog struct {
	types      []devices.DeviceType
	created    []devices.Device
	deleted    []string
	tokensSeen []string
	err        error
}

func (this *fakeCatalog) DeviceTypes(token string) ([]devices.DeviceType, error) {
	this.tokensSeen = append(this.tokensSeen, token)
	return this.types, this.err
}

func (this *fakeCatalog) CreateDevice(ctx context.Context, token string, deviceTypeId string, name string) (devices.Device, error) {
	if this.err != nil {
		return devices.Device{}, this.err
	}
	this.tokensSeen = append(this.tokensSeen, token)
	device := devices.Device{Id: "urn:device:new", Name: name, DeviceTypeId: deviceTypeId}
	this.created = append(this.created, device)
	return device, nil
}

func (this *fakeCatalog) DeleteDevice(ctx context.Context, token string, id string) error {
	this.deleted = append(this.deleted, id)
	return this.err
}

func catalogRouter(catalog DeviceCatalog) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	CatalogEndpoints(config.Config{}, catalog, router)
	return router
}

func TestTheCatalogServesWhatAnEditorNeedsToOfferAChoice(t *testing.T) {
	catalog := &fakeCatalog{types: []devices.DeviceType{{
		Id: "dt-1", Name: "KREISEL Ceramic Rotary Valve",
		Services: []devices.Service{{Id: "svc-1", Name: "Get Current Consumption", Direction: "sensor"}},
	}}}
	resp := do(t, catalogRouter(catalog), "GET", "/device-types", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "KREISEL") || !strings.Contains(resp.Body.String(), "Get Current Consumption") {
		t.Errorf("the names are what makes a dropdown readable, got %s", resp.Body.String())
	}
	//the caller's own token goes on: the device-repository decides what they see
	if len(catalog.tokensSeen) != 1 || !strings.HasPrefix(catalog.tokensSeen[0], "Bearer ") {
		t.Errorf("expected the caller's token to be forwarded, got %v", catalog.tokensSeen)
	}
}

func TestCreatingADeviceAnswersWithItsId(t *testing.T) {
	catalog := &fakeCatalog{}
	router := catalogRouter(catalog)
	resp := do(t, router, "POST", "/devices", "user-a", CreateDeviceRequest{DeviceTypeId: "dt-1", Name: "Kompressor 1"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	device := devices.Device{}
	if err := json.Unmarshal(resp.Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}
	if device.Id == "" {
		t.Error("the id is what becomes the asset's external_ref")
	}
	if len(catalog.created) != 1 || catalog.created[0].Name != "Kompressor 1" {
		t.Errorf("unexpected creation: %+v", catalog.created)
	}
}

func TestCreatingADeviceRefusesAnIncompleteRequest(t *testing.T) {
	catalog := &fakeCatalog{}
	router := catalogRouter(catalog)
	for _, request := range []CreateDeviceRequest{
		{DeviceTypeId: "", Name: "x"},
		{DeviceTypeId: "dt-1", Name: "  "},
	} {
		resp := do(t, router, "POST", "/devices", "user-a", request)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for %+v, got %d", request, resp.Code)
		}
	}
	if len(catalog.created) != 0 {
		t.Errorf("nothing may be created from an incomplete request, got %+v", catalog.created)
	}
}

func TestDeletingADeviceIsForwarded(t *testing.T) {
	catalog := &fakeCatalog{}
	resp := do(t, catalogRouter(catalog), "DELETE", "/devices/urn:device:old", "user-a", nil)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(catalog.deleted) != 1 || catalog.deleted[0] != "urn:device:old" {
		t.Errorf("unexpected deletion: %v", catalog.deleted)
	}
}

// A failing registry must not look like an empty catalog: an editor would then
// show no device types and the user would conclude none exist.
func TestAFailingRegistryIsAnError(t *testing.T) {
	catalog := &fakeCatalog{err: errors.New("device-repository unreachable")}
	if code := do(t, catalogRouter(catalog), "GET", "/device-types", "user-a", nil).Code; code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", code)
	}
}

func TestTheCatalogNeedsAToken(t *testing.T) {
	if code := doWithAuthorization(t, catalogRouter(&fakeCatalog{}), "GET", "/device-types", "").Code; code != http.StatusBadRequest {
		t.Errorf("expected the missing token to be refused, got %d", code)
	}
}
