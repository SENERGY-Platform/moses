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
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/repo"
	permModel "github.com/SENERGY-Platform/permissions-v2/pkg/model"
)

// ---------------------------------------------------------------------------
// Graph admins get administrate on the graph, so they may delete it. moses
// takes back only the administrate it granted, and never the owner's.
// ---------------------------------------------------------------------------

func (this *fakeShares) setWithGraphAdmins(environmentId string, users []string, groups []string, writers repo.Principals, admins repo.Principals) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.stored[environmentId] = repo.ShareSet{EnvironmentId: environmentId, Users: users, Groups: groups,
		GraphWriters: writers, GraphAdmins: admins, Version: 1}
}

func (this *fakeShares) graphAdmins(environmentId string) repo.Principals {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.stored[environmentId].GraphAdmins
}

var fullRights = permModel.PermissionsMap{Read: true, Write: true, Execute: true, Administrate: true}

func isGraphAdmin(rights permModel.PermissionsMap) bool {
	return rights == fullRights
}

// storeWithAGraphAdmin is demo-user administrating, other-user reading the graph.
func storeWithAGraphAdmin() (*fakeEnvironments, *fakeShares, *fakePermissions) {
	shares := newFakeShares()
	shares.setWithGraphAdmins("env-1", []string{"demo-user", "other-user"}, []string{"/demo"}, repo.Principals{},
		repo.Principals{Users: []string{"demo-user"}, Groups: []string{"/demo"}})
	permissions := graphWriterPermissions()
	permissions.graphRights("demo-user", fullRights)
	permissions.graphGroup("/demo", fullRights)
	permissions.graphRights("other-user", permModel.PermissionsMap{Read: true, Execute: true})
	return storeWithGraphEnvironment(), shares, permissions
}

