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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// repositoryDouble answers the three list reads of the check the way the
// device-repository does: by ids, leaving out what it does not hold.
type repositoryDouble struct {
	mux       sync.Mutex
	protocols []models.Protocol
	types     map[string]models.DeviceType
	devices   map[string]models.Device
	// readOnly are devices the caller may read and nothing more, shared with it
	// for instance; a query for any other right leaves them out.
	readOnly map[string]bool

	// status, when set, is what every read answers with.
	status int
	// hold keeps every read open until the request gives up.
	hold bool

	protocolReads int
	typeQueries   [][]string
	deviceQueries [][]string
	permissions   []string
	tokens        []string
	longestUri    int
}

func newRepositoryDouble() *repositoryDouble {
	return &repositoryDouble{
		protocols: []models.Protocol{{Id: "p-other", Handler: "mqtt"}, {Id: "p-moses", Handler: "moses"}},
		types: map[string]models.DeviceType{
			"dt-1": {Id: "dt-1", Name: "Kompressor", Services: []models.Service{
				{Id: "svc-power", ProtocolId: "p-moses", Interaction: models.EVENT},
				{Id: "svc-mqtt", ProtocolId: "p-other", Interaction: models.EVENT},
				//a declared time path that names a whole output: the publisher refuses every reading
				{Id: "svc-timed", ProtocolId: "p-moses", Interaction: models.EVENT,
					Attributes: []models.Attribute{{Key: TimePathAttribute, Value: "time"}}},
			}},
			"dt-2":       {Id: "dt-2", Name: "Zähler", Services: []models.Service{{Id: "svc-energy", ProtocolId: "p-moses"}}},
			"dt-foreign": {Id: "dt-foreign", Name: "Fremd", Services: []models.Service{{Id: "svc-f", ProtocolId: "p-other"}}},
		},
		devices: map[string]models.Device{
			"urn:device:picked": {Id: "urn:device:picked", DeviceTypeId: "dt-2"},
			"urn:device:shared": {Id: "urn:device:shared", DeviceTypeId: "dt-2"},
		},
		readOnly: map[string]bool{"urn:device:shared": true},
	}
}

// idsOf splits the parameter like the device-repository does, trim included.
func idsOf(request *http.Request) []string {
	return strings.Split(strings.TrimSpace(request.URL.Query().Get("ids")), ",")
}

func (this *repositoryDouble) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	this.mux.Lock()
	this.tokens = append(this.tokens, request.Header.Get("Authorization"))
	this.longestUri = max(this.longestUri, len(request.RequestURI))
	status, hold := this.status, this.hold
	this.mux.Unlock()
	if hold {
		<-request.Context().Done()
		return
	}
	if status != 0 {
		http.Error(writer, "the repository is down", status)
		return
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	var result interface{}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/protocols":
		this.protocolReads++
		result = this.protocols
	case request.Method == http.MethodGet && request.URL.Path == "/v3/device-types":
		ids := idsOf(request)
		this.typeQueries = append(this.typeQueries, ids)
		found := []models.DeviceType{}
		for _, id := range ids {
			if deviceType, ok := this.types[id]; ok {
				found = append(found, deviceType)
			}
		}
		result = found
	case request.Method == http.MethodGet && request.URL.Path == "/devices":
		ids := idsOf(request)
		this.deviceQueries = append(this.deviceQueries, ids)
		permission := request.URL.Query().Get("p")
		this.permissions = append(this.permissions, permission)
		found := []models.Device{}
		for _, id := range ids {
			//the repository defaults to read
			if device, ok := this.devices[id]; ok && (!this.readOnly[id] || permission == "" || permission == "r") {
				found = append(found, device)
			}
		}
		result = found
	default:
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(result)
}

func checkingCatalog(t *testing.T, repository http.Handler) *Catalog {
	t.Helper()
	server := httptest.NewServer(repository)
	t.Cleanup(server.Close)
	return NewCatalog(server.URL, server.URL, "moses")
}

func problemPaths(problems []domain.Problem) []string {
	result := []string{}
	for _, problem := range problems {
		result = append(result, problem.Path)
	}
	sort.Strings(result)
	return result
}

