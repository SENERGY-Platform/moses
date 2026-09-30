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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// storeWrite is one write a worldStore was asked to make.
type storeWrite struct {
	world   World
	deleted bool
	failed  bool
}

// worldStore keeps the last stored version of every world as the database
// would, serialised at write time, and records every write attempt in order.
type worldStore struct {
	crudPersistence
	storeMux sync.Mutex
	worlds   map[string][]byte
	log      []storeWrite
	fail     atomic.Bool
	writes   chan storeWrite
}

func newWorldStore() *worldStore {
	return &worldStore{worlds: map[string][]byte{}, writes: make(chan storeWrite, 1024)}
}

// report is called with storeMux held, so the log order is the order the store applied the writes in.
func (this *worldStore) report(write storeWrite) {
	this.log = append(this.log, write)
	select {
	case this.writes <- write:
	default:
	}
}

func (this *worldStore) history() []storeWrite {
	this.storeMux.Lock()
	defer this.storeMux.Unlock()
	return append([]storeWrite{}, this.log...)
}

func (this *worldStore) PersistWorld(world World) error {
	encoded, err := json.Marshal(world)
	if err != nil {
		return err
	}
	var copied World
	if err = json.Unmarshal(encoded, &copied); err != nil {
		return err
	}
	this.storeMux.Lock()
	defer this.storeMux.Unlock()
	if this.fail.Load() {
		this.report(storeWrite{world: copied, failed: true})
		return errPersistFailed
	}
	this.worlds[world.Id] = encoded
	this.report(storeWrite{world: copied})
	return nil
}

func (this *worldStore) DeleteWorld(id string) error {
	this.storeMux.Lock()
	defer this.storeMux.Unlock()
	if this.fail.Load() {
		this.report(storeWrite{world: World{Id: id}, deleted: true, failed: true})
		return errPersistFailed
	}
	delete(this.worlds, id)
	this.report(storeWrite{world: World{Id: id}, deleted: true})
	return nil
}

func (this *worldStore) stored(t *testing.T, id string) (world World, exists bool) {
	t.Helper()
	this.storeMux.Lock()
	encoded, exists := this.worlds[id]
	this.storeMux.Unlock()
	if !exists {
		return world, false
	}
	if err := json.Unmarshal(encoded, &world); err != nil {
		t.Fatal(err)
	}
	return world, true
}

// waitForWrite reports whether a write matching match arrives within timeout.
func (this *worldStore) waitForWrite(timeout time.Duration, match func(storeWrite) bool) bool {
	deadline := time.After(timeout)
	for {
		select {
		case write := <-this.writes:
			if match(write) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// scriptGate is an endpoint a script calls with httpGet; each call reports on
// entered and blocks until open is called, which holds the run mid-script.
type scriptGate struct {
	url     string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newScriptGate(t *testing.T) *scriptGate {
	t.Helper()
	gate := &scriptGate{entered: make(chan struct{}, 16), release: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gate.entered <- struct{}{}:
		default:
		}
		<-gate.release
	}))
	//cleanups run last in first, so the gate opens before Close waits for its handlers
	t.Cleanup(server.Close)
	t.Cleanup(gate.open)
	gate.url = server.URL
	return gate
}

func (this *scriptGate) open() {
	this.once.Do(func() { close(this.release) })
}

func (this *scriptGate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-this.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the script never reached the gate")
	}
}

func awaitResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not return")
		return nil
	}
}

// goroutineID reads the calling goroutine's id from its stack header.
func goroutineID() int64 {
	buf := make([]byte, 64)
	fields := strings.Fields(string(buf[:runtime.Stack(buf, false)]))
	id, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		panic(err)
	}
	return id
}

// goroutineStack returns the current stack of goroutine id, or "" once it has exited.
func goroutineStack(id int64) string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	prefix := fmt.Sprintf("goroutine %d [", id)
	for _, block := range strings.Split(string(buf), "\n\n") {
		if strings.HasPrefix(block, prefix) {
			return block
		}
	}
	return ""
}

// goSpawn runs f on a new goroutine and returns that goroutine's id, so a test
// can watch where exactly it blocks instead of sleeping.
func goSpawn(f func()) int64 {
	id := make(chan int64, 1)
	go func() {
		id <- goroutineID()
		f()
	}()
	return <-id
}

