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

package repo

import (
	"errors"
	"testing"
)

func TestSharesRoundTripAndReplace(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	shares := store.Shares()

	set := ShareSet{EnvironmentId: "env-1", Users: []string{"demo-user"}, Groups: []string{"/demo"}}
	version, err := shares.Save(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("a set that is created starts at version 1, got %d", version)
	}
	stored, err := shares.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 1 {
		t.Errorf("the version has to be handed out with the set, got %d", stored.Version)
	}
	if len(stored.Users) != 1 || stored.Users[0] != "demo-user" || len(stored.Groups) != 1 || stored.Groups[0] != "/demo" {
		t.Errorf("the set did not survive: %+v", stored)
	}
	//set by the store, not by the caller
	if stored.UpdatedAtUnix == 0 {
		t.Error("expected the store to stamp the write")
	}

	//a second write replaces rather than adding a document, or Load would pick
	//an arbitrary one of the copies
	set.Users = []string{"other-user"}
	set.Version = stored.Version
	if version, err = shares.Save(ctx, set); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Errorf("every write counts the version up, got %d", version)
	}
	stored, err = shares.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Users) != 1 || stored.Users[0] != "other-user" {
		t.Errorf("the write has to replace the set, got %+v", stored)
	}
}

// Two shares of one environment arriving together would otherwise each store
// their own set, and the rights of the loser would stand on the devices with
// nothing that remembers them.
func TestSavingASetAgainstAnOutdatedVersionIsRefused(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	shares := store.Shares()

	//both read the same nothing, and only one may create it
	first, err := shares.Save(ctx, ShareSet{EnvironmentId: "env-1", Users: []string{"demo-user"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = shares.Save(ctx, ShareSet{EnvironmentId: "env-1", Users: []string{"other-user"}})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected a version conflict on the second create, got %v", err)
	}

	//and the same once it exists
	_, err = shares.Save(ctx, ShareSet{EnvironmentId: "env-1", Users: []string{"third-user"}, Version: first + 7})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected a version conflict against an outdated version, got %v", err)
	}
	conflict := &VersionConflictError{}
	if errors.As(err, &conflict) && conflict.Stored != first {
		t.Errorf("the conflict has to name the stored version, got %+v", conflict)
	}

	stored, err := shares.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Users) != 1 || stored.Users[0] != "demo-user" {
		t.Errorf("a refused write must change nothing, got %+v", stored)
	}
}

// The set of an environment that was deleted while a share was being applied is
// not silently recreated: the caller has to learn that it is gone.
func TestSavingASetThatWasDeletedInBetweenIsRefused(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	shares := store.Shares()

	version, err := shares.Save(ctx, ShareSet{EnvironmentId: "env-1", Users: []string{"demo-user"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = shares.Delete(ctx, "env-1"); err != nil {
		t.Fatal(err)
	}
	_, err = shares.Save(ctx, ShareSet{EnvironmentId: "env-1", Users: []string{"demo-user"}, Version: version})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected a version conflict, got %v", err)
	}
	conflict := &VersionConflictError{}
	if errors.As(err, &conflict) && !conflict.Gone {
		t.Errorf("the conflict has to say that it is gone, got %+v", conflict)
	}
}

// A caller reads the lists without checking for nil first, and an environment
// nobody shared is the ordinary case.
func TestSharesOfAnUnknownEnvironmentAreEmptyAndNotAnError(t *testing.T) {
	store := testStore(t)
	stored, err := store.Shares().Load(testContext(t), "env-unknown")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Empty() || stored.Users == nil || stored.Groups == nil {
		t.Errorf("expected an empty set with initialised lists, got %+v", stored)
	}
	if stored.EnvironmentId != "env-unknown" {
		t.Errorf("expected the id to be carried, got %q", stored.EnvironmentId)
	}
}

func TestSavingASetWithoutAnEnvironmentIdIsRefused(t *testing.T) {
	store := testStore(t)
	_, err := store.Shares().Save(testContext(t), ShareSet{Users: []string{"demo-user"}})
	if !errors.Is(err, ErrMissingId) {
		t.Errorf("expected ErrMissingId, got %v", err)
	}
}

func TestDeletingASetToleratesAMissingId(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	if err := store.Shares().Delete(ctx, "env-unknown"); err != nil {
		t.Errorf("deleting what is not there is not an error, got %v", err)
	}
}

// An id that is used again must not come back shared with the accounts of the
// environment that is gone.
func TestDeletingAnEnvironmentDeletesItsShareSet(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	if _, err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "user-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Shares().Save(ctx, ShareSet{EnvironmentId: "env-1", Users: []string{"demo-user"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "env-1"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Shares().Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Empty() {
		t.Errorf("the set has to go with the definition, got %+v", stored)
	}
}
