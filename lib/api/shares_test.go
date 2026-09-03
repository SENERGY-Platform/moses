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
	"encoding/json"
	"errors"
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
	"github.com/SENERGY-Platform/moses/lib/repo"
	permModel "github.com/SENERGY-Platform/permissions-v2/pkg/model"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// eventLog records what happened in which order across the fakes, which is how
// the tests pin that the superset is stored BEFORE a right is written.
type eventLog struct {
	mux     sync.Mutex
	entries []string
}

func (this *eventLog) add(entry string) {
	if this == nil {
		return
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	this.entries = append(this.entries, entry)
}

func (this *eventLog) all() []string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]string{}, this.entries...)
}

// firstWith is the index of the first entry containing needle, or -1.
func (this *eventLog) firstWith(needle string) int {
	for index, entry := range this.all() {
		if strings.Contains(entry, needle) {
			return index
		}
	}
	return -1
}

// fakeShares is an in-memory repo.Shares with the store's compare-and-swap.
type fakeShares struct {
	mux     sync.Mutex
	stored  map[string]repo.ShareSet
	loadErr error
	saveErr error
	saves   []repo.ShareSet
	deletes []string
	log     *eventLog

	// saveErrOn fails the n-th Save (1 based), which is how a test gets the
	// rights written and the final set not stored.
	saveErrOn int
	saveCalls int

	// afterLoad and beforeSave let a test put two requests exactly into the
	// window the compare-and-swap exists for; onDelete is how it sees when the
	// leftover set went, relative to what the save was doing.
	afterLoad  func()
	beforeSave func()
	onDelete   func()
}

func newFakeShares() *fakeShares {
	return &fakeShares{stored: map[string]repo.ShareSet{}}
}

func (this *fakeShares) Load(ctx context.Context, environmentId string) (repo.ShareSet, error) {
	this.mux.Lock()
	if this.loadErr != nil {
		this.mux.Unlock()
		return repo.ShareSet{}, this.loadErr
	}
	stored, known := this.stored[environmentId]
	if !known {
		stored = repo.ShareSet{EnvironmentId: environmentId, Users: []string{}, Groups: []string{}}
	}
	this.log.add("shares.load " + environmentId)
	hook := this.afterLoad
	this.mux.Unlock()
	//outside the lock: the hook is what makes a second request run to here
	if hook != nil {
		hook()
	}
	return stored, nil
}

func (this *fakeShares) Save(ctx context.Context, shares repo.ShareSet) (int64, error) {
	this.mux.Lock()
	hook := this.beforeSave
	this.mux.Unlock()
	if hook != nil {
		hook()
	}

	this.mux.Lock()
	defer this.mux.Unlock()
	this.saveCalls++
	if this.saveErr != nil {
		return 0, this.saveErr
	}
	if this.saveErrOn == this.saveCalls {
		return 0, errors.New("mongodb unreachable")
	}
	stored, known := this.stored[shares.EnvironmentId]
	expected := shares.Version
	if (!known && expected != 0) || (known && stored.Version != expected) {
		return 0, &repo.VersionConflictError{
			Id: shares.EnvironmentId, Expected: expected, Stored: stored.Version, Gone: !known,
		}
	}
	shares.Version = expected + 1
	this.stored[shares.EnvironmentId] = shares
	this.saves = append(this.saves, shares)
	this.log.add(fmt.Sprintf("shares.save %v users=%v groups=%v", shares.EnvironmentId, shares.Users, shares.Groups))
	return shares.Version, nil
}

func (this *fakeShares) Delete(ctx context.Context, environmentId string) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	delete(this.stored, environmentId)
	this.deletes = append(this.deletes, environmentId)
	this.log.add("shares.delete " + environmentId)
	if this.onDelete != nil {
		this.onDelete()
	}
	return nil
}

func (this *fakeShares) set(environmentId string, users []string, groups []string) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.stored[environmentId] = repo.ShareSet{EnvironmentId: environmentId, Users: users, Groups: groups, Version: 1}
}

func (this *fakeShares) users(environmentId string) []string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.stored[environmentId].Users
}

func (this *fakeShares) groups(environmentId string) []string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.stored[environmentId].Groups
}

func (this *fakeShares) has(environmentId string) bool {
	this.mux.Lock()
	defer this.mux.Unlock()
	_, known := this.stored[environmentId]
	return known
}

// fakePermissions is an in-memory permissions-v2. It hands out copies and
// records every write, so a test can tell "the handler changed its own copy"
// from "the handler stored the change". Locked throughout: the handler works on
// the devices concurrently.
type fakePermissions struct {
	mux    sync.Mutex
	rights map[string]permModel.ResourcePermissions
	gets   []string
	sets   []string
	tokens []string
	topics []string

	// getErr and setErr fail single devices, which is what a share over thirty
	// devices has to survive without losing the ones it already wrote. getCode
	// and setCode say with which status, defaulting to 500.
	getErr  map[string]error
	setErr  map[string]error
	getCode map[string]int
	setCode map[string]int

	log *eventLog

	// forbiddenGroups models the platform's own rule: a caller without the admin
	// role may not share with a group they are not a member of, and
	// permissions-v2 refuses the write with 400.
	forbiddenGroups map[string]bool
}

func newFakePermissions(deviceIds ...string) *fakePermissions {
	result := &fakePermissions{
		rights:          map[string]permModel.ResourcePermissions{},
		getErr:          map[string]error{},
		setErr:          map[string]error{},
		getCode:         map[string]int{},
		setCode:         map[string]int{},
		forbiddenGroups: map[string]bool{},
	}
	for _, id := range deviceIds {
		result.own(id, "user-a")
	}
	return result
}

// the rights are keyed by topic AND id: a share that addressed the graph under
// the devices topic would otherwise look like it worked
func permKey(topicId string, id string) string {
	return topicId + "/" + id
}

// own puts a device under an owner, the way permissions-v2 holds a device
// somebody created.
func (this *fakePermissions) own(deviceId string, owner string) {
	this.ownResource(devicesTopic, deviceId, owner)
}

// ownGraph is the same for the graph an environment is mirrored as, which the
// device-repository registers under its own topic.
func (this *fakePermissions) ownGraph(graphId string, owner string) {
	this.ownResource(graphsTopic, graphId, owner)
}

func (this *fakePermissions) ownResource(topicId string, id string, owner string) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.rights[permKey(topicId, id)] = permModel.ResourcePermissions{
		UserPermissions: map[string]permModel.PermissionsMap{
			owner: {Read: true, Write: true, Execute: true, Administrate: true},
		},
	}
}

func (this *fakePermissions) GetResource(ctx context.Context, token string, topicId string, id string) (permModel.Resource, error, int) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.gets = append(this.gets, id)
	this.tokens = append(this.tokens, token)
	this.topics = append(this.topics, topicId)
	this.log.add("permissions.get " + id)
	if err := this.getErr[id]; err != nil {
		return permModel.Resource{}, err, this.codeOr(this.getCode[id])
	}
	rights, known := this.rights[permKey(topicId, id)]
	if !known {
		//a device permissions-v2 does not know about yet, which is what a device
		//created a moment ago looks like
		return permModel.Resource{}, errors.New("unknown resource " + id), http.StatusNotFound
	}
	return permModel.Resource{Id: id, TopicId: topicId, ResourcePermissions: copyRights(rights)}, nil, http.StatusOK
}

