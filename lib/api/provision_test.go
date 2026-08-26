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
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// The platform device is deleted with the asset that justified it - and only
// then. Every test here exists for one half of that sentence: a device moses
// created must not survive its asset, and a device the user attached must not be
// touched, ever. The second half is the expensive one to get wrong: it deletes
// somebody's real device together with its timeseries, and no retry brings it
// back.
// ---------------------------------------------------------------------------

// environmentWithTwoAssets is a hall with a machine and a sub-room with a meter,
// so the walk through nested zones is exercised rather than assumed.
func environmentWithTwoAssets() domain.Environment {
	env := environmentWithNewMachine()
	env.Zones[0].Zones = []domain.Zone{{
		Name: "Nebenraum", Type: domain.ZoneRoom,
		Assets: []domain.Asset{{Name: "Zähler", Kind: domain.AssetMeter, ExternalTypeId: "dt-2"}},
	}}
	return env
}

func namedDeviceIds() map[string]string {
	return map[string]string{"Kompressor 1": "urn:device:kompressor", "Zähler": "urn:device:zaehler"}
}

// putEnvironment stores a document over the api and returns what was stored,
// which is what a client would edit and send back.
func putEnvironment(t *testing.T, router *gin.Engine, id string, user string, env domain.Environment) domain.Environment {
	t.Helper()
	resp := do(t, router, "PUT", "/environments/"+id, user, env)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	stored := domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// storeDirectly puts a document into the store without going through a handler,
// for the cases where the stored state is the precondition rather than the
// result: ids are assigned as the handler would.
func storeDirectly(t *testing.T, store *fakeEnvironments, id string, owner string, env domain.Environment) domain.Environment {
	t.Helper()
	env.Id, env.Owner = id, owner
	domain.AssignIds(&env)
	if err := domain.Validate(env); err != nil {
		t.Fatalf("the precondition itself is invalid: %v", err)
	}
	store.stored[id] = env
	return env
}

func assetNamed(t *testing.T, env domain.Environment, name string) domain.Asset {
	t.Helper()
	found := []domain.Asset{}
	forEachAsset(&env, func(asset *domain.Asset) {
		if asset.Name == name {
			found = append(found, *asset)
		}
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly one asset named %q, got %d", name, len(found))
	}
	return found[0]
}

// copyEnvironment deep copies through json, so a test that edits the copy does
// not silently edit the document it compares against: the zone slices are shared
// by a plain assignment.
func copyEnvironment(t *testing.T, env domain.Environment) domain.Environment {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	copied := domain.Environment{}
	if err = json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

// withoutAsset returns a copy of the document with one asset removed, which is
// what an editor sends after the user deleted it.
func withoutAsset(t *testing.T, env domain.Environment, name string) domain.Environment {
	t.Helper()
	copied := copyEnvironment(t, env)
	removed := 0
	var walk func(zone *domain.Zone)
	walk = func(zone *domain.Zone) {
		for i := range zone.Zones {
			walk(&zone.Zones[i])
		}
		kept := []domain.Asset{}
		for _, asset := range zone.Assets {
			if asset.Name == name {
				removed++
				continue
			}
			kept = append(kept, asset)
		}
		zone.Assets = kept
	}
	for i := range copied.Zones {
		walk(&copied.Zones[i])
	}
	if removed != 1 {
		t.Fatalf("expected to remove exactly one asset named %q, removed %d", name, removed)
	}
	return copied
}

// (a) The lifecycle closes: what storing created, removing deletes. The asset
// sits in a nested zone, where the old code would not even have looked.
func TestUpdatingAnEnvironmentDeletesTheDeviceOfARemovedAsset(t *testing.T) {
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	stored := putEnvironment(t, router, "env-1", "user-a", environmentWithTwoAssets())
	if ref := assetNamed(t, stored, "Zähler").ExternalRef; ref != "urn:device:zaehler" {
		t.Fatalf("setup: expected the nested asset to be provisioned, got %q", ref)
	}
	if !assetNamed(t, stored, "Zähler").ExternalManaged {
		t.Fatal("setup: a device moses created has to be marked as its own")
	}

	putEnvironment(t, router, "env-1", "user-a", withoutAsset(t, stored, "Zähler"))

	if len(catalog.deleted) != 1 || catalog.deleted[0] != "urn:device:zaehler" {
		t.Fatalf("expected exactly the device of the removed asset to be deleted, got %v", catalog.deleted)
	}
	//the asset that stayed keeps its device
	if ref := assetNamed(t, store.stored["env-1"], "Kompressor 1").ExternalRef; ref != "urn:device:kompressor" {
		t.Errorf("the remaining asset lost its device, got %q", ref)
	}
}

// (b) A device the user picked in an editor is inventory of the platform: it
// exists without the simulation and carries timeseries that outlive it.
func TestRemovingAnAssetLeavesAPickedDeviceAlone(t *testing.T) {
	catalog := &fakeCatalog{}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	env := environmentWithTwoAssets()
	env.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:picked"
	stored := putEnvironment(t, router, "env-1", "user-a", env)
	if assetNamed(t, stored, "Zähler").ExternalManaged {
		t.Fatal("a device that was already there was not created by moses")
	}
	if len(catalog.created) != 1 {
		t.Fatalf("setup: only the asset without a device may be provisioned, got %+v", catalog.created)
	}

	putEnvironment(t, router, "env-1", "user-a", withoutAsset(t, stored, "Zähler"))

	if len(catalog.deleted) != 0 {
		t.Fatalf("a picked device must survive its asset, deleted %v", catalog.deleted)
	}
}

// (c) The client sends the whole document back, so external_managed is the one
// field that must never be believed: a stale copy or a handcrafted request would
// otherwise hand moses the right to delete a device it never created.
func TestAnEchoedManagedFlagOnAPickedDeviceIsIgnored(t *testing.T) {
	catalog := &fakeCatalog{}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	env := environmentWithTwoAssets()
	env.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:picked"
	stored := storeDirectly(t, store, "env-1", "user-a", env)

	//the client sends the document back and claims the picked device is moses'
	lying := copyEnvironment(t, stored)
	lying.Zones[0].Zones[0].Assets[0].ExternalManaged = true
	putEnvironment(t, router, "env-1", "user-a", lying)

	if assetNamed(t, store.stored["env-1"], "Zähler").ExternalManaged {
		t.Fatal("the server has to decide external_managed, not the client")
	}
	if len(catalog.deleted) != 0 {
		t.Fatalf("nothing was removed, so nothing may be deleted, got %v", catalog.deleted)
	}

	//and the claim must not arm the deletion of the next update either
	putEnvironment(t, router, "env-1", "user-a", withoutAsset(t, store.stored["env-1"], "Zähler"))
	if len(catalog.deleted) != 0 {
		t.Fatalf("the picked device must survive, deleted %v", catalog.deleted)
	}
}

// (c2) The same lie on a create, followed by deleting the environment: a new
// document owns nothing, whatever it says about itself.
func TestAnEchoedManagedFlagOnACreateIsIgnored(t *testing.T) {
	catalog := &fakeCatalog{}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	env := environmentWithTwoAssets()
	env.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:picked"
	env.Zones[0].Zones[0].Assets[0].ExternalManaged = true

	resp := do(t, router, "POST", "/environments", "user-a", env)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	created := domain.Environment{}
	if err := json.Unmarshal(resp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if assetNamed(t, created, "Zähler").ExternalManaged {
		t.Fatal("a create cannot claim a device that was picked")
	}

	if code := do(t, router, "DELETE", "/environments/"+created.Id, "user-a", nil).Code; code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", code)
	}
	for _, deleted := range catalog.deleted {
		if deleted == "urn:device:picked" {
			t.Fatalf("the picked device must not be deleted with the environment, deleted %v", catalog.deleted)
		}
	}
}

// (d) Exchanging a provisioned device for a picked one releases the old device
// even though the asset stayed: the comparison is about the device, not about
// the asset that carried it.
func TestExchangingAManagedDeviceForAPickedOneReleasesTheOldOne(t *testing.T) {
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	stored := putEnvironment(t, router, "env-1", "user-a", environmentWithTwoAssets())

	next := copyEnvironment(t, stored)
	//the user picks a different device for the same asset
	next.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:picked"
	putEnvironment(t, router, "env-1", "user-a", next)

	if len(catalog.deleted) != 1 || catalog.deleted[0] != "urn:device:zaehler" {
		t.Fatalf("expected the released device to be deleted, got %v", catalog.deleted)
	}
	after := assetNamed(t, store.stored["env-1"], "Zähler")
	if after.ExternalRef != "urn:device:picked" || after.ExternalManaged {
		t.Fatalf("the picked device must be stored and must not be managed, got %+v", after)
	}

	//and it survives the asset it was picked for
	putEnvironment(t, router, "env-1", "user-a", withoutAsset(t, store.stored["env-1"], "Zähler"))
	if len(catalog.deleted) != 1 {
		t.Fatalf("only the released device may ever be deleted, got %v", catalog.deleted)
	}
}

// (e) Deleting the environment takes the devices moses created with it and
// leaves the picked ones standing.
func TestDeletingAnEnvironmentDeletesOnlyTheDevicesItProvisioned(t *testing.T) {
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	env := environmentWithTwoAssets()
	env.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:picked"
	putEnvironment(t, router, "env-1", "user-a", env)

	if code := do(t, router, "DELETE", "/environments/env-1", "user-a", nil).Code; code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", code)
	}
	if len(catalog.deleted) != 1 || catalog.deleted[0] != "urn:device:kompressor" {
		t.Fatalf("expected only the provisioned device to be deleted, got %v", catalog.deleted)
	}
	if len(store.stored) != 0 {
		t.Fatal("expected the environment to be gone")
	}
}

// An environment nobody may touch must not have its devices touched either. The
// device deletion happens after the ownership check, and this pins that it stays
// there.
func TestDeletingAForeignEnvironmentDeletesNoDevice(t *testing.T) {
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)
	putEnvironment(t, router, "env-1", "user-a", environmentWithTwoAssets())

	if code := do(t, router, "DELETE", "/environments/env-1", "user-b", nil).Code; code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", code)
	}
	if len(catalog.deleted) != 0 {
		t.Fatalf("a foreign environment must not lose its devices, deleted %v", catalog.deleted)
	}
}

