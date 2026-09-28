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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/gin-gonic/gin"
)

// platformDouble stands in for the device-repository and the device-manager
// behind a real devices.Catalog, so the check runs through the same reads it
// makes in production.
type platformDouble struct {
	mux     sync.Mutex
	types   map[string]models.DeviceType
	devices map[string]models.Device
	// readOnly are devices the caller may read and not write.
	readOnly map[string]bool
	// status, when set, is what every read answers with.
	status int

	typeReads     map[string]int
	typeQueries   int
	deviceQueries [][]string
	created       []string
}

func newPlatformDouble() *platformDouble {
	return &platformDouble{
		types: map[string]models.DeviceType{
			"dt-1": {Id: "dt-1", Services: []models.Service{
				{Id: "svc-power", ProtocolId: "p-moses"},
				{Id: "svc-mqtt", ProtocolId: "p-other"},
			}},
			"dt-2":       {Id: "dt-2", Services: []models.Service{{Id: "svc-energy", ProtocolId: "p-moses"}}},
			"dt-foreign": {Id: "dt-foreign", Services: []models.Service{{Id: "svc-f", ProtocolId: "p-other"}}},
		},
		devices: map[string]models.Device{
			"urn:device:picked": {Id: "urn:device:picked", DeviceTypeId: "dt-2"},
			"urn:device:shared": {Id: "urn:device:shared", DeviceTypeId: "dt-2"},
		},
		readOnly:  map[string]bool{"urn:device:shared": true},
		typeReads: map[string]int{},
	}
}

func (this *platformDouble) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.status != 0 {
		http.Error(writer, "down", this.status)
		return
	}
	//a gateway in front of the repository refuses a long request line
	if len(request.RequestURI) > 8192 {
		http.Error(writer, "uri too long", http.StatusRequestURITooLong)
		return
	}
	ids := strings.Split(strings.TrimSpace(request.URL.Query().Get("ids")), ",")
	var result interface{}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/protocols":
		result = []models.Protocol{{Id: "p-other", Handler: "mqtt"}, {Id: "p-moses", Handler: "moses"}}
	case request.Method == http.MethodGet && request.URL.Path == "/v3/device-types":
		this.typeQueries++
		found := []models.DeviceType{}
		for _, id := range ids {
			this.typeReads[id]++
			if deviceType, ok := this.types[id]; ok {
				found = append(found, deviceType)
			}
		}
		result = found
	case request.Method == http.MethodGet && request.URL.Path == "/devices":
		this.deviceQueries = append(this.deviceQueries, ids)
		permission := request.URL.Query().Get("p")
		found := []models.Device{}
		for _, id := range ids {
			//the repository defaults to read
			if device, ok := this.devices[id]; ok && (!this.readOnly[id] || permission == "" || permission == "r") {
				found = append(found, device)
			}
		}
		result = found
	case request.Method == http.MethodPost && request.URL.Path == "/devices":
		device := models.Device{}
		if err := json.NewDecoder(request.Body).Decode(&device); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		device.Id = "urn:device:created-" + device.LocalId
		this.created = append(this.created, device.Id)
		result = device
	default:
		http.NotFound(writer, request)
		return
	}
	_ = json.NewEncoder(writer).Encode(result)
}

func (this *platformDouble) setStatus(status int) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.status = status
}

// platformWitnesses is validateWitnesses with the real catalog in front of a
// platform double.
type platformWitnesses struct {
	store    *fakeEnvironments
	shares   *fakeShares
	platform *platformDouble
	server   *httptest.Server
	mirror   *fakeGraphMirror
	notifier *recordingNotifier
	router   *gin.Engine
}

func newPlatformWitnesses(t *testing.T) platformWitnesses {
	t.Helper()
	w := platformWitnesses{
		store:    newFakeEnvironments(),
		shares:   newFakeShares(),
		platform: newPlatformDouble(),
		mirror:   newFakeGraphMirror(),
		notifier: &recordingNotifier{},
	}
	w.server = httptest.NewServer(w.platform)
	t.Cleanup(w.server.Close)
	catalog := devices.NewCatalog(w.server.URL, w.server.URL, "moses")
	w.router = testRouterWithAll(w.store, w.shares, catalog, w.mirror, w.notifier, newFakePermissions())
	return w
}

func (this platformWitnesses) wrote() []string {
	result := []string{}
	add := func(what string, count int) {
		if count > 0 {
			result = append(result, what)
		}
	}
	add("environment store", len(this.store.stored))
	add("share store save", len(this.shares.saves))
	add("share store delete", len(this.shares.deletes))
	add("device created", len(this.platform.created))
	add("graph written", len(this.mirror.sent))
	add("runtime reloaded", len(this.notifier.reloaded))
	return result
}