func (this *fakePermissions) SetPermission(ctx context.Context, token string, topicId string, id string, rights permModel.ResourcePermissions) (permModel.ResourcePermissions, error, int) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.tokens = append(this.tokens, token)
	this.topics = append(this.topics, topicId)
	this.log.add("permissions.set " + id)
	if err := this.setErr[id]; err != nil {
		return permModel.ResourcePermissions{}, err, this.codeOr(this.setCode[id])
	}
	for group := range rights.GroupPermissions {
		if this.forbiddenGroups[group] {
			return permModel.ResourcePermissions{},
				fmt.Errorf("permissions-v2 answered 400: user may not share with group %v", group), http.StatusBadRequest
		}
	}
	this.sets = append(this.sets, id)
	this.rights[permKey(topicId, id)] = copyRights(rights)
	return rights, nil, http.StatusOK
}

// codeOr defaults a seeded failure to 500, which is what an unreachable
// permissions-v2 looks like.
func (this *fakePermissions) codeOr(code int) int {
	if code == 0 {
		return http.StatusInternalServerError
	}
	return code
}

func copyRights(in permModel.ResourcePermissions) permModel.ResourcePermissions {
	out := permModel.ResourcePermissions{}
	for _, pair := range []struct {
		from map[string]permModel.PermissionsMap
		to   *map[string]permModel.PermissionsMap
	}{
		{in.UserPermissions, &out.UserPermissions},
		{in.GroupPermissions, &out.GroupPermissions},
		{in.RolePermissions, &out.RolePermissions},
	} {
		if pair.from == nil {
			continue
		}
		copied := map[string]permModel.PermissionsMap{}
		for key, value := range pair.from {
			copied[key] = value
		}
		*pair.to = copied
	}
	return out
}

func (this *fakePermissions) userRights(deviceId string, user string) permModel.PermissionsMap {
	return this.resourceUserRights(devicesTopic, deviceId, user)
}

func (this *fakePermissions) graphUserRights(graphId string, user string) permModel.PermissionsMap {
	return this.resourceUserRights(graphsTopic, graphId, user)
}

func (this *fakePermissions) resourceUserRights(topicId string, id string, user string) permModel.PermissionsMap {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.rights[permKey(topicId, id)].UserPermissions[user]
}

func (this *fakePermissions) groupRights(deviceId string, group string) permModel.PermissionsMap {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.rights[permKey(devicesTopic, deviceId)].GroupPermissions[group]
}

func (this *fakePermissions) graphGroupRights(graphId string, group string) permModel.PermissionsMap {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.rights[permKey(graphsTopic, graphId)].GroupPermissions[group]
}

func (this *fakePermissions) knowsUser(deviceId string, user string) bool {
	this.mux.Lock()
	defer this.mux.Unlock()
	_, known := this.rights[permKey(devicesTopic, deviceId)].UserPermissions[user]
	return known
}

func (this *fakePermissions) graphKnowsUser(graphId string, user string) bool {
	this.mux.Lock()
	defer this.mux.Unlock()
	_, known := this.rights[permKey(graphsTopic, graphId)].UserPermissions[user]
	return known
}

func (this *fakePermissions) knowsGroup(deviceId string, group string) bool {
	this.mux.Lock()
	defer this.mux.Unlock()
	_, known := this.rights[permKey(devicesTopic, deviceId)].GroupPermissions[group]
	return known
}

func (this *fakePermissions) userEntries(deviceId string) int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return len(this.rights[permKey(devicesTopic, deviceId)].UserPermissions)
}

func (this *fakePermissions) sawTopic(topicId string) bool {
	this.mux.Lock()
	defer this.mux.Unlock()
	for _, seen := range this.topics {
		if seen == topicId {
			return true
		}
	}
	return false
}

func (this *fakePermissions) setsOf(deviceId string) int {
	this.mux.Lock()
	defer this.mux.Unlock()
	count := 0
	for _, id := range this.sets {
		if id == deviceId {
			count++
		}
	}
	return count
}

func (this *fakePermissions) calls() (gets []string, sets []string, tokens []string) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]string{}, this.gets...), append([]string{}, this.sets...), append([]string{}, this.tokens...)
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func assetWithDevice(id string, name string, deviceId string, managed bool) domain.Asset {
	return domain.Asset{
		Id: id, Name: name, Kind: domain.AssetMeter,
		ExternalTypeId:  "urn:infai:ses:device-type:abc",
		ExternalRef:     deviceId,
		ExternalManaged: managed,
		Channels: []domain.Channel{{
			Name: "Wirkenergie", Direction: domain.Sensor, Unit: "kWh", IntervalSeconds: 30,
			Source: domain.Source{Kind: domain.SourceProfile, Profile: &domain.ProfileSource{Base: 1}},
		}},
	}
}

// sharedEnvironment carries two devices moses created and one the user attached,
// which is the difference every share test turns on.
func sharedEnvironment() domain.Environment {
	return domain.Environment{
		Id: "env-1", Owner: "user-a", Version: 1,
		Name: "Metallbau Musterstadt", Type: domain.IndustrialSite,
		Zones: []domain.Zone{{
			Id: "zone-1", Name: "Halle 1", Type: domain.ZoneHall,
			Assets: []domain.Asset{
				assetWithDevice("asset-1", "Hauptzähler", "dev-1", true),
				assetWithDevice("asset-2", "Kompressor", "dev-2", true),
				assetWithDevice("asset-3", "Fremdgerät", "dev-3", false),
			},
		}},
	}
}

func newMachine(id string, name string) domain.Asset {
	return domain.Asset{
		Id: id, Name: name, Kind: domain.AssetMachine,
		ExternalTypeId: "urn:infai:ses:device-type:abc",
		Channels: []domain.Channel{{
			Name: "Strom", Direction: domain.Sensor, IntervalSeconds: 30,
			Source: domain.Source{Kind: domain.SourceProfile, Profile: &domain.ProfileSource{Base: 1}},
		}},
	}
}

func storeWithSharedEnvironment() *fakeEnvironments {
	store := newFakeEnvironments()
	store.stored["env-1"] = sharedEnvironment()
	return store
}