// (f) The cleanup runs after the write, so a failing delete may not fail the
// request: the change did happen, and the caller told otherwise would repeat it.
// What stays behind is a device without an asset, which is what moses left behind
// for every removal before any of this existed.
func TestAFailingDeviceDeletionDoesNotFailTheRequest(t *testing.T) {
	catalog := &fakeCatalog{idsByName: namedDeviceIds(), deleteErr: errors.New("device-manager unreachable")}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	stored := putEnvironment(t, router, "env-1", "user-a", environmentWithTwoAssets())

	//the update is a success, the orphan is logged
	reduced := withoutAsset(t, stored, "Zähler")
	resp := do(t, router, "PUT", "/environments/env-1", "user-a", reduced)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 despite the failing deletion, got %d: %s", resp.Code, resp.Body.String())
	}
	after := store.stored["env-1"]
	if len(after.Zones[0].Zones[0].Assets) != 0 {
		t.Fatal("the document has to be written even though the cleanup failed")
	}

	//and so is the delete of the whole environment
	if code := do(t, router, "DELETE", "/environments/env-1", "user-a", nil).Code; code != http.StatusNoContent {
		t.Fatalf("expected 204 despite the failing deletion, got %d", code)
	}
	if len(store.stored) != 0 {
		t.Fatal("the environment has to be gone even though the cleanup failed")
	}
	if len(catalog.deleted) != 2 {
		t.Fatalf("expected both deletions to have been attempted, got %v", catalog.deleted)
	}
}

