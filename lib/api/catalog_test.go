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
	"github.com/SENERGY-Platform/moses/lib/domain"
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

// ---------------------------------------------------------------------------
// The platform device is created when the environment is stored
// ---------------------------------------------------------------------------

func environmentWithNewMachine() domain.Environment {
	return domain.Environment{
		Name: "Metallbau", Type: domain.IndustrialSite,
		Zones: []domain.Zone{{
			Name: "Halle 1", Type: domain.ZoneHall,
			Assets: []domain.Asset{{
				Name: "Kompressor 1", Kind: domain.AssetMachine,
				//a device type but no device: this asset is new
				ExternalTypeId: "dt-1",
				Channels: []domain.Channel{{
					Name: "Strom", Direction: domain.Sensor, IntervalSeconds: 30,
					Source: domain.Source{Kind: domain.SourceProfile, Profile: &domain.ProfileSource{Base: 1}},
				}},
			}},
		}},
	}
}

// An editor that creates the device up front leaves one behind for every edit
// that is abandoned or refused. Storing is the first moment the asset is real.
func TestStoringAnEnvironmentCreatesTheDeviceOfANewAsset(t *testing.T) {
	catalog := &fakeCatalog{}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	resp := do(t, router, "POST", "/environments", "user-a", environmentWithNewMachine())
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(catalog.created) != 1 || catalog.created[0].Name != "Kompressor 1" {
		t.Fatalf("expected one device for the new asset, got %+v", catalog.created)
	}
	stored := domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Zones[0].Assets[0].ExternalRef != "urn:device:new" {
		t.Errorf("the device id has to be written back, got %q", stored.Zones[0].Assets[0].ExternalRef)
	}
}

// Re-storing must not create a second device, or every save would add one.
func TestStoringAnAssetThatAlreadyHasItsDeviceCreatesNothing(t *testing.T) {
	catalog := &fakeCatalog{}
	router := testRouterWithCatalog(newFakeEnvironments(), catalog)

	env := environmentWithNewMachine()
	env.Zones[0].Assets[0].ExternalRef = "urn:device:existing"
	if code := do(t, router, "POST", "/environments", "user-a", env).Code; code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", code)
	}
	if len(catalog.created) != 0 {
		t.Errorf("an asset with a device must not get another one, got %+v", catalog.created)
	}
}

// A document that validation refuses must create nothing: the asset it would
// have belonged to never exists.
func TestARefusedDocumentCreatesNoDevice(t *testing.T) {
	catalog := &fakeCatalog{}
	router := testRouterWithCatalog(newFakeEnvironments(), catalog)

	env := environmentWithNewMachine()
	env.Zones[0].Assets[0].Name = "" //refused by validation
	if code := do(t, router, "POST", "/environments", "user-a", env).Code; code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", code)
	}
	if len(catalog.created) != 0 {
		t.Errorf("a refused document must create nothing, got %+v", catalog.created)
	}
}

// An asset stored without its device would publish nowhere, silently - so the
// request fails instead.
func TestAFailingDeviceCreationFailsTheRequest(t *testing.T) {
	catalog := &fakeCatalog{err: errors.New("device-manager unreachable")}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	resp := do(t, router, "POST", "/environments", "user-a", environmentWithNewMachine())
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "Kompressor 1") {
		t.Errorf("the answer has to name the asset that could not be provisioned, got %s", resp.Body.String())
	}
	list, err := store.ListByOwner(t.Context(), "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("nothing may be stored when provisioning failed, got %+v", list)
	}
}

// Nested zones are reached too, or a machine in a sub-zone silently gets none.
func TestADeviceIsCreatedForAnAssetInANestedZone(t *testing.T) {
	catalog := &fakeCatalog{}
	router := testRouterWithCatalog(newFakeEnvironments(), catalog)

	env := environmentWithNewMachine()
	env.Zones[0].Zones = []domain.Zone{{
		Name: "Nebenraum", Type: domain.ZoneRoom,
		Assets: []domain.Asset{{Name: "Zähler", Kind: domain.AssetMeter, ExternalTypeId: "dt-2"}},
	}}
	if code := do(t, router, "POST", "/environments", "user-a", env).Code; code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", code)
	}
	if len(catalog.created) != 2 {
		t.Errorf("expected a device for both assets, got %+v", catalog.created)
	}
}