func sharesResponseOf(t *testing.T, body []byte) SharesResponse {
	t.Helper()
	result := SharesResponse{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unable to read the answer: %v: %s", err, string(body))
	}
	return result
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// doAsAdmin in environment_test.go carries no body; a share is a body.
func doWithAdminToken(t *testing.T, router *gin.Engine, method string, path string, userId string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Authorization", adminTokenFor(userId))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// ---------------------------------------------------------------------------
// Granting
// ---------------------------------------------------------------------------

func TestSharingSetsReadAndExecuteOnEveryManagedDeviceAndLeavesAnAttachedOneAlone(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2", "dev-3")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}, Groups: []string{"/demo"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	answer := sharesResponseOf(t, resp.Body.Bytes())
	if answer.Devices != 2 {
		t.Errorf("the answer has to name the number of devices the share acts on, got %d", answer.Devices)
	}

	for _, device := range []string{"dev-1", "dev-2"} {
		if rights := permissions.userRights(device, "demo-user"); !rights.Read || !rights.Execute {
			t.Errorf("%s: expected read and execute for the shared user, got %+v", device, rights)
		}
		if rights := permissions.groupRights(device, "/demo"); !rights.Read || !rights.Execute {
			t.Errorf("%s: expected read and execute for the shared group, got %+v", device, rights)
		}
		//the whole rights object is replaced on write, so the owner's entry has
		//to survive the read-modify-write or the share locks them out
		if rights := permissions.userRights(device, "user-a"); !rights.Administrate || !rights.Write {
			t.Errorf("%s: the owner's rights must not be narrowed, got %+v", device, rights)
		}
		if rights := permissions.userRights(device, "demo-user"); rights.Write || rights.Administrate {
			t.Errorf("%s: a share grants read and execute only, got %+v", device, rights)
		}
	}
	//a device the user attached is not ours to share
	if permissions.knowsUser("dev-3", "demo-user") || permissions.setsOf("dev-3") != 0 {
		t.Error("an attached device must not be touched")
	}

	if got := shares.users("env-1"); len(got) != 1 || got[0] != "demo-user" {
		t.Errorf("the set has to be stored, got %v", got)
	}
	if got := shares.groups("env-1"); len(got) != 1 || got[0] != "/demo" {
		t.Errorf("the groups have to be stored, got %v", got)
	}
	//the set lives beside the document, not in it
	if stored := store.stored["env-1"]; stored.Version != 1 {
		t.Errorf("a share must not write the document, version is %d", stored.Version)
	}
}

// The rights are granted with the caller's own credential; permissions-v2
// decides from it what they may hand out. A service token here would let anybody
// with an environment share anything.
func TestTheCallersOwnTokenIsWhatReachesPermissions(t *testing.T) {
	store := storeWithSharedEnvironment()
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}}).Code; code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	_, _, tokens := permissions.calls()
	if len(tokens) != 4 {
		t.Fatalf("expected a read and a write per device, got %d calls", len(tokens))
	}
	for _, token := range tokens {
		if token != tokenFor("user-a") {
			t.Errorf("expected exactly the caller's token, got %q", token)
		}
	}
}

func TestSharingKeepsAWriteAnEntryAlreadyCarried(t *testing.T) {
	store := storeWithSharedEnvironment()
	permissions := newFakePermissions("dev-1", "dev-2")
	rights := permissions.rights[permKey(devicesTopic, "dev-1")]
	rights.UserPermissions["demo-user"] = permModel.PermissionsMap{Write: true}
	permissions.rights[permKey(devicesTopic, "dev-1")] = rights
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}}).Code; code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if got := permissions.userRights("dev-1", "demo-user"); !got.Read || !got.Execute || !got.Write {
		t.Errorf("a share adds rights and takes none away, got %+v", got)
	}
}

func TestSharingTheSameSetTwiceChangesNothing(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)
	targets := ShareTargets{Users: []string{"demo-user"}, Groups: []string{"/demo"}}

	first := do(t, router, "PUT", "/environments/env-1/shares", "user-a", targets)
	second := do(t, router, "PUT", "/environments/env-1/shares", "user-a", targets)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("expected both to be accepted, got %d and %d", first.Code, second.Code)
	}
	if got := permissions.userEntries("dev-1"); got != 2 {
		t.Errorf("expected the owner and the shared user, got %d entries", got)
	}
	if got := shares.users("env-1"); len(got) != 1 {
		t.Errorf("a repeat must not double the set, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Withdrawing
// ---------------------------------------------------------------------------

func TestWithdrawingAShareRemovesTheEntryOfThatPrincipalOnly(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user", "other-user"}, []string{"/demo", "/other"})

	permissions := newFakePermissions("dev-1", "dev-2")
	for _, device := range []string{"dev-1", "dev-2"} {
		rights := permissions.rights[permKey(devicesTopic, device)]
		rights.UserPermissions["demo-user"] = permModel.PermissionsMap{Read: true, Execute: true}
		rights.UserPermissions["other-user"] = permModel.PermissionsMap{Read: true, Execute: true}
		rights.GroupPermissions = map[string]permModel.PermissionsMap{
			"/demo":  {Read: true, Execute: true},
			"/other": {Read: true, Execute: true},
		}
		permissions.rights[permKey(devicesTopic, device)] = rights
	}
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}, Groups: []string{"/demo"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	for _, device := range []string{"dev-1", "dev-2"} {
		if permissions.knowsUser(device, "other-user") {
			t.Errorf("%s: the user the set no longer names has to lose the entry", device)
		}
		if permissions.knowsGroup(device, "/other") {
			t.Errorf("%s: the group the set no longer names has to lose the entry", device)
		}
		if rights := permissions.userRights(device, "demo-user"); !rights.Read || !rights.Execute {
			t.Errorf("%s: the user the set still names keeps the rights, got %+v", device, rights)
		}
		if rights := permissions.userRights(device, "user-a"); !rights.Administrate {
			t.Errorf("%s: the owner must not be touched, got %+v", device, rights)
		}
	}
	if got := shares.users("env-1"); len(got) != 1 || got[0] != "demo-user" {
		t.Errorf("the withdrawn user has to be gone from the stored set, got %v", got)
	}
	if got := shares.groups("env-1"); len(got) != 1 || got[0] != "/demo" {
		t.Errorf("the withdrawn group has to be gone from the stored set, got %v", got)
	}
}

// The owner and the platform administrators reach a device through their own
// administrate entry. permissions-v2 refuses a rights object without an
// administrating USER, so a withdrawal that removed one could make the device
// unwritable - and it would take the device away from the account that owns it.
func TestWithdrawingAShareNeverRemovesAnAdministrator(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.set("env-1", []string{"admin-user"}, []string{"/admins"})

	permissions := newFakePermissions("dev-1", "dev-2")
	for _, device := range []string{"dev-1", "dev-2"} {
		rights := permissions.rights[permKey(devicesTopic, device)]
		rights.UserPermissions["admin-user"] = permModel.PermissionsMap{Read: true, Execute: true, Administrate: true}
		rights.GroupPermissions = map[string]permModel.PermissionsMap{
			"/admins": {Read: true, Write: true, Execute: true, Administrate: true},
		}
		permissions.rights[permKey(devicesTopic, device)] = rights
	}
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	for _, device := range []string{"dev-1", "dev-2"} {
		if rights := permissions.userRights(device, "admin-user"); !rights.Administrate || !rights.Read {
			t.Errorf("%s: an entry carrying administrate must survive a withdrawal, got %+v", device, rights)
		}
		if rights := permissions.groupRights(device, "/admins"); !rights.Administrate {
			t.Errorf("%s: an administrating group must survive a withdrawal, got %+v", device, rights)
		}
	}
}

// ---------------------------------------------------------------------------
// Failures
// ---------------------------------------------------------------------------

// The set that is stored is what the next call computes its withdrawals from.
// If it were written only on success, the grants a failed call did manage to
// make would be remembered nowhere and could never be taken back.
func TestAFailedShareLeavesTheSupersetStoredSoItCanBeWithdrawn(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.getErr["dev-2"] = errors.New("permissions-v2 unreachable")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	failures := ShareFailures{}
	if err := json.Unmarshal(resp.Body.Bytes(), &failures); err != nil {
		t.Fatal(err)
	}
	if len(failures.Devices) != 1 || failures.Devices[0].Id != "dev-2" || failures.Devices[0].Error == "" {
		t.Fatalf("the answer has to name the device and the reason, got %+v", failures.Devices)
	}
	//dev-1 went through, and the stored set is what remembers it
	if !permissions.userRights("dev-1", "demo-user").Read {
		t.Fatal("expected the device that did not fail to be shared")
	}
	if got := shares.users("env-1"); len(got) != 1 || got[0] != "demo-user" {
		t.Fatalf("the superset has to be stored before anything is granted, got %v", got)
	}

	//and now the withdrawal, which has to reach the stray grant
	permissions.getErr = map[string]error{}
	empty := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{})
	if empty.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", empty.Code, empty.Body.String())
	}
	for _, device := range []string{"dev-1", "dev-2"} {
		if permissions.knowsUser(device, "demo-user") {
			t.Errorf("%s: the stray grant of the failed share has to be withdrawn", device)
		}
	}
	if got := shares.users("env-1"); len(got) != 0 {
		t.Errorf("the empty set has to be stored, got %v", got)
	}
}