// checkedEnvironment is valid on its own and against the double: a new asset of
// dt-1, and an asset with a device of dt-2 attached by hand.
func checkedEnvironment() domain.Environment {
	sensor := func(name string, service string) domain.Channel {
		return domain.Channel{Name: name, Direction: domain.Sensor, IntervalSeconds: 30, ExternalRef: service,
			Source: domain.Source{Kind: domain.SourceProfile, Profile: &domain.ProfileSource{Base: 1}}}
	}
	return domain.Environment{
		Name: "Metallbau", Type: domain.IndustrialSite,
		Zones: []domain.Zone{{
			Name: "Halle 1", Type: domain.ZoneHall,
			Assets: []domain.Asset{
				{Name: "Kompressor 1", Kind: domain.AssetMachine, ExternalTypeId: "dt-1",
					Channels: []domain.Channel{sensor("Leistung", "svc-power")}},
				{Name: "Zähler", Kind: domain.AssetMeter, ExternalTypeId: "dt-2", ExternalRef: "urn:device:picked",
					Channels: []domain.Channel{sensor("Energie", "svc-energy")}},
			},
		}},
	}
}

// uncheckableEnvironment breaks one rule of Validate and every rule of the
// platform check, so the merged answer is compared as a whole.
func uncheckableEnvironment() domain.Environment {
	env := checkedEnvironment()
	env.Name = ""
	machine := env.Zones[0].Assets[0]
	unknownType := machine
	unknownType.Name, unknownType.ExternalTypeId = "Unbekannt", "dt-missing"
	machine.Channels = append(machine.Channels, machine.Channels[0], machine.Channels[0])
	machine.Channels[1].Name, machine.Channels[1].ExternalRef = "Fehlt", "svc-missing"
	machine.Channels[2].Name, machine.Channels[2].ExternalRef = "Falsches Protokoll", "svc-mqtt"
	foreign := domain.Asset{Name: "Fremd", Kind: domain.AssetMachine, ExternalTypeId: "dt-foreign"}
	gone := env.Zones[0].Assets[1]
	gone.Name, gone.ExternalRef = "Weg", "urn:device:gone"
	otherType := env.Zones[0].Assets[1]
	otherType.Name, otherType.ExternalTypeId, otherType.Channels = "Anderer Typ", "dt-1", nil
	env.Zones[0].Assets = []domain.Asset{unknownType, machine, foreign, gone, otherType}
	return env
}

var uncheckableProblemPaths = []string{
	"name",
	"zones[0].assets[0].external_type_id",
	"zones[0].assets[1].channels[1].external_ref",
	"zones[0].assets[1].channels[2].external_ref",
	"zones[0].assets[2].external_type_id",
	"zones[0].assets[3].external_ref",
	"zones[0].assets[4].external_ref",
}

func problemPathsOf(t *testing.T, resp *httptest.ResponseRecorder) []string {
	t.Helper()
	answer := domain.ValidationError{}
	if err := json.Unmarshal(resp.Body.Bytes(), &answer); err != nil {
		t.Fatalf("expected the problem list, got %s", resp.Body.String())
	}
	result := []string{}
	for _, problem := range answer.Problems {
		result = append(result, problem.Path)
	}
	return result
}

func TestThePlatformCheckRefusesWhatThePlatformCannotServeOnEveryRouteAndWritesNothing(t *testing.T) {
	w := newPlatformWitnesses(t)
	storeDirectly(t, w.store, "env-stored", "user-a", minimalEnvironment())
	stored := len(w.store.stored)
	w.shares.set("env-new", []string{"former-user"}, nil)
	env := uncheckableEnvironment()

	validated := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", env)
	if validated.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", validated.Code, validated.Body.String())
	}
	//sorted by path, the pure problem and the platform ones in one list
	if got := problemPathsOf(t, validated); strings.Join(got, " ") != strings.Join(uncheckableProblemPaths, " ") {
		t.Fatalf("expected problems at\n%v\ngot\n%v\n%s", uncheckableProblemPaths, got, validated.Body.String())
	}
	for route, resp := range map[string]*httptest.ResponseRecorder{
		"POST":             do(t, w.router, http.MethodPost, "/environments", "user-a", env),
		"PUT to a new id":  do(t, w.router, http.MethodPut, "/environments/env-new", "user-a", env),
		"PUT over a store": do(t, w.router, http.MethodPut, "/environments/env-stored", "user-a", env),
	} {
		if resp.Code != validated.Code || resp.Body.String() != validated.Body.String() {
			t.Errorf("validate and %s answer differently:\nvalidate %d %s\n%s %d %s",
				route, validated.Code, validated.Body.String(), route, resp.Code, resp.Body.String())
		}
	}
	if len(w.store.stored) != stored || w.store.stored["env-stored"].Version != 0 {
		t.Errorf("no document may be written, got %+v", w.store.stored)
	}
	if wrote := w.wrote(); len(wrote) != 1 || wrote[0] != "environment store" {
		t.Errorf("a refused document must write nothing beyond the precondition, but wrote to: %v", wrote)
	}
	if !w.shares.has("env-new") {
		t.Error("a refused put must leave the leftover share set")
	}
}