// A failed write must delete nothing: the stored document still names every
// device, and deleting one would leave an asset publishing into nowhere.
func TestAFailedWriteDeletesNoDevice(t *testing.T) {
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	stored := putEnvironment(t, router, "env-1", "user-a", environmentWithTwoAssets())

	store.failing = errors.New("database is down")
	if code := do(t, router, "PUT", "/environments/env-1", "user-a", withoutAsset(t, stored, "Zähler")).Code; code < 400 {
		t.Fatalf("expected the write to fail, got %d", code)
	}
	if len(catalog.deleted) != 0 {
		t.Fatalf("a document that was not written keeps its devices, deleted %v", catalog.deleted)
	}
}

// A copy of a document does not own the devices it names. Copying an environment
// to a new id and deleting the copy would otherwise delete the devices the
// original still publishes through.
func TestACopiedDocumentDoesNotOwnTheDevicesItNames(t *testing.T) {
	catalog := &fakeCatalog{idsByName: namedDeviceIds()}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	original := putEnvironment(t, router, "env-1", "user-a", environmentWithTwoAssets())
	copied := putEnvironment(t, router, "env-2", "user-a", original)
	forEachAsset(&copied, func(asset *domain.Asset) {
		if asset.ExternalManaged {
			t.Fatalf("a copy must not claim the devices of the original, got %+v", *asset)
		}
	})

	if code := do(t, router, "DELETE", "/environments/env-2", "user-a", nil).Code; code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", code)
	}
	if len(catalog.deleted) != 0 {
		t.Fatalf("deleting the copy must leave the devices of the original, deleted %v", catalog.deleted)
	}
	//the original is untouched and still owns its devices
	if !assetNamed(t, store.stored["env-1"], "Zähler").ExternalManaged {
		t.Error("the original has to keep its claim")
	}
}

// Two assets on one device: removing one of them must not delete the device the
// other still publishes through. Ownership is per document, and the reference is
// what decides.
func TestADeviceStillReferencedByAnotherAssetIsKept(t *testing.T) {
	catalog := &fakeCatalog{}
	store := newFakeEnvironments()
	router := testRouterWithCatalog(store, catalog)

	env := environmentWithTwoAssets()
	env.Zones[0].Assets[0].ExternalRef = "urn:device:shared"
	env.Zones[0].Assets[0].ExternalManaged = true
	env.Zones[0].Zones[0].Assets[0].ExternalRef = "urn:device:shared"
	stored := storeDirectly(t, store, "env-1", "user-a", env)

	putEnvironment(t, router, "env-1", "user-a", withoutAsset(t, stored, "Kompressor 1"))

	if len(catalog.deleted) != 0 {
		t.Fatalf("a device another asset still names must not be deleted, got %v", catalog.deleted)
	}
}