func TestARepeatAfterAFailureReachesTheDeviceThatFailed(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.getErr["dev-2"] = errors.New("permissions-v2 unreachable")
	router := testRouterWithShares(store, shares, nil, permissions)
	targets := ShareTargets{Users: []string{"demo-user"}}

	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a", targets).Code; code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", code)
	}
	permissions.getErr = map[string]error{}
	again := do(t, router, "PUT", "/environments/env-1/shares", "user-a", targets)
	if again.Code != http.StatusOK {
		t.Fatalf("expected the repeat to succeed, got %d: %s", again.Code, again.Body.String())
	}
	for _, device := range []string{"dev-1", "dev-2"} {
		if rights := permissions.userRights(device, "demo-user"); !rights.Read || !rights.Execute {
			t.Errorf("%s: the repeat has to reach every device, got %+v", device, rights)
		}
		if got := permissions.userEntries(device); got != 2 {
			t.Errorf("%s: expected the owner and the shared user, got %d entries", device, got)
		}
	}
}

func TestAFailingWriteOfTheRightsIsReportedAsWell(t *testing.T) {
	store := storeWithSharedEnvironment()
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.setErr["dev-1"] = errors.New("no administrate right on this device")
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "dev-1") {
		t.Errorf("the failing device has to be named, got %s", resp.Body.String())
	}
}

// The platform decides who may be named, not moses: a caller without the admin
// role may share with groups they are a member of and with users who share a
// group with them. The refusal has to arrive readably rather than as a bare 502.
func TestAGroupTheCallerMayNotShareWithComesBackAsTheDevicesReason(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.forbiddenGroups["/strangers"] = true
	router := testRouterWithShares(store, shares, nil, permissions)

	//every failure of this call is the caller naming something they may not:
	//that is their request being wrong, not the platform being broken
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Groups: []string{"/strangers"}})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "/strangers") || !strings.Contains(body, "400") {
		t.Errorf("the answer has to carry the platform's reason, got %s", body)
	}
	if len(shares.groups("env-1")) != 1 {
		t.Errorf("the attempted group stays recorded until it is withdrawn, got %v", shares.groups("env-1"))
	}
}

func TestSharingAnEnvironmentWithoutAPermissionsClientIsAnError(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	router := testRouterWithShares(store, shares, nil, nil)
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", resp.Code, resp.Body.String())
	}
	if shares.has("env-1") {
		t.Error("a set that was never granted must not be stored")
	}
}

func TestAFailingShareStoreIsAnErrorAndGrantsNothing(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.saveErr = errors.New("mongodb unreachable")
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", resp.Code, resp.Body.String())
	}
	gets, sets, _ := permissions.calls()
	if len(gets) != 0 || len(sets) != 0 {
		t.Errorf("nothing may be granted that cannot be recorded, got %v/%v", gets, sets)
	}
}

// ---------------------------------------------------------------------------
// Reading, access and validation
// ---------------------------------------------------------------------------

func TestGetServesTheStoredSetAndTheNumberOfDevicesItActsOn(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	router := testRouterWithShares(store, shares, nil, newFakePermissions("dev-1", "dev-2"))

	resp := do(t, router, "GET", "/environments/env-1/shares", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	//an empty set is [] and not null, or a client has two cases for "nobody"
	if body := resp.Body.String(); !strings.Contains(body, `"users":[]`) || !strings.Contains(body, `"groups":[]`) {
		t.Errorf("expected empty lists, got %s", body)
	}
	if answer := sharesResponseOf(t, resp.Body.Bytes()); answer.Devices != 2 {
		t.Errorf("expected the two managed devices, got %d", answer.Devices)
	}

	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}}).Code; code != http.StatusOK {
		t.Fatalf("expected the share to be accepted, got %d", code)
	}
	answer := sharesResponseOf(t, do(t, router, "GET", "/environments/env-1/shares", "user-a", nil).Body.Bytes())
	if len(answer.Users) != 1 || answer.Users[0] != "demo-user" {
		t.Errorf("expected the stored set to be served, got %+v", answer)
	}
}

func TestSharesOfAForeignEnvironmentAreNotFound(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)

	if code := do(t, router, "GET", "/environments/env-1/shares", "user-b", nil).Code; code != http.StatusNotFound {
		t.Errorf("expected 404 on read, got %d", code)
	}
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-b",
		ShareTargets{Users: []string{"user-b"}})
	if resp.Code != http.StatusNotFound {
		t.Errorf("expected 404 on write, got %d: %s", resp.Code, resp.Body.String())
	}
	gets, sets, _ := permissions.calls()
	if len(gets) != 0 || len(sets) != 0 {
		t.Errorf("a caller without access must not reach permissions-v2 at all, got %v/%v", gets, sets)
	}
	if shares.has("env-1") {
		t.Error("a caller without access must not change the set")
	}
}

