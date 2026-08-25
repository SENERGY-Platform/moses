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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	deviceRepo "github.com/SENERGY-Platform/device-repository/lib/client"
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