// waitInStack waits until goroutine id's stack satisfies at, and fails the
// test if it never does: the interleaving the test relies on was not reached.
func waitInStack(t *testing.T, id int64, what string, at func(stack string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !at(goroutineStack(id)) {
		if time.Now().After(deadline) {
			t.Fatalf("interleaving not reached: the goroutine never got %v; last stack:\n%v", what, goroutineStack(id))
		}
		time.Sleep(time.Millisecond)
	}
}

func inFrames(frames ...string) func(stack string) bool {
	return func(stack string) bool {
		for _, frame := range frames {
			if !strings.Contains(stack, frame) {
				return false
			}
		}
		return true
	}
}

// assertStaleWriteFirst checks that the stale run did write and that every such
// write was applied before the update's: the order the fixed code enforces.
func assertStaleWriteFirst(t *testing.T, history []storeWrite, stale func(storeWrite) bool, applied func(storeWrite) bool) {
	t.Helper()
	appliedAt, staleAt := -1, []int{}
	for i, write := range history {
		if write.failed {
			continue
		}
		if appliedAt < 0 && applied(write) {
			appliedAt = i
		}
		if stale(write) {
			staleAt = append(staleAt, i)
		}
	}
	if appliedAt < 0 {
		t.Fatal("the update was never stored")
	}
	if len(staleAt) == 0 {
		t.Fatal("interleaving not reached: the stale run never stored its world")
	}
	for _, i := range staleAt {
		if i > appliedAt {
			t.Fatalf("the stale run stored the old world (write %d) after the update (write %d)", i, appliedAt)
		}
	}
}

// A change routine run that is in flight when a world update starts holds the
// old world and persists it through state.set when it ends. The stored world
// must still be the updated one afterwards.
func TestAStaleRoutineRunDoesNotOverwriteAWorldUpdate(t *testing.T) {
	cases := []struct {
		name string
		// update runs the api change on a world snapshot taken before the routine ticked
		update func(repo *StateRepo, snapshot WorldMsg) error
		// applied tells the update's write apart from the stale run's
		applied func(write storeWrite) bool
		check   func(t *testing.T, store *worldStore)
	}{
		{
			name: "delete routine",
			update: func(repo *StateRepo, snapshot WorldMsg) error {
				delete(snapshot.ChangeRoutines, "routine-world")
				return repo.DevUpdateWorld(snapshot)
			},
			applied: func(write storeWrite) bool {
				_, ok := write.world.ChangeRoutines["routine-world"]
				return !write.deleted && !ok
			},
			check: func(t *testing.T, store *worldStore) {
				world, exists := store.stored(t, "w")
				if !exists {
					t.Fatal("the world is gone from the store")
				}
				if _, ok := world.ChangeRoutines["routine-world"]; ok {
					t.Fatal("the deleted routine is back in the store")
				}
			},
		},
		{
			name: "update routine",
			update: func(repo *StateRepo, snapshot WorldMsg) error {
				snapshot.ChangeRoutines["routine-world"] = ChangeRoutine{Id: "routine-world", Code: "updated"}
				return repo.DevUpdateWorld(snapshot)
			},
			applied: func(write storeWrite) bool {
				return !write.deleted && write.world.ChangeRoutines["routine-world"].Code == "updated"
			},
			check: func(t *testing.T, store *worldStore) {
				world, exists := store.stored(t, "w")
				if !exists {
					t.Fatal("the world is gone from the store")
				}
				if code := world.ChangeRoutines["routine-world"].Code; code != "updated" {
					t.Fatalf("the stored routine code is %q, the update is lost", code)
				}
			},
		},
		{
			name: "create room",
			update: func(repo *StateRepo, snapshot WorldMsg) error {
				snapshot.Rooms["new"] = RoomMsg{Id: "new", Name: "new room"}
				return repo.DevUpdateWorld(snapshot)
			},
			applied: func(write storeWrite) bool {
				_, ok := write.world.Rooms["new"]
				return !write.deleted && ok
			},
			check: func(t *testing.T, store *worldStore) {
				world, exists := store.stored(t, "w")
				if !exists {
					t.Fatal("the world is gone from the store")
				}
				if _, ok := world.Rooms["new"]; !ok {
					t.Fatal("the created room vanished from the store")
				}
			},
		},
		{
			name: "delete world",
			update: func(repo *StateRepo, snapshot WorldMsg) error {
				return repo.DevDeleteWorld("w")
			},
			applied: func(write storeWrite) bool {
				return write.deleted
			},
			check: func(t *testing.T, store *worldStore) {
				if _, exists := store.stored(t, "w"); exists {
					t.Fatal("the deleted world is back in the store")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newWorldStore()
			repo := crudTestRepo(t, store)
			//the run is held at the gate far longer than the default timeout
			repo.Config.JsTimeout = time.Minute
			gate := newScriptGate(t)

			snapshot, _, err := repo.DevGetWorld("w")
			if err != nil {
				t.Fatal(err)
			}
			code := fmt.Sprintf(`httpGet(%q); moses.world.state.set("late", 1);`, gate.url)
			if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 1, Code: code}); err != nil {
				t.Fatal(err)
			}
			gate.waitEntered(t)

			done := make(chan error, 1)
			update := goSpawn(func() { done <- tc.update(repo, snapshot) })
			//Stop cannot return while the run is held, so the update is pinned there:
			//the old code has stored the new world by then, the fixed code has not yet
			waitInStack(t, update, "into Stop", inFrames(".(*StateRepo).Stop("))
			gate.open()
			if err := awaitResult(t, done); err != nil {
				t.Fatal(err)
			}
			assertStaleWriteFirst(t, store.history(), func(write storeWrite) bool {
				return !write.deleted && write.world.States["late"] != nil
			}, tc.applied)
			tc.check(t, store)
		})
	}
}