func TestAnAdminSharesTheDevicesOfAForeignEnvironment(t *testing.T) {
	store := storeWithSharedEnvironment()
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	resp := doWithAdminToken(t, router, "PUT", "/environments/env-1/shares", "admin-user",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if rights := permissions.userRights("dev-1", "demo-user"); !rights.Read {
		t.Errorf("an admin may share, got %+v", rights)
	}
	_, _, tokens := permissions.calls()
	for _, token := range tokens {
		if token != adminTokenFor("admin-user") {
			t.Errorf("expected exactly the admin's own token, got %q", token)
		}
	}
}

func TestTheShareSetIsValidated(t *testing.T) {
	for name, targets := range map[string]ShareTargets{
		"an empty user id":         {Users: []string{"demo-user", ""}},
		"a blank user id":          {Users: []string{"  "}},
		"a group without a path":   {Groups: []string{"demo"}},
		"the root as a group":      {Groups: []string{"/"}},
		"an overlong user id":      {Users: []string{strings.Repeat("u", maxPrincipalRunes+1)}},
		"an overlong group path":   {Groups: []string{"/" + strings.Repeat("g", maxPrincipalRunes)}},
		"more than the set allows": {Users: manyPrincipals(maxShares + 1)},
	} {
		store := storeWithSharedEnvironment()
		shares := newFakeShares()
		permissions := newFakePermissions("dev-1", "dev-2")
		resp := do(t, testRouterWithShares(store, shares, nil, permissions), "PUT", "/environments/env-1/shares", "user-a", targets)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, resp.Code, resp.Body.String())
		}
		_, sets, _ := permissions.calls()
		if len(sets) != 0 || shares.has("env-1") {
			t.Errorf("%s: a refused set must change nothing", name)
		}
	}
}

func TestABodyBeyondTheLimitIsRefused(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	router := testRouterWithShares(store, shares, nil, newFakePermissions("dev-1", "dev-2"))

	huge := `{"users":["` + strings.Repeat("u", maxShareBytes+1) + `"]}`
	request := httptest.NewRequest("PUT", "/environments/env-1/shares", strings.NewReader(huge))
	request.Header.Set("Authorization", tokenFor("user-a"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if shares.has("env-1") {
		t.Error("a refused body must change nothing")
	}
}

func TestAShareSetAtTheLimitIsAccepted(t *testing.T) {
	store := storeWithSharedEnvironment()
	router := testRouterWithShares(store, newFakeShares(), nil, newFakePermissions("dev-1", "dev-2"))
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{Users: manyPrincipals(maxShares)})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 at the limit, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestDuplicatesInTheShareSetAreRemoved(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	router := testRouterWithShares(store, shares, nil, newFakePermissions("dev-1", "dev-2"))

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{
		Users:  []string{"demo-user", "demo-user", " demo-user "},
		Groups: []string{"/demo", "/demo"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := shares.users("env-1"); len(got) != 1 || got[0] != "demo-user" {
		t.Errorf("expected the users to be deduplicated, got %v", got)
	}
	if got := shares.groups("env-1"); len(got) != 1 {
		t.Errorf("expected the groups to be deduplicated, got %v", got)
	}
}

func manyPrincipals(count int) []string {
	result := make([]string, 0, count)
	for i := 0; i < count; i++ {
		result = append(result, fmt.Sprintf("user-%d", i))
	}
	return result
}

// A share of thirty devices is sixty round trips; they have to overlap or the
// call runs into the api's write timeout.
func TestEveryDeviceIsReachedWhenThereAreMoreOfThemThanWorkers(t *testing.T) {
	env := sharedEnvironment()
	deviceIds := []string{}
	for i := 0; i < shareWorkers*4; i++ {
		id := fmt.Sprintf("dev-many-%d", i)
		deviceIds = append(deviceIds, id)
		env.Zones[0].Assets = append(env.Zones[0].Assets, assetWithDevice(fmt.Sprintf("asset-many-%d", i), id, id, true))
	}
	store := newFakeEnvironments()
	store.stored["env-1"] = env
	permissions := newFakePermissions(append(deviceIds, "dev-1", "dev-2")...)
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if answer := sharesResponseOf(t, resp.Body.Bytes()); answer.Devices != len(deviceIds)+2 {
		t.Errorf("expected every managed device to be counted, got %d", answer.Devices)
	}
	for _, id := range deviceIds {
		if !permissions.userRights(id, "demo-user").Read {
			t.Fatalf("%s was not reached", id)
		}
	}
}

// The report is read by a human, so it must not depend on which worker finished
// first.
func TestTheFailureReportKeepsDocumentOrder(t *testing.T) {
	env := sharedEnvironment()
	deviceIds := []string{}
	for i := 0; i < shareWorkers*2; i++ {
		id := fmt.Sprintf("dev-many-%d", i)
		deviceIds = append(deviceIds, id)
		env.Zones[0].Assets = append(env.Zones[0].Assets, assetWithDevice(fmt.Sprintf("asset-many-%d", i), id, id, true))
	}
	store := newFakeEnvironments()
	store.stored["env-1"] = env
	permissions := newFakePermissions()
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	//no device is known to permissions-v2, so every one of them fails
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.Code)
	}
	failures := ShareFailures{}
	if err := json.Unmarshal(resp.Body.Bytes(), &failures); err != nil {
		t.Fatal(err)
	}
	expected := append([]string{"dev-1", "dev-2"}, deviceIds...)
	if len(failures.Devices) != len(expected) {
		t.Fatalf("expected every device to be reported, got %d", len(failures.Devices))
	}
	for i, failure := range failures.Devices {
		if failure.Id != expected[i] {
			t.Fatalf("expected the report in document order, got %v at %d", failure.Id, i)
		}
	}
}

// ---------------------------------------------------------------------------
// The document
// ---------------------------------------------------------------------------

// The set is not part of the document, so a document write can neither claim nor
// lose one.
func TestADocumentWriteNeitherClaimsNorLosesTheShareSet(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, []string{"/demo"})
	router := testRouterWithShares(store, shares, nil, newFakePermissions("dev-1", "dev-2"))

	sent := map[string]interface{}{}
	exported := do(t, router, "GET", "/environments/env-1", "user-a", nil)
	if err := json.Unmarshal(exported.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	if _, carried := sent["shares"]; carried {
		t.Error("the document must not carry the set at all")
	}
	//a claimed set in the body changes nothing either
	sent["shares"] = map[string]interface{}{"users": []string{"intruder"}, "groups": []string{"/intruders"}}
	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", sent); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := shares.users("env-1"); len(got) != 1 || got[0] != "demo-user" {
		t.Errorf("a document write must not change the set, got %v", got)
	}
	if got := shares.groups("env-1"); len(got) != 1 || got[0] != "/demo" {
		t.Errorf("a document write must not change the groups, got %v", got)
	}
}

// An id that is used again must not come back shared with the accounts of the
// environment that is gone.
func TestAnIdThatIsUsedAgainStartsUnshared(t *testing.T) {
	store := newFakeEnvironments()
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, nil)
	router := testRouterWithShares(store, shares, nil, newFakePermissions())

	//nothing is stored under env-1, so this put creates it
	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", minimalEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if shares.has("env-1") {
		t.Errorf("a leftover set must be dropped, got %v", shares.users("env-1"))
	}

	//and a create with a server assigned id does the same
	shares.set("env-2", []string{"demo-user"}, nil)
	created := domain.Environment{}
	resp := do(t, router, "POST", "/environments", "user-a", minimalEnvironment())
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(shares.deletes) < 2 || shares.deletes[len(shares.deletes)-1] != created.Id {
		t.Errorf("a created environment has to drop what its id may have carried, got %v", shares.deletes)
	}
}