func TestAGraphAdminGetsAdministrateOnTheGraphAndNothingMoreOnTheDevices(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	permissions := graphWriterPermissions()
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareRequest{
		ShareTargets: ShareTargets{Users: []string{"demo-user", "other-user"}, Groups: []string{"/demo"}},
		GraphAdmins:  &ShareTargets{Users: []string{"demo-user"}, Groups: []string{"/demo"}},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if rights := permissions.graphUserRights(testGraph, "demo-user"); !isGraphAdmin(rights) {
		t.Errorf("a graph admin holds every right on the graph, got %+v", rights)
	}
	if rights := permissions.graphGroupRights(testGraph, "/demo"); !isGraphAdmin(rights) {
		t.Errorf("an administrating group holds every right on the graph, got %+v", rights)
	}
	if rights := permissions.graphUserRights(testGraph, "other-user"); rights.Write || rights.Administrate || !rights.Read {
		t.Errorf("a shared account that is no graph admin reads the graph only, got %+v", rights)
	}
	if rights := permissions.graphUserRights(testGraph, "user-a"); !isGraphAdmin(rights) {
		t.Errorf("the owner must not be narrowed, got %+v", rights)
	}
	for _, device := range managedTestDevices {
		if rights := permissions.userRights(device, "demo-user"); rights.Write || rights.Administrate || !rights.Read {
			t.Errorf("%s: a graph admin reads the device only, got %+v", device, rights)
		}
		if rights := permissions.groupRights(device, "/demo"); rights.Write || rights.Administrate || !rights.Read {
			t.Errorf("%s: an administrating group reads the device only, got %+v", device, rights)
		}
	}

	if got := shares.graphAdmins("env-1"); len(got.Users) != 1 || got.Users[0] != "demo-user" || len(got.Groups) != 1 || got.Groups[0] != "/demo" {
		t.Errorf("the graph admins have to be stored with the set, got %+v", got)
	}
	if answer := sharesResponseOf(t, resp.Body.Bytes()); len(answer.GraphAdmins.Users) != 1 || answer.GraphAdmins.Users[0] != "demo-user" {
		t.Errorf("the answer has to carry the graph admins, got %+v", answer.GraphAdmins)
	}
	read := sharesResponseOf(t, do(t, router, "GET", "/environments/env-1/shares", "user-a", nil).Body.Bytes())
	if len(read.GraphAdmins.Users) != 1 || read.GraphAdmins.Users[0] != "demo-user" || len(read.GraphAdmins.Groups) != 1 {
		t.Errorf("the read has to serve the stored graph admins, got %+v", read.GraphAdmins)
	}
}

func TestAGraphAdminsObjectReplacesTheStoredOnes(t *testing.T) {
	store, shares, permissions := storeWithAGraphAdmin()
	resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions),
		`{"users":["demo-user","other-user"],"groups":["/demo"],"graph_admins":{"users":["other-user"]}}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := shares.graphAdmins("env-1"); len(got.Users) != 1 || got.Users[0] != "other-user" || len(got.Groups) != 0 {
		t.Errorf("the sent admins replace the stored ones, got %+v", got)
	}
	if rights := permissions.graphUserRights(testGraph, "demo-user"); rights.Administrate || rights.Write || !rights.Read || !rights.Execute {
		t.Errorf("the former admin loses administrate and write and keeps read and execute, got %+v", rights)
	}
	if rights := permissions.graphGroupRights(testGraph, "/demo"); rights.Administrate || rights.Write || !rights.Read {
		t.Errorf("the former administrating group loses administrate and write, got %+v", rights)
	}
	if rights := permissions.graphUserRights(testGraph, "other-user"); !isGraphAdmin(rights) {
		t.Errorf("the new admin gets every right, got %+v", rights)
	}
	if rights := permissions.graphUserRights(testGraph, "user-a"); !isGraphAdmin(rights) {
		t.Errorf("the owner must not be touched, got %+v", rights)
	}
}

// The write that came with administrate stays for an account that is a graph
// writer as well.
func TestAFormerGraphAdminThatIsAGraphWriterKeepsWrite(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	shares.setWithGraphAdmins("env-1", []string{"demo-user"}, nil, repo.Principals{Users: []string{"demo-user"}},
		repo.Principals{Users: []string{"demo-user"}})
	permissions := graphWriterPermissions()
	permissions.graphRights("demo-user", fullRights)

	if resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions), `{"users":["demo-user"],"graph_admins":{}}`); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if rights := permissions.graphUserRights(testGraph, "demo-user"); rights.Administrate || !rights.Write || !rights.Read {
		t.Errorf("a graph writer loses administrate only, got %+v", rights)
	}
}

func TestAnExplicitEmptyGraphAdminsReplacesThem(t *testing.T) {
	for name, body := range map[string]string{
		"empty object": `{"users":["demo-user","other-user"],"groups":["/demo"],"graph_admins":{}}`,
		"empty lists":  `{"users":["demo-user","other-user"],"groups":["/demo"],"graph_admins":{"users":[],"groups":[]}}`,
	} {
		store, shares, permissions := storeWithAGraphAdmin()
		resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions), body)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", name, resp.Code, resp.Body.String())
		}
		if got := shares.graphAdmins("env-1"); len(got.Users) != 0 || len(got.Groups) != 0 {
			t.Errorf("%s: an explicit empty set replaces the admins, got %+v", name, got)
		}
		if rights := permissions.graphUserRights(testGraph, "demo-user"); rights.Administrate || rights.Write || !rights.Read {
			t.Errorf("%s: the former admin keeps read only, got %+v", name, rights)
		}
		if rights := permissions.graphGroupRights(testGraph, "/demo"); rights.Administrate || rights.Write || !rights.Read {
			t.Errorf("%s: the former administrating group keeps read only, got %+v", name, rights)
		}
	}
}

// A client that does not know graph_admins must not take administrate away.
func TestAnAbsentOrNullGraphAdminsKeepsTheStoredOnes(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"users":["demo-user","other-user","third-user"],"groups":["/demo"]}`,
		"null":   `{"users":["demo-user","other-user","third-user"],"groups":["/demo"],"graph_admins":null}`,
	} {
		store, shares, permissions := storeWithAGraphAdmin()
		resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions), body)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", name, resp.Code, resp.Body.String())
		}
		if got := shares.graphAdmins("env-1"); len(got.Users) != 1 || got.Users[0] != "demo-user" || len(got.Groups) != 1 {
			t.Errorf("%s: the stored admins have to be kept, got %+v", name, got)
		}
		if answer := sharesResponseOf(t, resp.Body.Bytes()); len(answer.GraphAdmins.Users) != 1 {
			t.Errorf("%s: the answer has to carry the kept admins, got %+v", name, answer.GraphAdmins)
		}
		if rights := permissions.graphUserRights(testGraph, "demo-user"); !isGraphAdmin(rights) {
			t.Errorf("%s: the kept admin keeps every right, got %+v", name, rights)
		}
		if rights := permissions.graphGroupRights(testGraph, "/demo"); !isGraphAdmin(rights) {
			t.Errorf("%s: the kept administrating group keeps every right, got %+v", name, rights)
		}
		if rights := permissions.graphUserRights(testGraph, "third-user"); rights.Administrate || rights.Write || !rights.Read {
			t.Errorf("%s: a newly shared account is no admin, got %+v", name, rights)
		}
	}
}

