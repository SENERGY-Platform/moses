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

package devices

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	deviceRepo "github.com/SENERGY-Platform/device-repository/lib/client"
	deviceRepoModel "github.com/SENERGY-Platform/device-repository/lib/model"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// The shape of a real device type: the measured value sits on a leaf below a
// structured root, which is where its characteristic lives.
func industryService(id string, name string, interaction models.Interaction, protocolId string) models.Service {
	return models.Service{
		Id: id, Name: name, Interaction: interaction, ProtocolId: protocolId,
		Outputs: []models.Content{{ContentVariable: models.ContentVariable{
			Name: "state",
			SubContentVariables: []models.ContentVariable{{
				Name: "value", CharacteristicId: "urn:infai:ses:characteristic:b59c3965",
			}},
		}}},
	}
}

func TestADeviceTypeYieldsWhatAChannelNeeds(t *testing.T) {
	converted := convertDeviceType(models.DeviceType{
		Id: "dt-1", Name: "KREISEL Ceramic Rotary Valve",
		Services: []models.Service{
			industryService("svc-1", "Get Current Consumption", models.EVENT, "p-moses"),
			industryService("svc-2", "Set Speed Level", models.REQUEST, "p-moses"),
		},
	}, "p-moses")

	if converted.Name != "KREISEL Ceramic Rotary Valve" || len(converted.Services) != 2 {
		t.Fatalf("unexpected conversion: %+v", converted)
	}
	sensor := converted.Services[0]
	if sensor.Id != "svc-1" || sensor.Name != "Get Current Consumption" {
		t.Errorf("a channel takes its id and name from the service: %+v", sensor)
	}
	if sensor.Direction != domain.Sensor {
		t.Errorf("an event service is a sensor, got %q", sensor.Direction)
	}
	if sensor.CharacteristicId != "urn:infai:ses:characteristic:b59c3965" {
		t.Errorf("the characteristic gives the value its unit, got %q", sensor.CharacteristicId)
	}
	//the query api addresses the value without the root
	if sensor.ValuePath != "value" {
		t.Errorf("expected the path below the root, got %q", sensor.ValuePath)
	}
	if converted.Services[1].Direction != domain.Actuator {
		t.Errorf("a request service is an actuator, got %q", converted.Services[1].Direction)
	}
}

// A device may speak several protocols; only what this service can drive
// belongs in the list, or the editor offers channels that never publish.
func TestServicesOfAnotherProtocolAreLeftOut(t *testing.T) {
	converted := convertDeviceType(models.DeviceType{
		Id: "dt-1", Name: "gemischt",
		Services: []models.Service{
			industryService("svc-1", "ours", models.EVENT, "p-moses"),
			industryService("svc-2", "somebody else's", models.EVENT, "p-mqtt"),
		},
	}, "p-moses")
	if len(converted.Services) != 1 || converted.Services[0].Id != "svc-1" {
		t.Errorf("expected only the service of our protocol, got %+v", converted.Services)
	}
}

func TestDirectionOfEveryInteraction(t *testing.T) {
	//event+request is measured and can additionally be asked: what a simulation
	//has to produce on a schedule is the measurement
	for interaction, want := range map[models.Interaction]domain.Direction{
		models.EVENT:             domain.Sensor,
		models.EVENT_AND_REQUEST: domain.Sensor,
		models.REQUEST:           domain.Actuator,
	} {
		if got := directionOf(interaction); got != want {
			t.Errorf("%q: expected %q, got %q", interaction, want, got)
		}
	}
}

func TestAValueNestedDeeperIsStillFound(t *testing.T) {
	service := models.Service{
		Id: "svc", ProtocolId: "p", Interaction: models.EVENT,
		Outputs: []models.Content{{ContentVariable: models.ContentVariable{
			Name: "root",
			SubContentVariables: []models.ContentVariable{{
				Name: "measurement",
				SubContentVariables: []models.ContentVariable{{
					Name: "reading", CharacteristicId: "char-deep",
				}},
			}},
		}}},
	}
	converted := convertDeviceType(models.DeviceType{Services: []models.Service{service}}, "p")
	if converted.Services[0].CharacteristicId != "char-deep" {
		t.Errorf("expected the nested characteristic, got %q", converted.Services[0].CharacteristicId)
	}
	if converted.Services[0].ValuePath != "measurement.reading" {
		t.Errorf("expected a dotted path without the root, got %q", converted.Services[0].ValuePath)
	}
}