func TestDeletingAnEnvironmentDeletesItsShareSet(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, nil)
	router := testRouterWithShares(store, shares, nil, newFakePermissions("dev-1", "dev-2"))

	if code := do(t, router, "DELETE", "/environments/env-1", "user-a", nil).Code; code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", code)
	}
	if shares.has("env-1") {
		t.Error("the set has to go with the environment")
	}
}

// A share that had to be renewed after every edit would be forgotten, and the
// asset added last is the one nobody sees.
func TestADeviceCreatedByASaveInheritsTheShareSetOfItsEnvironment(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, []string{"/demo"})

	permissions := newFakePermissions("dev-1", "dev-2", "dev-3", "urn:device:new")
	catalog := &fakeCatalog{}
	router := testRouterWithShares(store, shares, catalog, permissions)

	sent := sharedEnvironment()
	sent.Zones[0].Assets = append(sent.Zones[0].Assets, newMachine("asset-4", "Neue Maschine"))
	resp := do(t, router, "PUT", "/environments/env-1", "user-a", sent)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if len(catalog.created) != 1 {
		t.Fatalf("expected one device for the new asset, got %+v", catalog.created)
	}
	if rights := permissions.userRights("urn:device:new", "demo-user"); !rights.Read || !rights.Execute {
		t.Errorf("the new device has to inherit the set, got %+v", rights)
	}
	if rights := permissions.groupRights("urn:device:new", "/demo"); !rights.Read || !rights.Execute {
		t.Errorf("the new device has to inherit the groups too, got %+v", rights)
	}
	//only the new device: rewriting the rights of every device on every save
	//would make an edit as expensive as a share
	if permissions.setsOf("dev-1") != 0 || permissions.setsOf("dev-2") != 0 {
		t.Error("a save must only touch the devices it created")
	}
}

// The rights of a freshly created device reach permissions-v2 asynchronously, so
// this fails in normal operation. Failing the save over it would leave the user
// with an asset they cannot store.
func TestAFailingInheritanceDoesNotFailTheSave(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, nil)

	//urn:device:new is deliberately unknown to permissions-v2
	permissions := newFakePermissions("dev-1", "dev-2")
	catalog := &fakeCatalog{}
	router := testRouterWithShares(store, shares, catalog, permissions)

	sent := sharedEnvironment()
	sent.Zones[0].Assets = append(sent.Zones[0].Assets, newMachine("asset-4", "Neue Maschine"))
	resp := do(t, router, "PUT", "/environments/env-1", "user-a", sent)
	if resp.Code != http.StatusOK {
		t.Fatalf("the save must not fail over a share, got %d: %s", resp.Code, resp.Body.String())
	}
	if stored := store.stored["env-1"]; len(stored.Zones[0].Assets) != 4 {
		t.Errorf("the document has to be stored, got %d assets", len(stored.Zones[0].Assets))
	}
	if got := sortedCopy(shares.users("env-1")); len(got) != 1 || got[0] != "demo-user" {
		t.Errorf("a failed inheritance must not change the set, got %v", got)
	}
}

// An environment nobody shared must not call permissions-v2 on every save.
func TestASaveWithoutAShareTouchesNoPermissions(t *testing.T) {
	store := storeWithSharedEnvironment()
	permissions := newFakePermissions("dev-1", "dev-2", "urn:device:new")
	catalog := &fakeCatalog{}
	router := testRouterWithShares(store, newFakeShares(), catalog, permissions)

	sent := sharedEnvironment()
	sent.Zones[0].Assets = append(sent.Zones[0].Assets, newMachine("asset-4", "Neue Maschine"))
	if code := do(t, router, "PUT", "/environments/env-1", "user-a", sent).Code; code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	gets, sets, _ := permissions.calls()
	if len(gets) != 0 || len(sets) != 0 {
		t.Errorf("an unshared environment must not talk to permissions-v2, got %v/%v", gets, sets)
	}
}

// ---------------------------------------------------------------------------
// The graph
// ---------------------------------------------------------------------------

// The graph is what an application reads a simulated site through, so a share
// that stopped at the devices would hand out the readings and hide the
// structure.
func sharedEnvironmentWithGraph() domain.Environment {
	env := sharedEnvironment()
	env.ExternalGraphRef = "urn:infai:ses:graph:1"
	return env
}

func storeWithGraphEnvironment() *fakeEnvironments {
	store := newFakeEnvironments()
	store.stored["env-1"] = sharedEnvironmentWithGraph()
	return store
}

func TestSharingReachesTheGraphOfTheEnvironment(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.ownGraph("urn:infai:ses:graph:1", "user-a")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}, Groups: []string{"/demo"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if answer := sharesResponseOf(t, resp.Body.Bytes()); !answer.Graph || answer.Devices != 2 {
		t.Errorf("the answer has to say that the graph is shared too, got %+v", answer)
	}
	if rights := permissions.graphUserRights("urn:infai:ses:graph:1", "demo-user"); !rights.Read || !rights.Execute {
		t.Errorf("expected read and execute on the graph, got %+v", rights)
	}
	if rights := permissions.graphGroupRights("urn:infai:ses:graph:1", "/demo"); !rights.Read || !rights.Execute {
		t.Errorf("expected read and execute on the graph for the group, got %+v", rights)
	}
	//the owner's entry survives the read-modify-write here as well
	if rights := permissions.graphUserRights("urn:infai:ses:graph:1", "user-a"); !rights.Administrate {
		t.Errorf("the owner of the graph must not be narrowed, got %+v", rights)
	}

	//and the withdrawal reaches it
	empty := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{})
	if empty.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", empty.Code, empty.Body.String())
	}
	if permissions.graphKnowsUser("urn:infai:ses:graph:1", "demo-user") {
		t.Error("the graph has to lose the entry with the devices")
	}
}

// An environment whose mirror never succeeded has no ref, and a share must not
// invent a resource for it.
func TestAnEnvironmentWithoutAGraphDoesNotTouchTheGraphTopic(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if answer := sharesResponseOf(t, resp.Body.Bytes()); answer.Graph {
		t.Error("an environment without a graph must not claim one is shared")
	}
	if permissions.sawTopic(graphsTopic) {
		t.Error("no graph, no call to the graph topic")
	}
	answer := sharesResponseOf(t, do(t, router, "GET", "/environments/env-1/shares", "user-a", nil).Body.Bytes())
	if answer.Graph {
		t.Error("the read has to say the same")
	}
}

// A graph the caller may not share is a different fix from a device that failed,
// so the report says which of the two it was.
func TestAFailingGraphIsReportedAsAGraph(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	//the graph is deliberately unknown to permissions-v2, the devices are not
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	failures := ShareFailures{}
	if err := json.Unmarshal(resp.Body.Bytes(), &failures); err != nil {
		t.Fatal(err)
	}
	if len(failures.Devices) != 1 {
		t.Fatalf("expected only the graph to fail, got %+v", failures.Devices)
	}
	if failures.Devices[0].Kind != shareKindGraph || failures.Devices[0].Id != "urn:infai:ses:graph:1" {
		t.Errorf("expected the graph to be named as one, got %+v", failures.Devices[0])
	}
	//and the devices that went through are recorded, so the withdrawal reaches them
	if got := shares.users("env-1"); len(got) != 1 {
		t.Errorf("the superset has to stand, got %v", got)
	}
}