// A service command run that starts between the snapshot DevUpdateRoom and
// DevUpdateDevice take and the moment they store the world holds the old world
// too, and must not overwrite the update either.
func TestAStaleServiceRunDoesNotOverwriteARoomOrDeviceUpdate(t *testing.T) {
	cases := []struct {
		name string
		// frame is the update's own function, where it waits for the write lock after its snapshot
		frame    string
		update   func(repo *StateRepo, snapshot WorldMsg) error
		routines func(world World) map[string]ChangeRoutine
	}{
		{
			name:  "room",
			frame: ".(*StateRepo).DevUpdateRoom(",
			update: func(repo *StateRepo, snapshot WorldMsg) error {
				room := snapshot.Rooms["r"]
				room.ChangeRoutines["added"] = ChangeRoutine{Id: "added"}
				return repo.DevUpdateRoom("w", room)
			},
			routines: func(world World) map[string]ChangeRoutine {
				if world.Rooms["r"] == nil {
					return nil
				}
				return world.Rooms["r"].ChangeRoutines
			},
		},
		{
			name:  "device",
			frame: ".(*StateRepo).DevUpdateDevice(",
			update: func(repo *StateRepo, snapshot WorldMsg) error {
				device := snapshot.Rooms["r"].Devices["d"]
				device.ChangeRoutines["added"] = ChangeRoutine{Id: "added"}
				return repo.DevUpdateDevice("w", "r", device)
			},
			routines: func(world World) map[string]ChangeRoutine {
				if world.Rooms["r"] == nil || world.Rooms["r"].Devices["d"] == nil {
					return nil
				}
				return world.Rooms["r"].Devices["d"].ChangeRoutines
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newWorldStore()
			repo := crudTestRepo(t, store)
			repo.Config.JsTimeout = time.Minute
			hold := newScriptGate(t)
			late := newScriptGate(t)

			setup, _, err := repo.DevGetWorld("w")
			if err != nil {
				t.Fatal(err)
			}
			device := setup.Rooms["r"].Devices["d"]
			device.Services = map[string]Service{
				"hold": {Id: "hold", Code: fmt.Sprintf(`httpGet(%q);`, hold.url)},
				"late": {Id: "late", Code: fmt.Sprintf(`httpGet(%q); moses.device.state.set("late", 1);`, late.url)},
			}
			if err := repo.DevUpdateDevice("w", "r", device); err != nil {
				t.Fatal(err)
			}
			snapshot, _, err := repo.DevGetWorld("w")
			if err != nil {
				t.Fatal(err)
			}

			//a first run holds the read lock, so the update queues for the write lock
			holdDone := make(chan error, 1)
			go func() { _, err := repo.RunService("hold", nil); holdDone <- err }()
			hold.waitEntered(t)
			done := make(chan error, 1)
			update := goSpawn(func() { done <- tc.update(repo, snapshot) })
			waitInStack(t, update, "to the snapshot's write lock", inFrames(".(*StateRepo).DevGetWorld(", "sync.(*RWMutex).Lock("))
			//a reader queued behind a pending writer is admitted when that writer unlocks,
			//so the late run starts right after the snapshot and before the update locks again
			lateDone := make(chan error, 1)
			lateRun := goSpawn(func() { _, err := repo.RunService("late", nil); lateDone <- err })
			waitInStack(t, lateRun, "to the read lock", inFrames(".(*StateRepo).RunService(", "sync.(*RWMutex).RLock("))
			hold.open()
			late.waitEntered(t)
			//the late run holds the read lock, so the update waits here for the swap: the
			//old code has stored the new world by then, the fixed code has not yet
			waitInStack(t, update, "to the swap's write lock", func(stack string) bool {
				return inFrames(tc.frame, "sync.(*RWMutex).Lock(")(stack) && !strings.Contains(stack, ".(*StateRepo).DevGetWorld(")
			})
			late.open()
			for _, ch := range []<-chan error{done, holdDone, lateDone} {
				if err := awaitResult(t, ch); err != nil {
					t.Fatal(err)
				}
			}
			assertStaleWriteFirst(t, store.history(), func(write storeWrite) bool {
				_, updated := tc.routines(write.world)["added"]
				room := write.world.Rooms["r"]
				if room == nil || room.Devices["d"] == nil {
					return false
				}
				return !updated && room.Devices["d"].States["late"] != nil
			}, func(write storeWrite) bool {
				_, updated := tc.routines(write.world)["added"]
				return updated
			})
			world, exists := store.stored(t, "w")
			if !exists {
				t.Fatal("the world is gone from the store")
			}
			if _, ok := tc.routines(world)["added"]; !ok {
				t.Fatal("the stored world lost the update")
			}
		})
	}
}