func TestTheCheckReportsEveryReferenceTheRuntimeCannotUse(t *testing.T) {
	repository := newRepositoryDouble()
	catalog := checkingCatalog(t, repository)
	assets := []AssetReference{
		{Path: "a0", DeviceTypeId: "dt-missing"},
		{Path: "a1", DeviceTypeId: "dt-1", Channels: []ChannelReference{
			{Path: "a1.c0", ServiceId: "svc-power", Publishes: true},
			{Path: "a1.c1", ServiceId: "svc-missing", Publishes: true},
			{Path: "a1.c2", ServiceId: "svc-mqtt", Publishes: true},
			{Path: "a1.c3", ServiceId: "svc-timed", Publishes: true},
			//an actuator publishes no reading, so the time path does not concern it
			{Path: "a1.c4", ServiceId: "svc-timed", Publishes: false},
			//a channel without a service publishes nowhere and needs nothing read
			{Path: "a1.c5", ServiceId: ""},
		}},
		{Path: "a2", DeviceTypeId: "dt-foreign"},
		{Path: "a3", DeviceTypeId: "dt-2", DeviceId: "urn:device:gone"},
		{Path: "a4", DeviceTypeId: "dt-1", DeviceId: "urn:device:picked"},
		{Path: "a5", DeviceTypeId: "dt-2", DeviceId: "urn:device:picked"},
		//Validate reports an empty type; the check neither reads nor repeats it
		{Path: "a6", DeviceTypeId: "  "},
	}
	problems, err := catalog.CheckReferences(context.Background(), "Bearer t", assets)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"a0.external_type_id",
		"a1.c1.external_ref",
		"a1.c2.external_ref",
		"a1.c3.external_ref",
		"a2.external_type_id",
		"a3.external_ref",
		"a4.external_ref",
	}
	if got := problemPaths(problems); strings.Join(got, " ") != strings.Join(expected, " ") {
		t.Fatalf("expected problems at\n%v\ngot\n%v\n%+v", expected, got, problems)
	}
	for _, problem := range problems {
		if problem.Path == "a4.external_ref" && !strings.Contains(problem.Message, `"dt-2"`) {
			t.Errorf("a device of another type has to name its own type, got %q", problem.Message)
		}
	}
	if len(repository.permissions) != 1 || repository.permissions[0] != "w" {
		t.Errorf("attached devices have to be read with the write permission, got %v", repository.permissions)
	}
	for _, token := range repository.tokens {
		if token != "Bearer t" {
			t.Errorf("the caller's token has to reach the repository, got %q", token)
		}
	}
}

// Moses publishes into an attached device with its own account, so a device the
// caller may read but not write, one shared with it for instance, is refused.
func TestAnAttachedDeviceTheCallerMayOnlyReadIsRefused(t *testing.T) {
	repository := newRepositoryDouble()
	catalog := checkingCatalog(t, repository)
	problems, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{
		{Path: "a0", DeviceTypeId: "dt-2", DeviceId: "urn:device:shared"},
		{Path: "a1", DeviceTypeId: "dt-2", DeviceId: "urn:device:picked"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || problems[0].Path != "a0.external_ref" {
		t.Fatalf("expected only the read-only device refused, got %+v", problems)
	}
	if !strings.Contains(problems[0].Message, "write") || !strings.Contains(problems[0].Message, "publishes into") {
		t.Errorf("the message has to say that writing it is required and why, got %q", problems[0].Message)
	}
}

// An id the repository normalises differently must not match by accident: the
// device-manager would get the id as the document spells it.
func TestTheCheckMatchesIdsExactly(t *testing.T) {
	repository := newRepositoryDouble()
	catalog := checkingCatalog(t, repository)
	problems, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{
		{Path: "a0", DeviceTypeId: " dt-1"},
		{Path: "a1", DeviceTypeId: "dt-1,dt-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := problemPaths(problems); strings.Join(got, " ") != "a0.external_type_id a1.external_type_id" {
		t.Fatalf("expected both ids reported as unknown, got %+v", problems)
	}
}

func TestEveryDistinctIdIsReadOnceInBoundedQueries(t *testing.T) {
	repository := newRepositoryDouble()
	catalog := checkingCatalog(t, repository)
	assets := []AssetReference{}
	for i := 0; i < 2*maxIdsPerQuery+10; i++ {
		//every type twice, and a device on every asset
		assets = append(assets,
			AssetReference{Path: fmt.Sprintf("a%d", i), DeviceTypeId: fmt.Sprintf("dt-%d", i%(maxIdsPerQuery+5))},
			AssetReference{Path: fmt.Sprintf("b%d", i), DeviceTypeId: "dt-2", DeviceId: fmt.Sprintf("urn:device:%d", i)})
	}
	if _, err := catalog.CheckReferences(context.Background(), "Bearer t", assets); err != nil {
		t.Fatal(err)
	}
	counted := func(queries [][]string) map[string]int {
		result := map[string]int{}
		for _, query := range queries {
			if len(query) > maxIdsPerQuery {
				t.Errorf("a query carries %d ids, the bound is %d", len(query), maxIdsPerQuery)
			}
			for _, id := range query {
				result[id]++
			}
		}
		return result
	}
	types := counted(repository.typeQueries)
	if len(types) != maxIdsPerQuery+5 || len(repository.typeQueries) != 2 {
		t.Errorf("expected %d distinct types in 2 queries, got %d in %d", maxIdsPerQuery+5, len(types), len(repository.typeQueries))
	}
	devices := counted(repository.deviceQueries)
	if len(devices) != 2*maxIdsPerQuery+10 || len(repository.deviceQueries) != 3 {
		t.Errorf("expected %d distinct devices in 3 queries, got %d in %d", 2*maxIdsPerQuery+10, len(devices), len(repository.deviceQueries))
	}
	for id, reads := range types {
		if reads != 1 {
			t.Errorf("the type %s was read %d times", id, reads)
		}
	}
	for id, reads := range devices {
		if reads != 1 {
			t.Errorf("the device %s was read %d times", id, reads)
		}
	}

	//and the protocol is resolved once for the life of the catalog
	if _, err := catalog.CheckReferences(context.Background(), "Bearer t", assets[:1]); err != nil {
		t.Fatal(err)
	}
	if repository.protocolReads != 1 {
		t.Errorf("expected the protocol to be read once, got %d", repository.protocolReads)
	}
}

// The ids travel in the url, and a gateway or proxy in front of the repository
// refuses a request line beyond a few KiB, commonly 4 or 8.
func TestAQueryStaysWithinAUrlAGatewayAccepts(t *testing.T) {
	repository := newRepositoryDouble()
	catalog := checkingCatalog(t, repository)
	assets := []AssetReference{}
	for i := 0; i < 200; i++ {
		assets = append(assets, AssetReference{
			Path:         fmt.Sprintf("a%d", i),
			DeviceTypeId: fmt.Sprintf("urn:infai:ses:device-type:%08d-0000-4000-8000-000000000000", i),
			DeviceId:     fmt.Sprintf("urn:infai:ses:device:%08d-0000-4000-8000-000000000000", i),
		})
	}
	if _, err := catalog.CheckReferences(context.Background(), "Bearer t", assets); err != nil {
		t.Fatal(err)
	}
	if repository.longestUri > 4096 {
		t.Errorf("a request line of %d bytes, beyond 4 KiB", repository.longestUri)
	}
}

// Nothing to read is no read at all, so a document without assets does not
// depend on the platform being there.
func TestACheckWithNothingToReadReadsNothing(t *testing.T) {
	repository := newRepositoryDouble()
	repository.status = http.StatusServiceUnavailable
	catalog := checkingCatalog(t, repository)
	//a blank type is Validate's to report, and no id the repository could hold
	problems, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{{Path: "a0"}, {Path: "a1", DeviceTypeId: "  "}})
	if err != nil || len(problems) != 0 {
		t.Fatalf("expected no problem and no error, got %+v, %v", problems, err)
	}
	if len(repository.tokens) != 0 {
		t.Errorf("expected no request, got %d", len(repository.tokens))
	}
}

// A repository that cannot answer is an error and never an empty answer: read as
// empty, every reference would be reported missing, or with a lenient reading
// none would.
func TestARepositoryThatCannotAnswerIsAnError(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusUnauthorized} {
		repository := newRepositoryDouble()
		repository.status = status
		catalog := checkingCatalog(t, repository)
		_, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{{Path: "a0", DeviceTypeId: "dt-1"}})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Errorf("%d: expected an error naming the status, got %v", status, err)
		}
	}

	//the protocol is cached, and it is the device read that fails
	repository := newRepositoryDouble()
	catalog := checkingCatalog(t, repository)
	if _, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{{Path: "a0", DeviceTypeId: "dt-1"}}); err != nil {
		t.Fatal(err)
	}
	repository.mux.Lock()
	repository.status = http.StatusBadGateway
	repository.mux.Unlock()
	if _, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{{Path: "a0", DeviceId: "urn:device:picked"}}); err == nil {
		t.Error("a failing device read has to be an error")
	}
}