func TestAFailingDeviceIsReportedAsADevice(t *testing.T) {
	store := storeWithSharedEnvironment()
	permissions := newFakePermissions("dev-1")
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	failures := ShareFailures{}
	if err := json.Unmarshal(resp.Body.Bytes(), &failures); err != nil {
		t.Fatal(err)
	}
	if len(failures.Devices) != 1 || failures.Devices[0].Kind != shareKindDevice {
		t.Fatalf("expected the device to be named as one, got %+v", failures.Devices)
	}
}

// A graph that is created by a save has no rights from the share that was made
// before it existed.
func TestAGraphCreatedByASaveInheritsTheShareSet(t *testing.T) {
	store := storeWithSharedEnvironment() //no graph ref yet
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, []string{"/demo"})
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.ownGraph("urn:infai:ses:graph:1", "user-a")
	mirror := newFakeGraphMirror()
	router := testRouterWithAll(store, shares, nil, mirror, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1", "user-a", sharedEnvironment())
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	stored := store.stored["env-1"]
	if stored.ExternalGraphRef != "urn:infai:ses:graph:1" {
		t.Fatalf("expected the save to create a graph, got %q", stored.ExternalGraphRef)
	}
	if rights := permissions.graphUserRights("urn:infai:ses:graph:1", "demo-user"); !rights.Read || !rights.Execute {
		t.Errorf("the new graph has to inherit the set, got %+v", rights)
	}
	if rights := permissions.graphGroupRights("urn:infai:ses:graph:1", "/demo"); !rights.Read {
		t.Errorf("the new graph has to inherit the groups too, got %+v", rights)
	}
}

// The graph an environment already had carries the set since it was shared;
// rewriting its rights on every save would be work for nothing.
func TestASaveThatKeepsItsGraphDoesNotTouchTheGraphTopic(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, nil)
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.ownGraph("urn:infai:ses:graph:1", "user-a")
	mirror := newFakeGraphMirror()
	mirror.stored["urn:infai:ses:graph:1"] = models.Graph{Id: "urn:infai:ses:graph:1"}
	router := testRouterWithAll(store, shares, nil, mirror, nil, permissions)

	if code := do(t, router, "PUT", "/environments/env-1", "user-a", sharedEnvironmentWithGraph()).Code; code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if store.stored["env-1"].ExternalGraphRef != "urn:infai:ses:graph:1" {
		t.Fatalf("expected the graph id to be kept, got %q", store.stored["env-1"].ExternalGraphRef)
	}
	if permissions.sawTopic(graphsTopic) {
		t.Error("a graph that was only rewritten needs no rights of its own")
	}
}

// panickingPermissions is what a client that ran into a bug of its own looks
// like from here.
type panickingPermissions struct{ *fakePermissions }

func (this *panickingPermissions) GetResource(ctx context.Context, token string, topicId string, id string) (permModel.Resource, error, int) {
	if id == "dev-2" {
		panic("boom")
	}
	return this.fakePermissions.GetResource(ctx, token, topicId, id)
}

// The devices are worked on off the request goroutine, where gin's recovery does
// not reach: an unrecovered panic there would take the whole service down over
// one resource. Without the recover this test does not fail, it crashes.
func TestAPanicWhileSharingOneDeviceBecomesItsFailure(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := &panickingPermissions{fakePermissions: newFakePermissions("dev-1", "dev-2")}
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	failures := ShareFailures{}
	if err := json.Unmarshal(resp.Body.Bytes(), &failures); err != nil {
		t.Fatal(err)
	}
	if len(failures.Devices) != 1 || failures.Devices[0].Id != "dev-2" {
		t.Fatalf("expected the panicking device to be reported, got %+v", failures.Devices)
	}
	//and no detail of the panic reaches the caller
	if strings.Contains(failures.Devices[0].Error, "boom") {
		t.Errorf("a panic is not a message for a caller, got %q", failures.Devices[0].Error)
	}
	if !permissions.userRights("dev-1", "demo-user").Read {
		t.Error("the other device still has to be reached")
	}
}

// ---------------------------------------------------------------------------
// Two shares at once
// ---------------------------------------------------------------------------

// Both read the same set before either writes. Without the compare-and-swap
// both would store their own, and the rights of the one that finished first
// would stand on the devices with nothing that remembers them - a withdrawal
// would never reach them.
func TestTwoSharesArrivingTogetherLeaveNoUnregisteredRights(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)

	//the barrier: neither request may write before both have read
	barrier := sync.WaitGroup{}
	barrier.Add(2)
	shares.afterLoad = func() {
		barrier.Done()
		barrier.Wait()
	}

	codes := make([]int, 2)
	running := sync.WaitGroup{}
	for index, user := range []string{"demo-user", "other-user"} {
		running.Add(1)
		go func(index int, user string) {
			defer running.Done()
			codes[index] = do(t, router, "PUT", "/environments/env-1/shares", "user-a",
				ShareTargets{Users: []string{user}}).Code
		}(index, user)
	}
	running.Wait()
	shares.afterLoad = nil

	sort.Ints(codes)
	if codes[0] != http.StatusOK || codes[1] != http.StatusConflict {
		t.Fatalf("expected one 200 and one 409, got %v", codes)
	}

	//whatever the stored set is, nobody may hold rights outside it
	stored := map[string]bool{}
	for _, user := range shares.users("env-1") {
		stored[user] = true
	}
	if len(stored) != 1 {
		t.Fatalf("expected exactly the winner's set to be stored, got %v", shares.users("env-1"))
	}
	for _, user := range []string{"demo-user", "other-user"} {
		if permissions.knowsUser("dev-1", user) && !stored[user] {
			t.Errorf("%s holds rights that no stored set remembers", user)
		}
	}

	//and the withdrawal reaches everything that is left
	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{}).Code; code != http.StatusOK {
		t.Fatalf("expected the withdrawal to be accepted, got %d", code)
	}
	for _, device := range []string{"dev-1", "dev-2"} {
		if permissions.userEntries(device) != 1 {
			t.Errorf("%s: expected the owner to be the only entry left", device)
		}
	}
}

// ---------------------------------------------------------------------------
// Order and failure of the two writes
// ---------------------------------------------------------------------------

// The superset is the record of everybody who may end up with rights below, so
// it has to be stored before the first of them is written.
func TestTheSupersetIsStoredBeforeTheFirstRightIsWritten(t *testing.T) {
	log := &eventLog{}
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.log = log
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.log = log
	router := testRouterWithShares(store, shares, nil, permissions)

	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}}).Code; code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	firstSave := log.firstWith("shares.save")
	firstTouch := log.firstWith("permissions.")
	if firstSave < 0 || firstTouch < 0 {
		t.Fatalf("expected both a save and a permissions call, got %v", log.all())
	}
	if firstSave > firstTouch {
		t.Errorf("the set has to be stored before the first right is written, got %v", log.all())
	}
	if !strings.Contains(log.all()[firstSave], "demo-user") {
		t.Errorf("the first save has to carry the new account, got %q", log.all()[firstSave])
	}
}