func TestThePlatformCheckPassesADocumentThePlatformServes(t *testing.T) {
	w := newPlatformWitnesses(t)
	env := checkedEnvironment()

	if resp := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", env); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if w.platform.typeReads["dt-1"] != 1 || w.platform.typeReads["dt-2"] != 1 {
		t.Errorf("expected both device types read, got %v", w.platform.typeReads)
	}
	if len(w.platform.deviceQueries) != 1 || strings.Join(w.platform.deviceQueries[0], ",") != "urn:device:picked" {
		t.Errorf("expected the attached device read, got %v", w.platform.deviceQueries)
	}
	if wrote := w.wrote(); len(wrote) != 0 {
		t.Fatalf("validate must write nothing, but wrote to: %v", wrote)
	}

	resp := do(t, w.router, http.MethodPost, "/environments", "user-a", env)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(w.platform.created) != 1 {
		t.Errorf("the new asset has to get its device, got %v", w.platform.created)
	}
}

// Moses publishes into an attached device with its own account, so attaching a
// device the caller may read but not write is refused on every route.
func TestAttachingADeviceTheCallerMayOnlyReadIsRefusedOnEveryRoute(t *testing.T) {
	w := newPlatformWitnesses(t)
	w.shares.set("env-new", []string{"former-user"}, nil)
	env := checkedEnvironment()
	env.Zones[0].Assets[1].ExternalRef = "urn:device:shared"
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/environments/validate"},
		{http.MethodPost, "/environments"},
		{http.MethodPut, "/environments/env-new"},
	} {
		resp := do(t, w.router, route.method, route.path, "user-a", env)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: expected 400, got %d: %s", route.method, route.path, resp.Code, resp.Body.String())
		}
		if got := problemPathsOf(t, resp); len(got) != 1 || got[0] != "zones[0].assets[1].external_ref" {
			t.Errorf("%s %s: expected the attached device reported, got %v", route.method, route.path, got)
		}
		if !strings.Contains(resp.Body.String(), "may not write") {
			t.Errorf("%s %s: expected the message to name the missing write right, got %s", route.method, route.path, resp.Body.String())
		}
	}
	if wrote := w.wrote(); len(wrote) != 0 {
		t.Fatalf("a refused document must write nothing, but wrote to: %v", wrote)
	}
}

// A device the previous save created may not be in the device-repository yet,
// which trails the device-manager, so a PUT does not read again what the stored
// document already carried. A changed device type makes it a new attachment.
func TestAPutDoesNotReadAgainADeviceTheStoredDocumentCarries(t *testing.T) {
	w := newPlatformWitnesses(t)
	env := checkedEnvironment()
	env.Zones[0].Assets[1].ExternalRef = "urn:device:not-yet-readable"
	storeDirectly(t, w.store, "env-1", "user-a", env)

	if resp := do(t, w.router, http.MethodPut, "/environments/env-1", "user-a", w.store.stored["env-1"]); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(w.platform.deviceQueries) != 0 {
		t.Errorf("the stored device must not be read again, got %v", w.platform.deviceQueries)
	}

	//a deep copy: the fake store hands out its own slices
	changed := domain.Environment{}
	raw, err := json.Marshal(w.store.stored["env-1"])
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Zones[0].Assets[1].ExternalTypeId = "dt-1"
	changed.Zones[0].Assets[1].Channels[0].ExternalRef = "svc-power"
	resp := do(t, w.router, http.MethodPut, "/environments/env-1", "user-a", changed)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := problemPathsOf(t, resp); len(got) != 1 || got[0] != "zones[0].assets[1].external_ref" {
		t.Errorf("expected the device reported, got %v", got)
	}

	//a copy under a new id owns nothing, so it reads everything
	copied := do(t, w.router, http.MethodPut, "/environments/env-copy", "user-a", w.store.stored["env-1"])
	if copied.Code != http.StatusBadRequest {
		t.Errorf("expected the copy's device read and refused, got %d: %s", copied.Code, copied.Body.String())
	}
}

