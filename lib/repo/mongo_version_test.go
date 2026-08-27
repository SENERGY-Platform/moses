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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// The compare-and-swap is what closes the gap described in
// docs/device-lifecycle-of-assets.md: of two concurrent updates, the loser's
// device cleanup could delete a device the winning document still publishes
// through. The check has to live here rather than in the handler, because a
// handler that reads, compares and then writes has the same race - only a
// shorter one.

func TestMongoPutIfVersionWritesWhileTheStoredVersionStillMatches(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	version, err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}

	changed := testEnvironment("env-1", "Metallbau (edited)", "owner-1")
	next, err := store.PutIfVersion(ctx, changed, version)
	if err != nil {
		t.Fatalf("a write against the current version must go through: %v", err)
	}
	if next != version+1 {
		t.Errorf("expected version %d after the write, got %d", version+1, next)
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Metallbau (edited)" || got.Version != next {
		t.Errorf("expected the edited document at version %d, got %q at %d", next, got.Name, got.Version)
	}
}

func TestMongoPutIfVersionRefusesTheSecondWriteOfTheSameVersion(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	read, err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}

	//two editors read version 1 and both write it back
	winner := testEnvironment("env-1", "written by the winner", "owner-1")
	if _, err = store.PutIfVersion(ctx, winner, read); err != nil {
		t.Fatalf("the first write must go through: %v", err)
	}
	loser := testEnvironment("env-1", "written by the loser", "owner-1")
	_, err = store.PutIfVersion(ctx, loser, read)

	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected an error wrapping ErrVersionConflict, got %v", err)
	}
	conflict := &VersionConflictError{}
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a *VersionConflictError, got %T", err)
	}
	if conflict.Expected != read || conflict.Stored != read+1 {
		t.Errorf("expected the conflict to name %d and %d, got %d and %d", read, read+1, conflict.Expected, conflict.Stored)
	}
	//both numbers in the message: a caller that cannot see how far behind it was
	//cannot tell a stale editor from a lost write
	if !strings.Contains(conflict.Error(), "version 1") || !strings.Contains(conflict.Error(), "stored is 2") {
		t.Errorf("expected both versions in the message, got %q", conflict.Error())
	}

	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "written by the winner" {
		t.Errorf("the refused write must not have landed, stored is %q", got.Name)
	}
	if got.Version != read+1 {
		t.Errorf("a refused write must not move the version, expected %d, got %d", read+1, got.Version)
	}
}

func TestMongoPutIfVersionNeitherCreatesNorResurrects(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)

	//a caller carrying a version read a document; recreating a deleted one under
	//that version would be the opposite of what it asked for
	_, err := store.PutIfVersion(ctx, testEnvironment("env-1", "Metallbau", "owner-1"), 1)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("expected a conflict for a document that does not exist, got %v", err)
	}
	conflict := &VersionConflictError{}
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a *VersionConflictError, got %v", err)
	}
	if !conflict.Gone {
		t.Error("expected a missing document to be reported as gone")
	}
	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("expected the message to say the document is gone, got %q", err.Error())
	}
	if count := countDocuments(t, store, "environments"); count != 0 {
		t.Errorf("expected nothing to be written, got %d documents", count)
	}
}

// A version of zero is what a client that knows nothing of the field sends, and
// the unchecked path is where it belongs. Accepting it here would silently drop
// the protection the caller asked for by calling this method at all.
func TestMongoPutIfVersionRefusesAVersionBelowOne(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	if _, err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1")); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int64{0, -1} {
		_, err := store.PutIfVersion(ctx, testEnvironment("env-1", "overwritten", "owner-1"), expected)
		if !errors.Is(err, ErrVersionConflict) {
			t.Errorf("expected expected-version %d to be refused, got %v", expected, err)
		}
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Metallbau" {
		t.Errorf("nothing may have been written, stored is %q", got.Name)
	}
}