// The rights are written and the shrink to the requested set is not stored. What
// stands is the superset, and that is what makes the grants withdrawable.
func TestAFailedFinalWriteLeavesTheSupersetStored(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	//the first save is the superset, the second the requested set
	shares.saveErrOn = 2
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", resp.Code, resp.Body.String())
	}
	if !permissions.userRights("dev-1", "demo-user").Read {
		t.Fatal("the rights were written before the failing store call")
	}
	//the read shows what may hold rights, which is the point of storing it first
	answer := sharesResponseOf(t, do(t, router, "GET", "/environments/env-1/shares", "user-a", nil).Body.Bytes())
	if len(answer.Users) != 1 || answer.Users[0] != "demo-user" {
		t.Fatalf("expected the superset to be readable, got %+v", answer)
	}
	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{}).Code; code != http.StatusOK {
		t.Fatalf("expected the withdrawal to be accepted, got %d", code)
	}
	if permissions.knowsUser("dev-1", "demo-user") {
		t.Error("the withdrawal has to reach the grant of the failed call")
	}
}

// ---------------------------------------------------------------------------
// Limits, deadline, statuses, stores
// ---------------------------------------------------------------------------

// The superset can grow past the limit through failed attempts, and the way out
// has to stay open.
func TestASupersetBeyondTheLimitIsRefusedAndCanStillBeShrunk(t *testing.T) {
	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	shares.set("env-1", manyPrincipals(maxShares), nil)
	permissions := newFakePermissions("dev-1", "dev-2")
	router := testRouterWithShares(store, shares, nil, permissions)

	//the request itself is within the limit; it is the union with what earlier
	//attempts left behind that is not
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"one-too-many"}})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "empty") {
		t.Errorf("the answer has to say how to get out of it, got %s", resp.Body.String())
	}
	_, sets, _ := permissions.calls()
	if len(sets) != 0 {
		t.Error("a refused request must not write a right")
	}
	//shrinking is never refused over the limit
	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareTargets{}).Code; code != http.StatusOK {
		t.Fatalf("expected the empty set to be accepted, got %d", code)
	}
	if len(shares.users("env-1")) != 0 {
		t.Errorf("expected the leftovers to be gone, got %v", shares.users("env-1"))
	}
}

// blockingPermissions never answers, which is what a permissions-v2 that accepts
// connections and hangs looks like.
type blockingPermissions struct {
	*fakePermissions
	release chan struct{}
}

func (this *blockingPermissions) GetResource(ctx context.Context, token string, topicId string, id string) (permModel.Resource, error, int) {
	select {
	case <-this.release:
		return this.fakePermissions.GetResource(ctx, token, topicId, id)
	case <-ctx.Done():
		return permModel.Resource{}, ctx.Err(), 0
	}
}

// A share of a very large environment has to end as a report rather than run
// past the answer the api still has to write.
func TestTheDeadlineTurnsUnreachedResourcesIntoFailures(t *testing.T) {
	previous := shareDeadline
	shareDeadline = 50 * time.Millisecond
	t.Cleanup(func() { shareDeadline = previous })

	store := storeWithSharedEnvironment()
	shares := newFakeShares()
	permissions := &blockingPermissions{fakePermissions: newFakePermissions("dev-1", "dev-2"), release: make(chan struct{})}
	t.Cleanup(func() { close(permissions.release) })
	router := testRouterWithShares(store, shares, nil, permissions)

	started := time.Now()
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Users: []string{"demo-user"}})
	//not the caller's fault, so not a 400
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	if time.Since(started) > 5*time.Second {
		t.Errorf("expected the deadline to end it, took %v", time.Since(started))
	}
	failures := ShareFailures{}
	if err := json.Unmarshal(resp.Body.Bytes(), &failures); err != nil {
		t.Fatal(err)
	}
	if len(failures.Devices) != 2 {
		t.Errorf("expected every unreached device to be reported, got %+v", failures.Devices)
	}
	//and the superset stands, so a repeat can still withdraw
	if len(shares.users("env-1")) != 1 {
		t.Errorf("expected the superset to stand, got %v", shares.users("env-1"))
	}
}

// One refused group is the caller's mistake; one unreachable device is not, and
// a call that hits both must not be reported as a bad request.
func TestARefusalMixedWithAPlatformFailureIsABadGateway(t *testing.T) {
	store := storeWithSharedEnvironment()
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.setErr["dev-1"] = errors.New("user may not share with group /strangers")
	permissions.setCode["dev-1"] = http.StatusBadRequest
	permissions.getErr["dev-2"] = errors.New("permissions-v2 unreachable")
	router := testRouterWithShares(store, newFakeShares(), nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a",
		ShareTargets{Groups: []string{"/strangers"}})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	failures := ShareFailures{}
	if err := json.Unmarshal(resp.Body.Bytes(), &failures); err != nil {
		t.Fatal(err)
	}
	if len(failures.Devices) != 2 {
		t.Fatalf("expected both to be reported, got %+v", failures.Devices)
	}
	//the status of each is what let the handler decide
	if failures.Devices[0].Status != http.StatusBadRequest || failures.Devices[1].Status == http.StatusBadRequest {
		t.Errorf("expected the statuses to be carried, got %+v", failures.Devices)
	}
}

// An instance without a share store cannot know that an environment is shared
// with nobody, so it must not say so.
func TestReadingSharesWithoutAStoreIsAnError(t *testing.T) {
	store := storeWithSharedEnvironment()
	router := testRouterWithShares(store, nil, nil, newFakePermissions("dev-1", "dev-2"))
	if code := do(t, router, "GET", "/environments/env-1/shares", "user-a", nil).Code; code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", code)
	}
}

// The leftover set of a deleted environment has to go before this save creates
// anything, or a second create of the same id that already shared its devices
// would have its set deleted by this one.
func TestTheLeftoverSetIsDroppedBeforeAnyDeviceIsCreated(t *testing.T) {
	store := newFakeEnvironments()
	shares := newFakeShares()
	shares.set("env-new", []string{"demo-user"}, nil)
	catalog := &fakeCatalog{}
	creationsAtDelete := -1
	shares.onDelete = func() { creationsAtDelete = len(catalog.created) }
	router := testRouterWithShares(store, shares, catalog, newFakePermissions())

	env := minimalEnvironment()
	if code := do(t, router, "PUT", "/environments/env-new", "user-a", env).Code; code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(catalog.created) != 1 {
		t.Fatalf("expected the asset to be provisioned, got %+v", catalog.created)
	}
	if creationsAtDelete != 0 {
		t.Errorf("the set has to be dropped before the first device is created, %d were there", creationsAtDelete)
	}
	if shares.has("env-new") {
		t.Error("the leftover set has to be gone")
	}

	//and the same on a create with a server assigned id
	catalog.created = nil
	creationsAtDelete = -1
	if code := do(t, router, "POST", "/environments", "user-a", minimalEnvironment()).Code; code != http.StatusCreated {
		t.Fatal("expected 201")
	}
	if creationsAtDelete != 0 {
		t.Errorf("a create has to drop it before provisioning too, %d were there", creationsAtDelete)
	}
}