// The routines are stopped before the update writes, so an update the store
// refuses must start them again from the unchanged worlds.
func TestAFailedWorldUpdateKeepsTheRoutinesRunning(t *testing.T) {
	cases := []struct {
		name   string
		update func(repo *StateRepo, snapshot WorldMsg) error
	}{
		{"world", func(repo *StateRepo, snapshot WorldMsg) error {
			snapshot.Name = "renamed"
			return repo.DevUpdateWorld(snapshot)
		}},
		{"room", func(repo *StateRepo, snapshot WorldMsg) error {
			return repo.DevUpdateRoom("w", snapshot.Rooms["r"])
		}},
		{"device", func(repo *StateRepo, snapshot WorldMsg) error {
			return repo.DevUpdateDevice("w", "r", snapshot.Rooms["r"].Devices["d"])
		}},
		{"delete world", func(repo *StateRepo, snapshot WorldMsg) error {
			return repo.DevDeleteWorld("w")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newWorldStore()
			repo := crudTestRepo(t, store)
			if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 1, Code: `moses.world.state.set("ran", 1);`}); err != nil {
				t.Fatal(err)
			}
			snapshot, _, err := repo.DevGetWorld("w")
			if err != nil {
				t.Fatal(err)
			}
			store.fail.Store(true)
			if err := tc.update(repo, snapshot); !errors.Is(err, errPersistFailed) {
				t.Fatalf("expected the store error, got %v", err)
			}
			if _, indexed := repo.getChangeRoutineFromIndex("routine-world"); !indexed {
				t.Fatal("the routine index was not rebuilt after the failed update")
			}
			//the update reported its own write before returning; every later one is a run
			for drained := false; !drained; {
				select {
				case <-store.writes:
				default:
					drained = true
				}
			}
			ran := store.waitForWrite(3*time.Second, func(write storeWrite) bool {
				return write.failed && !write.deleted && write.world.States["ran"] != nil
			})
			if !ran {
				t.Fatal("the routine stopped running after the failed update")
			}
		})
	}
}