func TestAnUnreadablePlatformIs502OnEveryRouteAndWritesNothing(t *testing.T) {
	for _, failure := range []string{"answers 503", "is unreachable"} {
		t.Run(failure, func(t *testing.T) {
			w := newPlatformWitnesses(t)
			w.shares.set("env-new", []string{"former-user"}, nil)
			if failure == "answers 503" {
				w.platform.setStatus(http.StatusServiceUnavailable)
			} else {
				w.server.Close()
			}
			//a valid document and an invalid one: a 400 has to carry every problem,
			//so neither may pass or be refused on what could be checked
			for _, env := range []domain.Environment{checkedEnvironment(), uncheckableEnvironment()} {
				for _, route := range []struct{ method, path string }{
					{http.MethodPost, "/environments/validate"},
					{http.MethodPost, "/environments"},
					{http.MethodPut, "/environments/env-new"},
				} {
					resp := do(t, w.router, route.method, route.path, "user-a", env)
					if resp.Code != http.StatusBadGateway {
						t.Errorf("%s %s: expected 502, got %d: %s", route.method, route.path, resp.Code, resp.Body.String())
					}
					if !strings.Contains(resp.Body.String(), "platform") {
						t.Errorf("%s %s: expected the message to name the platform, got %s", route.method, route.path, resp.Body.String())
					}
				}
			}
			if wrote := w.wrote(); len(wrote) != 0 {
				t.Fatalf("a failed check must write nothing, but wrote to: %v", wrote)
			}
			if !w.shares.has("env-new") {
				t.Error("a failed check must leave the leftover share set")
			}
		})
	}
}

// The demonstrator document has 51 assets over 8 device types: a check of it is
// one query for the types, and every type is read once.
func TestEveryDistinctDeviceTypeIsReadOnce(t *testing.T) {
	raw, err := os.ReadFile("../effects/testdata/musterwerke.json")
	if err != nil {
		t.Fatal(err)
	}
	env := domain.Environment{}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	w := newPlatformWitnesses(t)
	w.platform.types = map[string]models.DeviceType{}
	assets := 0
	forEachAsset(&env, func(asset *domain.Asset) {
		assets++
		deviceType := w.platform.types[asset.ExternalTypeId]
		deviceType.Id = asset.ExternalTypeId
		for _, channel := range asset.Channels {
			deviceType.Services = append(deviceType.Services, models.Service{Id: channel.ExternalRef, ProtocolId: "p-moses"})
		}
		w.platform.types[asset.ExternalTypeId] = deviceType
	})
	if assets != 51 || len(w.platform.types) != 8 {
		t.Fatalf("expected the demonstrator's 51 assets over 8 types, got %d over %d", assets, len(w.platform.types))
	}

	started := time.Now()
	resp := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", env)
	elapsed := time.Since(started)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if w.platform.typeQueries != 1 {
		t.Errorf("expected one query for the types, got %d", w.platform.typeQueries)
	}
	read := []string{}
	for id, reads := range w.platform.typeReads {
		read = append(read, id)
		if reads != 1 {
			t.Errorf("the type %s was read %d times", id, reads)
		}
	}
	sort.Strings(read)
	if len(read) != 8 {
		t.Errorf("expected the 8 types read, got %v", read)
	}
	if elapsed > time.Second {
		t.Errorf("the check took %v", elapsed)
	}
}

// ---------------------------------------------------------------------------
// A refused PUT under a new id leaves the leftover share set
// ---------------------------------------------------------------------------