func TestAnUnreadableAnswerOfTheRepositoryIsAnError(t *testing.T) {
	catalog := checkingCatalog(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("<html>gateway</html>"))
	}))
	if _, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{{Path: "a0", DeviceTypeId: "dt-1"}}); err == nil {
		t.Error("expected an error")
	}
}

func TestAMissingProtocolFailsTheCheck(t *testing.T) {
	repository := newRepositoryDouble()
	repository.protocols = []models.Protocol{{Id: "p-other", Handler: "mqtt"}}
	catalog := checkingCatalog(t, repository)
	if _, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{{Path: "a0", DeviceTypeId: "dt-1"}}); err == nil || !strings.Contains(err.Error(), "moses") {
		t.Errorf("expected an error naming the handler, got %v", err)
	}
}

// The registry client takes no context, which is why the check reads on its own:
// a repository that does not answer has to end the check with the deadline.
func TestAHangingRepositoryEndsTheCheckAtTheDeadline(t *testing.T) {
	repository := newRepositoryDouble()
	repository.hold = true
	catalog := checkingCatalog(t, repository)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := catalog.CheckReferences(ctx, "Bearer t", []AssetReference{{Path: "a0", DeviceTypeId: "dt-1"}})
	if err == nil {
		t.Fatal("expected the deadline to fail the check")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the check outlived its deadline by far: %v", elapsed)
	}
}

// Concurrent requests share the protocol cache; run with -race this pins that
// the cache is guarded.
func TestConcurrentChecksShareTheProtocolCache(t *testing.T) {
	repository := newRepositoryDouble()
	catalog := checkingCatalog(t, repository)
	wg := sync.WaitGroup{}
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := catalog.CheckReferences(context.Background(), "Bearer t", []AssetReference{{Path: "a0", DeviceTypeId: "dt-1"}}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := catalog.ProtocolId("Bearer t"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if id := catalog.cachedProtocolId(); id != "p-moses" {
		t.Errorf("expected the protocol cached, got %q", id)
	}
}
