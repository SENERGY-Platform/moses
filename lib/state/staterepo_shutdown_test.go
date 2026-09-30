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

package state

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// assertNothingRuns checks under the lock that no change routine is running or indexed.
func assertNothingRuns(t *testing.T, repo *StateRepo) {
	t.Helper()
	repo.mux.RLock()
	defer repo.mux.RUnlock()
	if len(repo.stopChannels) != 0 || len(repo.changeRoutinesTickers) != 0 {
		t.Fatalf("%d change routines run after the shutdown", len(repo.stopChannels))
	}
	if repo.changeRoutineIndex != nil {
		t.Fatal("the routines were indexed again after the shutdown")
	}
}

// A shutdown that arrives while an update waits in Stop for a running routine
// must neither stop the same routines a second time, which blocks forever on
// the stop channel, nor leave the routines the update restarts running.
func TestShutdownDuringAnUpdateStopsEverything(t *testing.T) {
	store := newWorldStore()
	repo := crudTestRepo(t, store)
	repo.Config.JsTimeout = time.Minute
	gate := newScriptGate(t)
	//taken first: DevGetWorld waits for the world's mutex, which the routine holds from its first tick on
	snapshot, _, err := repo.DevGetWorld("w")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Name = "renamed"
	if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 1, Code: fmt.Sprintf(`httpGet(%q);`, gate.url)}); err != nil {
		t.Fatal(err)
	}
	gate.waitEntered(t)

	updateDone := make(chan error, 1)
	update := goSpawn(func() { updateDone <- repo.DevUpdateWorld(snapshot) })
	waitInStack(t, update, "into Stop", inFrames(".(*StateRepo).Stop("))
	shutdownDone := make(chan error, 1)
	shutdown := goSpawn(func() {
		repo.Shutdown()
		shutdownDone <- nil
	})
	//the fixed Shutdown waits for the lock, the old one went straight into Stop
	waitInStack(t, shutdown, "to the lock or into Stop", func(stack string) bool {
		return strings.Contains(stack, ".(*StateRepo).Shutdown(") &&
			(strings.Contains(stack, "sync.(*RWMutex).Lock(") || strings.Contains(stack, ".(*StateRepo).Stop("))
	})
	gate.open()
	if err := awaitResult(t, updateDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitResult(t, shutdownDone); err != nil {
		t.Fatal(err)
	}
	assertNothingRuns(t, repo)
	if world, _ := store.stored(t, "w"); world.Name != "renamed" {
		t.Fatalf("the update that held the lock was not stored, the stored name is %q", world.Name)
	}
}

// After Shutdown every world change is refused before it stops, stores or
// starts anything, so no routine outlives the service.
func TestAChangeAfterShutdownStartsNothing(t *testing.T) {
	cases := []struct {
		name   string
		change func(repo *StateRepo, snapshot WorldMsg) error
	}{
		{"dev update world", func(repo *StateRepo, snapshot WorldMsg) error {
			return repo.DevUpdateWorld(snapshot)
		}},
		{"dev update room", func(repo *StateRepo, snapshot WorldMsg) error {
			return repo.DevUpdateRoom("w", snapshot.Rooms["r"])
		}},
		{"dev update device", func(repo *StateRepo, snapshot WorldMsg) error {
			return repo.DevUpdateDevice("w", "r", snapshot.Rooms["r"].Devices["d"])
		}},
		{"dev delete world", func(repo *StateRepo, snapshot WorldMsg) error {
			return repo.DevDeleteWorld("w")
		}},
		{"create change routine", func(repo *StateRepo, snapshot WorldMsg) error {
			_, _, _, err := repo.CreateChangeRoutine(crudToken, CreateChangeRoutineRequest{RefType: "world", RefId: "w", Interval: 1, Code: "var x = 1;"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newWorldStore()
			repo := crudTestRepo(t, store)
			if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 1, Code: "var x = 1;"}); err != nil {
				t.Fatal(err)
			}
			snapshot, _, err := repo.DevGetWorld("w")
			if err != nil {
				t.Fatal(err)
			}
			writes := len(store.history())
			repo.Shutdown()
			if err := tc.change(repo, snapshot); !errors.Is(err, errShutDown) {
				t.Fatalf("expected the shutdown error, got %v", err)
			}
			assertNothingRuns(t, repo)
			if later := store.history()[writes:]; len(later) != 0 {
				t.Fatalf("%d writes reached the store after the shutdown", len(later))
			}
		})
	}
}

// DevUpdateDevice used to create a missing world map without the lock, a data
// race with every reader of this.Worlds; only the race detector sees it.
func TestDevUpdateDeviceWithoutWorldsDoesNotRace(t *testing.T) {
	repo := &StateRepo{Persistence: &crudPersistence{}, StateLogger: &recordingConnectionLog{}}
	release := make(chan struct{})
	readerDone := make(chan error, 1)
	//the read must come first and nothing but the lock may order it before the write
	reader := goSpawn(func() {
		_, err := repo.ReadWorlds(crudToken)
		<-release
		readerDone <- err
	})
	waitInStack(t, reader, "past its read", func(stack string) bool {
		return !strings.Contains(stack, ".(*StateRepo).ReadWorlds(") && strings.Contains(stack, "chan receive")
	})
	updateDone := make(chan error, 1)
	go func() { updateDone <- repo.DevUpdateDevice("w", "r", DeviceMsg{Id: "d"}) }()
	if err := awaitResult(t, updateDone); err == nil || err.Error() != "unknown world id" {
		t.Fatalf("expected the unknown world error, got %v", err)
	}
	close(release)
	if err := awaitResult(t, readerDone); err != nil {
		t.Fatal(err)
	}
}