// A service without a characteristic anywhere is still offered: the user can
// pick the unit by hand, and hiding the service would hide the measuring point.
func TestAServiceWithoutACharacteristicIsStillOffered(t *testing.T) {
	service := models.Service{Id: "svc", ProtocolId: "p", Interaction: models.EVENT,
		Outputs: []models.Content{{ContentVariable: models.ContentVariable{Name: "state"}}}}
	converted := convertDeviceType(models.DeviceType{Services: []models.Service{service}}, "p")
	if len(converted.Services) != 1 || converted.Services[0].CharacteristicId != "" {
		t.Errorf("expected the service without a characteristic, got %+v", converted.Services)
	}
}

// --- the boundaries: the registry and the device-manager ---

type fakeRegistry struct {
	pages     [][]models.DeviceType
	protocols []models.Protocol
	calls     []deviceRepo.DeviceTypeListOptions
	err       error

	// device is what a read of any id answers with. readCode is the status a
	// failing read reports and zero means success, which is what the real client
	// does - it carries a status only when it failed. Setting it puts a device
	// that is already gone, or an unreachable repository, in front of a rename.
	device      models.Device
	readCode    int
	readIds     []string
	readActions []deviceRepoModel.AuthAction
}

func (this *fakeRegistry) ListDeviceTypesV3(token string, options deviceRepo.DeviceTypeListOptions) ([]models.DeviceType, int64, error, int) {
	if this.err != nil {
		return nil, 0, this.err, 500
	}
	this.calls = append(this.calls, options)
	index := int(options.Offset) / listLimit
	if index >= len(this.pages) {
		return []models.DeviceType{}, 0, nil, 200
	}
	return this.pages[index], 0, nil, 200
}

func (this *fakeRegistry) ListProtocols(token string, limit int64, offset int64, sort string) ([]models.Protocol, error, int) {
	if this.err != nil {
		return nil, this.err, 500
	}
	return this.protocols, nil, 200
}

func (this *fakeRegistry) ReadDevice(id string, token string, action deviceRepoModel.AuthAction) (models.Device, error, int) {
	this.readIds = append(this.readIds, id)
	this.readActions = append(this.readActions, action)
	if this.readCode != 0 {
		return models.Device{}, errors.New("the device-repository answered " + http.StatusText(this.readCode)), this.readCode
	}
	device := this.device
	device.Id = id
	return device, nil, 0
}

func catalogWith(registry *fakeRegistry, managerUrl string) *Catalog {
	return &Catalog{repo: registry, managerUrl: managerUrl, protocol: "moses"}
}

func full(count int) []models.DeviceType {
	result := make([]models.DeviceType, count)
	for i := range result {
		result[i] = models.DeviceType{Id: "dt", Name: "dt"}
	}
	return result
}

func TestDeviceTypesResolvesTheProtocolAndPagesUntilTheEnd(t *testing.T) {
	registry := &fakeRegistry{
		protocols: []models.Protocol{{Id: "p-other", Handler: "mqtt"}, {Id: "p-moses", Handler: "moses"}},
		pages:     [][]models.DeviceType{full(listLimit), {{Id: "last", Name: "last"}}},
	}
	catalog := catalogWith(registry, "")

	result, err := catalog.DeviceTypes("Bearer t")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != listLimit+1 {
		t.Errorf("a full page has to be followed by the next, got %d", len(result))
	}
	for _, call := range registry.calls {
		if len(call.ProtocolIds) != 1 || call.ProtocolIds[0] != "p-moses" {
			t.Fatalf("the query has to filter by our protocol, got %v", call.ProtocolIds)
		}
	}
	//resolved once and remembered: it cannot change while the service runs
	if _, err = catalog.DeviceTypes("Bearer t"); err != nil {
		t.Fatal(err)
	}
	if catalog.protocolId != "p-moses" {
		t.Errorf("expected the protocol id to be cached, got %q", catalog.protocolId)
	}
}

// Without the protocol nothing is simulatable, and an empty list would read as
// "no device types exist" rather than as a misconfiguration.
func TestAMissingProtocolIsAnError(t *testing.T) {
	catalog := catalogWith(&fakeRegistry{protocols: []models.Protocol{{Id: "p", Handler: "mqtt"}}}, "")
	if _, err := catalog.DeviceTypes("Bearer t"); err == nil || !strings.Contains(err.Error(), "moses") {
		t.Errorf("expected an error naming the handler, got %v", err)
	}
}