func TestARefusedPutUnderANewIdLeavesTheLeftoverShareSet(t *testing.T) {
	cases := map[string]struct {
		env     domain.Environment
		catalog *fakeCatalog
		status  int
	}{
		"invalid":           {invalidEnvironment(), &fakeCatalog{}, http.StatusBadRequest},
		"refused by check":  {minimalEnvironment(), &fakeCatalog{checkProblems: []domain.Problem{{Path: "zones[0].assets[0].external_type_id", Message: "unknown"}}}, http.StatusBadRequest},
		"check unreachable": {minimalEnvironment(), &fakeCatalog{checkErr: errors.New("device-repository unreachable")}, http.StatusBadGateway},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeEnvironments()
			shares := newFakeShares()
			shares.set("env-9", []string{"former-user"}, nil)
			router := testRouterWithAll(store, shares, c.catalog, nil, nil, newFakePermissions())

			resp := do(t, router, http.MethodPut, "/environments/env-9", "user-a", c.env)
			if resp.Code != c.status {
				t.Fatalf("expected %d, got %d: %s", c.status, resp.Code, resp.Body.String())
			}
			if got := shares.users("env-9"); len(got) != 1 || got[0] != "former-user" {
				t.Errorf("a refused put must leave the set, got %v", got)
			}
			if len(shares.deletes) != 0 || len(store.stored) != 0 || len(c.catalog.created) != 0 {
				t.Errorf("nothing may be written, got share deletes %v, documents %d, devices %v",
					shares.deletes, len(store.stored), c.catalog.created)
			}
		})
	}
}

// A document over the node limit is refused by Validate, and its thousands of
// references must not become as many platform reads first.
func TestADocumentOverTheNodeLimitIsNotCheckedAgainstThePlatform(t *testing.T) {
	catalog := &fakeCatalog{}
	router := testRouterWithAll(newFakeEnvironments(), nil, catalog, nil, nil, nil)
	env := minimalEnvironment()
	for len(env.Zones[0].Assets)*2+1 <= domain.MaxNodes {
		env.Zones[0].Assets = append(env.Zones[0].Assets, env.Zones[0].Assets[0])
	}
	resp := do(t, router, http.MethodPost, "/environments/validate", "user-a", env)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.Code)
	}
	if len(catalog.checked) != 0 {
		t.Errorf("expected no platform check, got one over %d assets", len(catalog.checked[0]))
	}

	//and one exactly at the limit is checked: the zone, 4999 assets of two nodes
	//and one without a channel
	env.Zones[0].Assets = env.Zones[0].Assets[:(domain.MaxNodes-1)/2]
	bare := env.Zones[0].Assets[0]
	bare.Channels = nil
	env.Zones[0].Assets = append(env.Zones[0].Assets, bare)
	if nodes := domain.WalkAssets(env, func(string, domain.Asset) {}); nodes != domain.MaxNodes {
		t.Fatalf("expected the document at %d nodes, got %d", domain.MaxNodes, nodes)
	}
	if resp := do(t, router, http.MethodPost, "/environments/validate", "user-a", env); resp.Code != http.StatusOK {
		t.Fatalf("expected 200 at the limit, got %d: %.300s", resp.Code, resp.Body.String())
	}
	if len(catalog.checked) != 1 {
		t.Errorf("expected the document at the limit checked, got %d checks", len(catalog.checked))
	}
}

// An oversized id is Validate's 400 at its field; read from the platform it
// would fail the query and turn the answer into a 502 worth retrying.
func TestAnOversizedIdIsRefusedAndNotReadFromThePlatform(t *testing.T) {
	w := newPlatformWitnesses(t)
	env := checkedEnvironment()
	long := "urn:infai:ses:device-type:" + strings.Repeat("x", 10<<10)
	env.Zones[0].Assets[0].ExternalTypeId = long
	env.Zones[0].Assets[1].ExternalRef = long
	env.Zones[0].Assets[1].Channels[0].ExternalRef = long
	resp := do(t, w.router, http.MethodPost, "/environments/validate", "user-a", env)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %.300s", resp.Code, resp.Body.String())
	}
	expected := []string{
		"zones[0].assets[0].external_type_id",
		"zones[0].assets[1].channels[0].external_ref",
		"zones[0].assets[1].external_ref",
	}
	if got := problemPathsOf(t, resp); strings.Join(got, " ") != strings.Join(expected, " ") {
		t.Errorf("expected problems at %v, got %v", expected, got)
	}
}

// The problems of both kinds arrive as one list sorted by path, whichever kind
// found a problem first.
func TestPlatformProblemsAreSortedIntoTheProblemsOfValidate(t *testing.T) {
	catalog := &fakeCatalog{checkProblems: []domain.Problem{
		{Path: "zones[0].assets[0].external_type_id", Message: "unknown"},
	}}
	router := testRouterWithAll(newFakeEnvironments(), nil, catalog, nil, nil, nil)
	env := minimalEnvironment()
	env.Zones[0].Type = "nonsense"
	resp := do(t, router, http.MethodPost, "/environments/validate", "user-a", env)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := problemPathsOf(t, resp); strings.Join(got, " ") != "zones[0].assets[0].external_type_id zones[0].type" {
		t.Errorf("expected the platform problem sorted before the one of Validate, got %v", got)
	}
}