// A kept admin that is no longer shared goes with its share, never a 400.
func TestAnAbsentGraphAdminsDropsAnAdminThatIsNoLongerShared(t *testing.T) {
	store, shares, permissions := storeWithAGraphAdmin()
	resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions), `{"users":["other-user"]}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if got := shares.graphAdmins("env-1"); len(got.Users) != 0 || len(got.Groups) != 0 {
		t.Errorf("the admin that left the share must not be kept, got %+v", got)
	}
	if permissions.graphKnowsUser(testGraph, "demo-user") || permissions.graphKnowsGroup("/demo") {
		t.Error("an admin moses granted loses the whole entry with the share")
	}
	if rights := permissions.graphUserRights(testGraph, "user-a"); !isGraphAdmin(rights) {
		t.Errorf("the owner must not be touched, got %+v", rights)
	}
}

func TestAGraphAdminHasToBeSharedWith(t *testing.T) {
	for name, request := range map[string]ShareRequest{
		"a user that is not shared with": {
			ShareTargets: ShareTargets{Users: []string{"demo-user"}},
			GraphAdmins:  &ShareTargets{Users: []string{"stranger"}},
		},
		"a group that is not shared with": {
			ShareTargets: ShareTargets{Groups: []string{"/demo"}},
			GraphAdmins:  &ShareTargets{Groups: []string{"/strangers"}},
		},
		"an admin without any share": {
			GraphAdmins: &ShareTargets{Users: []string{"demo-user"}},
		},
		"an empty admin id": {
			ShareTargets: ShareTargets{Users: []string{"demo-user"}},
			GraphAdmins:  &ShareTargets{Users: []string{" "}},
		},
		"an admin group without a path": {
			ShareTargets: ShareTargets{Groups: []string{"/demo"}},
			GraphAdmins:  &ShareTargets{Groups: []string{"demo"}},
		},
	} {
		shares := newFakeShares()
		permissions := graphWriterPermissions()
		resp := do(t, testRouterWithShares(storeWithGraphEnvironment(), shares, nil, permissions), "PUT", "/environments/env-1/shares", "user-a", request)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", name, resp.Code, resp.Body.String())
			continue
		}
		if body := resp.Body.String(); !strings.Contains(body, "graph_admins") || strings.Contains(body, "graph_writers") {
			t.Errorf("%s: the answer has to say it is about graph_admins, got %s", name, body)
		}
		gets, sets, _ := permissions.calls()
		if len(gets) != 0 || len(sets) != 0 || shares.has("env-1") {
			t.Errorf("%s: a refused set must change nothing", name)
		}
	}
}

func TestGraphAdminsDoNotCountAgainTowardsTheLimit(t *testing.T) {
	everybody := manyPrincipals(maxShares)
	router := testRouterWithShares(storeWithGraphEnvironment(), newFakeShares(), nil, graphWriterPermissions())
	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareRequest{
		ShareTargets: ShareTargets{Users: everybody},
		GraphAdmins:  &ShareTargets{Users: everybody},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 at the limit with every account an admin, got %d: %s", resp.Code, resp.Body.String())
	}
}

// administrate moses did not grant is never changed: not the owner's, not an
// administrator's, not one given by hand, whatever the request says.
func TestAPutTakesBackOnlyTheAdministrateItGranted(t *testing.T) {
	for name, body := range map[string]string{
		"kept in the share": `{"users":["demo-user","admin-user","hand-admin"],"graph_admins":{}}`,
		"withdrawn":         `{"users":[],"graph_admins":{}}`,
	} {
		store := storeWithGraphEnvironment()
		shares := newFakeShares()
		shares.setWithGraphAdmins("env-1", []string{"demo-user", "admin-user", "hand-admin"}, nil, repo.Principals{},
			repo.Principals{Users: []string{"demo-user"}})
		permissions := graphWriterPermissions()
		permissions.graphRights("demo-user", fullRights)
		permissions.graphRights("admin-user", fullRights)
		permissions.graphRights("hand-admin", permModel.PermissionsMap{Read: true, Administrate: true})
		permissions.graphRights("outsider", fullRights)

		if resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions), body); resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", name, resp.Code, resp.Body.String())
		}
		if name == "withdrawn" {
			if permissions.graphKnowsUser(testGraph, "demo-user") {
				t.Errorf("%s: the former graph admin loses the entry with the share", name)
			}
		} else if rights := permissions.graphUserRights(testGraph, "demo-user"); rights.Administrate || rights.Write || !rights.Read {
			t.Errorf("%s: the former graph admin loses administrate and write, got %+v", name, rights)
		}
		if rights := permissions.graphUserRights(testGraph, "admin-user"); !isGraphAdmin(rights) {
			t.Errorf("%s: an administrator's entry is never changed, got %+v", name, rights)
		}
		//a share adds read and execute to every shared entry, so only these two are pinned
		if rights := permissions.graphUserRights(testGraph, "hand-admin"); !rights.Administrate || rights.Write {
			t.Errorf("%s: an administrate given by hand is never changed, got %+v", name, rights)
		}
		if rights := permissions.graphUserRights(testGraph, "outsider"); !isGraphAdmin(rights) {
			t.Errorf("%s: an account outside the share must not be touched, got %+v", name, rights)
		}
		if rights := permissions.graphUserRights(testGraph, "user-a"); !isGraphAdmin(rights) {
			t.Errorf("%s: the owner keeps the graph, got %+v", name, rights)
		}
	}
}

// The owner named as a graph admin of their own environment keeps administrate
// when they are taken out again.
func TestTheOwnerKeepsAdministrateAsAFormerGraphAdmin(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	shares.setWithGraphAdmins("env-1", []string{"user-a", "demo-user"}, nil, repo.Principals{},
		repo.Principals{Users: []string{"user-a", "demo-user"}})
	permissions := graphWriterPermissions()
	permissions.graphRights("demo-user", fullRights)
	//another administrating user, so the owner is not kept as the last one
	permissions.graphRights("admin-user", fullRights)

	if resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions), `{"users":[]}`); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if rights := permissions.graphUserRights(testGraph, "user-a"); !isGraphAdmin(rights) {
		t.Errorf("the owner keeps administrate, got %+v", rights)
	}
	if permissions.graphKnowsUser(testGraph, "demo-user") {
		t.Error("the other former graph admin loses the entry")
	}
}

// permissions-v2 requires an administrating user on every resource, so
// administrate is not taken when nobody else would hold it.
func TestTakingAdministrateBackNeverLeavesTheGraphWithoutAnAdministratingUser(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	shares.setWithGraphAdmins("env-1", []string{"demo-user"}, []string{"/demo"}, repo.Principals{},
		repo.Principals{Users: []string{"demo-user"}, Groups: []string{"/demo"}})
	//the graph is owned by somebody else than the environment, as after a save by an administrator
	permissions := newFakePermissions("dev-1", "dev-2")
	permissions.ownGraph(testGraph, "demo-user")
	permissions.graphGroup("/demo", fullRights)

	if resp := putRaw(t, testRouterWithShares(store, shares, nil, permissions), `{"users":["demo-user"],"groups":["/demo"],"graph_admins":{}}`); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if rights := permissions.graphUserRights(testGraph, "demo-user"); !rights.Administrate {
		t.Errorf("the last administrating user keeps administrate, got %+v", rights)
	}
	//a group never is the administrating user, so it loses it regardless
	if rights := permissions.graphGroupRights(testGraph, "/demo"); rights.Administrate || rights.Write {
		t.Errorf("the former administrating group loses administrate, got %+v", rights)
	}
}

// administrate granted by a call that failed elsewhere has to be recorded, or the
// next call would not know to take it back.
func TestAFailedShareRecordsTheGraphAdminsSoAdministrateCanBeTakenBack(t *testing.T) {
	store := storeWithGraphEnvironment()
	shares := newFakeShares()
	shares.setWithGraphAdmins("env-1", []string{"other-user"}, nil, repo.Principals{}, repo.Principals{Users: []string{"other-user"}})
	permissions := graphWriterPermissions()
	permissions.graphRights("other-user", fullRights)
	permissions.getErr["dev-2"] = errors.New("permissions-v2 unreachable")
	router := testRouterWithShares(store, shares, nil, permissions)

	resp := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareRequest{
		ShareTargets: ShareTargets{Users: []string{"demo-user"}},
		GraphAdmins:  &ShareTargets{Users: []string{"demo-user"}},
	})
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", resp.Code, resp.Body.String())
	}
	if !permissions.graphUserRights(testGraph, "demo-user").Administrate {
		t.Fatal("expected the graph to have gone through")
	}
	if got := sortedCopy(shares.graphAdmins("env-1").Users); len(got) != 2 || got[0] != "demo-user" || got[1] != "other-user" {
		t.Fatalf("the union of the stored and the requested admins has to stand, got %v", got)
	}
	read := sharesResponseOf(t, do(t, router, "GET", "/environments/env-1/shares", "user-a", nil).Body.Bytes())
	if got := read.GraphAdmins.Users; len(got) != 2 {
		t.Errorf("the read has to show every account that may hold administrate, got %v", got)
	}

	permissions.getErr = map[string]error{}
	if code := do(t, router, "PUT", "/environments/env-1/shares", "user-a", ShareRequest{
		ShareTargets: ShareTargets{Users: []string{"demo-user", "other-user"}},
		GraphAdmins:  &ShareTargets{},
	}).Code; code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	for _, user := range []string{"demo-user", "other-user"} {
		if rights := permissions.graphUserRights(testGraph, user); rights.Administrate || rights.Write || !rights.Read {
			t.Errorf("%s: the administrate of the failed call has to be taken back, got %+v", user, rights)
		}
	}
}

func TestALegacySetServesNoGraphAdmins(t *testing.T) {
	shares := newFakeShares()
	shares.set("env-1", []string{"demo-user"}, nil)
	router := testRouterWithShares(storeWithSharedEnvironment(), shares, nil, newFakePermissions("dev-1", "dev-2"))

	resp := do(t, router, "GET", "/environments/env-1/shares", "user-a", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if body := resp.Body.String(); !strings.Contains(body, `"graph_admins":{"users":[],"groups":[]}`) {
		t.Errorf("expected empty admin lists, got %s", body)
	}
}

// A graph a save creates gets the stored set, administrate for the graph admins
// included.
func TestAGraphCreatedByASaveInheritsTheGraphAdmins(t *testing.T) {
	store := storeWithSharedEnvironment() //no graph ref yet
	shares := newFakeShares()
	shares.setWithGraphAdmins("env-1", []string{"demo-user", "writer", "reader"}, []string{"/demo"},
		repo.Principals{Users: []string{"writer"}}, repo.Principals{Users: []string{"demo-user"}, Groups: []string{"/demo"}})
	permissions := newFakePermissions("dev-1", "dev-2")
	mirror := newFakeGraphMirror()
	mirror.onCreate = func(id string, owner string) { permissions.ownGraph(id, owner) }
	router := testRouterWithAll(store, shares, nil, mirror, nil, permissions)

	if resp := do(t, router, "PUT", "/environments/env-1", "user-a", sharedEnvironment()); resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	graph := store.stored["env-1"].ExternalGraphRef
	if graph == "" {
		t.Fatal("expected the save to create a graph")
	}
	assertSharedGraph(t, permissions, graph)
}

// assertSharedGraph checks a graph that got the set of
// TestAGraphCreatedByASaveInheritsTheGraphAdmins.
func assertSharedGraph(t *testing.T, permissions *fakePermissions, graph string) {
	t.Helper()
	if rights := permissions.graphUserRights(graph, "demo-user"); !isGraphAdmin(rights) {
		t.Errorf("the graph admin gets every right on the graph, got %+v", rights)
	}
	if rights := permissions.graphGroupRights(graph, "/demo"); !isGraphAdmin(rights) {
		t.Errorf("the administrating group gets every right on the graph, got %+v", rights)
	}
	if rights := permissions.graphUserRights(graph, "writer"); rights.Administrate || !rights.Write || !rights.Read {
		t.Errorf("the graph writer gets write, got %+v", rights)
	}
	if rights := permissions.graphUserRights(graph, "reader"); rights.Administrate || rights.Write || !rights.Read || !rights.Execute {
		t.Errorf("a shared account reads the graph, got %+v", rights)
	}
	if rights := permissions.graphUserRights(graph, "user-a"); !isGraphAdmin(rights) {
		t.Errorf("the owner keeps the graph, got %+v", rights)
	}
}