func TestCreateDeviceSendsWhatTheManagerExpects(t *testing.T) {
	var seen struct {
		method, path, auth, body string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.method, seen.path, seen.auth, seen.body = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"urn:device:created","local_id":"abc","name":"Kompressor 1","device_type_id":"dt-1"}`))
	}))
	defer server.Close()

	device, err := catalogWith(&fakeRegistry{}, server.URL).CreateDevice(context.Background(), "Bearer t", "dt-1", "Kompressor 1")
	if err != nil {
		t.Fatal(err)
	}
	if device.Id != "urn:device:created" {
		t.Errorf("the id becomes the asset's external_ref, got %q", device.Id)
	}
	if seen.method != http.MethodPost || seen.path != "/devices" || seen.auth != "Bearer t" {
		t.Errorf("unexpected request: %+v", seen)
	}
	//a local id is generated: the protocol needs one and it has no meaning here
	if !strings.Contains(seen.body, `"device_type_id":"dt-1"`) || !strings.Contains(seen.body, `"local_id":"`) {
		t.Errorf("unexpected body: %s", seen.body)
	}
}

func TestDeleteDeviceAddressesTheDevice(t *testing.T) {
	seen := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if err := catalogWith(&fakeRegistry{}, server.URL).DeleteDevice(context.Background(), "Bearer t", "urn:device:old"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seen, "DELETE ") || !strings.Contains(seen, "urn:device:old") {
		t.Errorf("unexpected request: %q", seen)
	}
}

// The cleanup after an environment update is a best effort that may be repeated,
// and a device may have been removed in the platform's own ui in between. Both
// end here, and both got what they wanted.
func TestDeletingADeviceThatIsAlreadyGoneIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such device", http.StatusNotFound)
	}))
	defer server.Close()

	if err := catalogWith(&fakeRegistry{}, server.URL).DeleteDevice(context.Background(), "Bearer t", "urn:device:gone"); err != nil {
		t.Errorf("a device that is not there is the state the caller wanted, got %v", err)
	}
}

// Everything else stays an error: a device moses may not delete answers 403, and
// swallowing that would report a cleanup that never happened.
func TestADeviceThatCannotBeDeletedIsAnError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", status)
		}))
		err := catalogWith(&fakeRegistry{}, server.URL).DeleteDevice(context.Background(), "Bearer t", "urn:device:x")
		server.Close()
		if err == nil {
			t.Errorf("expected %d to be an error", status)
		}
	}
}

// A 404 on a create means the url is wrong, not that anything is already done -
// tolerating it there would store an asset publishing into nowhere.
func TestA404OnACreateIsStillAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no route", http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := catalogWith(&fakeRegistry{}, server.URL).CreateDevice(context.Background(), "Bearer t", "dt-1", "x"); err == nil {
		t.Error("a create that answered 404 must not pass as a created device")
	}
}

// A refusal from the manager has to carry its answer: "unable to create the
// device" alone leaves the user without a reason.
func TestAManagerRefusalCarriesItsAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "device type does not exist", http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := catalogWith(&fakeRegistry{}, server.URL).CreateDevice(context.Background(), "Bearer t", "dt-gone", "x")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "device type does not exist") {
		t.Errorf("expected the status and the answer, got %v", err)
	}
}

func TestAnUnreadableAnswerIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("kein json"))
	}))
	defer server.Close()

	if _, err := catalogWith(&fakeRegistry{}, server.URL).CreateDevice(context.Background(), "Bearer t", "dt-1", "x"); err == nil {
		t.Error("an unreadable answer must not pass as a created device")
	}
}