// A document written before the version field existed carries no version at all.
// It has to read as 0, so that no client is asked to defend a number it never
// saw, and its first write has to start the count at 1.
func TestMongoADocumentWithoutAVersionFieldStartsAtOne(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	_, err := store.environmentCollection().InsertOne(ctx, bson.M{
		"id": "env-old", "name": "Metallbau", "owner": "owner-1", "zones": []interface{}{},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "env-old")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 0 {
		t.Errorf("expected a document without the field to read as version 0, got %d", got.Version)
	}

	//and a caller that carries a version against it is told that the stored one
	//is 0, not that the document disappeared: it is right there, it has simply
	//never been written since the field exists
	_, err = store.PutIfVersion(ctx, got, 1)
	conflict := &VersionConflictError{}
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a *VersionConflictError, got %v", err)
	}
	if conflict.Gone {
		t.Error("a stored document without a version field must not be reported as gone")
	}
	if !strings.Contains(conflict.Error(), "stored is 0") {
		t.Errorf("expected the message to name the stored version 0, got %q", conflict.Error())
	}

	version, err := store.Put(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("expected the first write of an old document to be version 1, got %d", version)
	}
}

// The version the caller hands in is never the version that is stored: an
// echoed, invented or replayed number must not decide what the next one is.
func TestMongoTheVersionInTheDocumentIsIgnored(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	env := testEnvironment("env-1", "Metallbau", "owner-1")
	env.Version = 4711
	version, err := store.Put(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("expected version 1, got %d", version)
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Errorf("expected the stored version to be 1, got %d", got.Version)
	}
}

// A value that looks like a mongodb field path must survive the write: the
// update is an aggregation pipeline, and without $literal the server would read
// "$name" as a reference to another field. Script code is full of "$".
func TestMongoAValueThatLooksLikeAFieldPathIsStoredAsIs(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	env := testEnvironment("env-1", "Metallbau", "owner-1")
	env.Context["dollar"] = "$name"
	env.Context["expression"] = map[string]interface{}{"$concat": "not an expression"}
	env.Zones[0].Assets[0].Channels[0].Source.Script.Code = `moses.service.send("$" + 1);`
	if _, err := store.Put(ctx, env); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Context["dollar"] != "$name" {
		t.Errorf("expected the value to survive verbatim, got %#v", got.Context["dollar"])
	}
	nested, ok := got.Context["expression"].(map[string]interface{})
	if !ok || nested["$concat"] != "not an expression" {
		t.Errorf("expected the nested value to survive verbatim, got %#v", got.Context["expression"])
	}
	if code := got.Zones[0].Assets[0].Channels[0].Source.Script.Code; code != `moses.service.send("$" + 1);` {
		t.Errorf("expected the script to survive verbatim, got %q", code)
	}
}

// The one that has to hold under -race and against a real database: many
// writers start from the same version, and exactly one of them may win.
func TestMongoOnlyOneOfManyWritesFromTheSameVersionWins(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	read, err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}

	const writers = 16
	won := atomic.Int64{}
	conflicts := atomic.Int64{}
	winner := atomic.Int64{}
	start := make(chan struct{})
	wg := sync.WaitGroup{}
	unexpected := make(chan error, writers)
	for run := 0; run < writers; run++ {
		wg.Add(1)
		go func(run int) {
			defer wg.Done()
			env := testEnvironment("env-1", "written by writer", "owner-1")
			env.Seed = int64(run)
			<-start
			version, err := store.PutIfVersion(ctx, env, read)
			switch {
			case err == nil:
				won.Add(1)
				winner.Store(int64(run))
				if version != read+1 {
					unexpected <- errWrongVersion(version, read+1)
				}
			case errors.Is(err, ErrVersionConflict):
				conflicts.Add(1)
			default:
				unexpected <- err
			}
		}(run)
	}
	close(start)
	wg.Wait()
	close(unexpected)
	for err := range unexpected {
		t.Errorf("unexpected error from a concurrent write: %v", err)
	}

	if won.Load() != 1 {
		t.Errorf("expected exactly one winner, got %d", won.Load())
	}
	if conflicts.Load() != writers-1 {
		t.Errorf("expected %d conflicts, got %d", writers-1, conflicts.Load())
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != read+1 {
		t.Errorf("expected the version to have moved exactly once, from %d to %d, got %d", read, read+1, got.Version)
	}
	//and the document is the winner's, whole: a losing write must not have left
	//a field of its own behind
	if got.Seed != winner.Load() {
		t.Errorf("expected the document of writer %d, got seed %d", winner.Load(), got.Seed)
	}
	if count := countDocuments(t, store, "environments"); count != 1 {
		t.Errorf("expected 1 document, got %d", count)
	}
}

func errWrongVersion(got int64, want int64) error {
	return fmt.Errorf("the winning write returned version %d, expected %d", got, want)
}