// ---------------------------------------------------------------------------
// reconcileManagedFlags directly, for the states a request cannot reach:
// validation rejects a document with duplicate ids, so an old document carrying
// them can only come from the store - from before that rule, or from the legacy
// migration.
// ---------------------------------------------------------------------------

func TestAnAmbiguousStoredAssetIdInheritsNothing(t *testing.T) {
	existing := domain.Environment{Zones: []domain.Zone{{
		Assets: []domain.Asset{
			{Id: "asset-1", ExternalRef: "urn:device:a", ExternalManaged: true},
			{Id: "asset-1", ExternalRef: "urn:device:b", ExternalManaged: true},
		},
	}}}
	env := domain.Environment{Zones: []domain.Zone{{
		Assets: []domain.Asset{{Id: "asset-1", ExternalRef: "urn:device:a"}},
	}}}

	reconcileManagedFlags(&existing, &env)
	if env.Zones[0].Assets[0].ExternalManaged {
		t.Error("which of the two the asset continues is unknowable, so nothing may be inherited")
	}
	//the devices of the stored document are still released, that part does not
	//depend on the ambiguity: one of them is no longer referenced
	orphans := orphanedDevices(&existing, &env)
	if len(orphans) != 1 || orphans[0].deviceId != "urn:device:b" {
		t.Errorf("expected only the unreferenced device to be released, got %+v", orphans)
	}
}

// An asset that is new to the document inherits nothing, whatever id it carries:
// a request could name the id of an asset that never existed.
func TestAnUnknownAssetIdInheritsNothing(t *testing.T) {
	existing := domain.Environment{Zones: []domain.Zone{{
		Assets: []domain.Asset{{Id: "asset-1", ExternalRef: "urn:device:a", ExternalManaged: true}},
	}}}
	env := domain.Environment{Zones: []domain.Zone{{
		Assets: []domain.Asset{{Id: "asset-2", ExternalRef: "urn:device:a", ExternalManaged: true}},
	}}}

	reconcileManagedFlags(&existing, &env)
	if env.Zones[0].Assets[0].ExternalManaged {
		t.Error("a device is managed for the asset it was provisioned for, not for whoever names it")
	}
	//it is still referenced, so it is not released either
	if orphans := orphanedDevices(&existing, &env); len(orphans) != 0 {
		t.Errorf("a device the new document still names must be kept, got %+v", orphans)
	}
}

// An asset without an id cannot be matched against anything: it must not fall
// together with a stored asset that has none either.
func TestAssetsWithoutAnIdAreNeverMatched(t *testing.T) {
	existing := domain.Environment{Zones: []domain.Zone{{
		Assets: []domain.Asset{{Id: "", ExternalRef: "urn:device:a", ExternalManaged: true}},
	}}}
	env := domain.Environment{Zones: []domain.Zone{{
		Assets: []domain.Asset{{Id: "", ExternalRef: "urn:device:a"}},
	}}}

	reconcileManagedFlags(&existing, &env)
	if env.Zones[0].Assets[0].ExternalManaged {
		t.Error("without an id there is nothing to match on")
	}
}

// The walk has to reach every asset, at every depth: an asset in a sub-zone that
// is not visited keeps a device forever, and one that is not reconciled keeps a
// flag the client set.
func TestTheWalkReachesEveryAssetAtEveryDepth(t *testing.T) {
	env := domain.Environment{Zones: []domain.Zone{{
		Assets: []domain.Asset{{Id: "a"}},
		Zones: []domain.Zone{{
			Assets: []domain.Asset{{Id: "b"}},
			Zones:  []domain.Zone{{Assets: []domain.Asset{{Id: "c"}, {Id: "d"}}}},
		}},
	}, {
		Assets: []domain.Asset{{Id: "e"}},
	}}}

	seen := map[string]bool{}
	forEachAsset(&env, func(asset *domain.Asset) { seen[asset.Id] = true })
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !seen[id] {
			t.Errorf("the asset %q was not visited", id)
		}
	}
	if len(seen) != 5 {
		t.Errorf("expected five assets, got %v", seen)
	}

	//and the visit is by reference, or nothing written there would be stored
	forEachAsset(&env, func(asset *domain.Asset) { asset.ExternalRef = "touched" })
	if env.Zones[0].Zones[0].Zones[0].Assets[1].ExternalRef != "touched" {
		t.Error("the walk has to hand out the asset itself, not a copy")
	}
}