// The device-manager takes the whole device on a put, so everything the rename
// does not touch has to survive it: a local id dropped here detaches the device
// from the protocol, and a lost device type id detaches it from its services.
func TestRenameDevicePutsTheWholeDeviceBack(t *testing.T) {
	registry := &fakeRegistry{device: models.Device{
		LocalId:      "local-abc",
		Name:         "Kompressor 1",
		DeviceTypeId: "dt-1",
		OwnerId:      "user-a",
		Attributes:   []models.Attribute{{Key: "senergy/local-mqtt", Value: "true", Origin: "web-ui"}},
	}}
	var seen struct {
		method, path, auth, body string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.method, seen.path, seen.auth, seen.body = r.Method, r.URL.Path, r.Header.Get("Authorization"), string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"urn:device:x","name":"Kompressor 2"}`))
	}))
	defer server.Close()

	if err := catalogWith(registry, server.URL).RenameDevice(context.Background(), "Bearer t", "urn:device:x", "Kompressor 2"); err != nil {
		t.Fatal(err)
	}
	if registry.readIds == nil || registry.readIds[0] != "urn:device:x" {
		t.Errorf("the current device has to be read back first, read %v", registry.readIds)
	}
	//the read has to demand write rights, or a caller who may only read the
	//device gets past it and fails on the put instead
	if len(registry.readActions) != 1 || registry.readActions[0] != deviceRepoModel.WRITE {
		t.Errorf("the read has to ask for %q, asked %v", deviceRepoModel.WRITE, registry.readActions)
	}
	if seen.method != http.MethodPut || seen.path != "/devices/urn:device:x" || seen.auth != "Bearer t" {
		t.Errorf("unexpected request: %+v", seen)
	}
	if !strings.Contains(seen.body, `"name":"Kompressor 2"`) {
		t.Errorf("the new name has to be written, got %s", seen.body)
	}
	for _, kept := range []string{`"local_id":"local-abc"`, `"device_type_id":"dt-1"`, `"senergy/local-mqtt"`, `"id":"urn:device:x"`, `"owner_id":"user-a"`} {
		if !strings.Contains(seen.body, kept) {
			t.Errorf("the put replaces the whole device, so %s has to survive it, got %s", kept, seen.body)
		}
	}
}

// The read goes to the device-repository, which trails the device-manager the put
// goes to. A read that reports the wanted name may therefore be two renames
// behind - so the put happens anyway, or renaming a device from A to B and back
// to A inside that window would leave it at B forever.
func TestRenamingToTheNameTheReadReportsStillWrites(t *testing.T) {
	registry := &fakeRegistry{device: models.Device{Name: "Kompressor 1", LocalId: "local-abc"}}
	bodies := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := catalogWith(registry, server.URL).RenameDevice(context.Background(), "Bearer t", "urn:device:x", "Kompressor 1"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("a stale read that happens to match must not swallow the write, put %d times", len(bodies))
	}
	if !strings.Contains(bodies[0], `"name":"Kompressor 1"`) {
		t.Errorf("the wanted name has to be written, got %s", bodies[0])
	}
}

// The device-repository's read adds the owner's account-wide default attributes
// to the answer. They are not stored on the device, and putting them back would
// make them stored - the default could then never be changed for this device
// again.
func TestRenameDeviceDropsTheAttributesTheReadInjected(t *testing.T) {
	registry := &fakeRegistry{device: models.Device{
		Name:    "alt",
		LocalId: "local-abc",
		Attributes: []models.Attribute{
			{Key: "shared/nickname", Value: "Halle 1", Origin: "web-ui"},
			{Key: "senergy/time-path", Value: "time", Origin: defaultAttributeOrigin},
		},
	}}
	body := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := catalogWith(registry, server.URL).RenameDevice(context.Background(), "Bearer t", "urn:device:x", "neu"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"shared/nickname"`) {
		t.Errorf("an attribute stored on the device has to survive the put, got %s", body)
	}
	if strings.Contains(body, `"senergy/time-path"`) {
		t.Errorf("an attribute the read injected must not be written back, got %s", body)
	}
}

// Unlike a delete, a 404 does not reach the goal state: the asset still points at
// this device, so the caller has to see that the rename did not happen. Both
// halves of the call have to report it.
func TestRenamingADeviceThatIsGoneIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such device", http.StatusNotFound)
	}))
	defer server.Close()

	//gone by the time the write lands
	registry := &fakeRegistry{device: models.Device{Name: "alt"}}
	err := catalogWith(registry, server.URL).RenameDevice(context.Background(), "Bearer t", "urn:device:gone", "neu")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("a 404 on the write means the rename did not happen, got %v", err)
	}

	//and gone already when it is read
	registry = &fakeRegistry{readCode: http.StatusNotFound}
	err = catalogWith(registry, server.URL).RenameDevice(context.Background(), "Bearer t", "urn:device:gone", "neu")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("a 404 on the read means the rename did not happen, got %v", err)
	}
}

// Everything else stays an error, or a rename that never happened is reported as
// done and the device keeps its old name silently.
func TestARenameThatWasRefusedIsAnError(t *testing.T) {
	registry := &fakeRegistry{device: models.Device{Name: "alt"}}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", status)
		}))
		err := catalogWith(registry, server.URL).RenameDevice(context.Background(), "Bearer t", "urn:device:x", "neu")
		server.Close()
		if err == nil {
			t.Errorf("expected %d to be an error", status)
		}
	}
	//and so does a read that failed for any other reason: renaming from a device
	//that could not be read would write an empty local id back
	registry = &fakeRegistry{readCode: http.StatusInternalServerError}
	if err := catalogWith(registry, "").RenameDevice(context.Background(), "Bearer t", "urn:device:x", "neu"); err == nil {
		t.Error("a failing read must not pass as a rename")
	}
}
