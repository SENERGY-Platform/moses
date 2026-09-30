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
	"sync"
	"testing"
	"time"
)

// blockingState is a state value whose first json encoding blocks until
// release, which holds an update inside its world conversion; every later
// encoding returns at once.
type blockingState struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	opened  sync.Once
}

func newBlockingState(t *testing.T) *blockingState {
	t.Helper()
	state := &blockingState{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(state.open)
	return state
}

func (this *blockingState) MarshalJSON() ([]byte, error) {
	this.once.Do(func() {
		close(this.entered)
		<-this.release
	})
	return []byte(`"pinned"`), nil
}

func (this *blockingState) open() {
	this.opened.Do(func() { close(this.release) })
}

func (this *blockingState) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-this.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the update never reached its world conversion")
	}
}

// crudResult keeps the flags of a crud call, so a denied or missing target fails the test instead of passing silently.
func crudResult(access bool, exists bool, err error) error {
	if err != nil {
		return err
	}
	if !access || !exists {
		return fmt.Errorf("access=%v exists=%v", access, exists)
	}
	return nil
}

// checkBoth checks that a world holds the changes of both edits.
type checkBoth func(world World) error

func hasRoutine(routines map[string]ChangeRoutine, code string) bool {
	for _, routine := range routines {
		if routine.Code == code {
			return true
		}
	}
	return false
}

func assertInStoreAndMemory(t *testing.T, repo *StateRepo, store *worldStore, check checkBoth) {
	t.Helper()
	stored, exists := store.stored(t, "w")
	if !exists {
		t.Fatal("the world is gone from the store")
	}
	if err := check(stored); err != nil {
		t.Errorf("stored world: %v", err)
	}
	running, exists, err := repo.DevGetWorld("w")
	if err != nil || !exists {
		t.Fatalf("running world: exists=%v err=%v", exists, err)
	}
	model, err := running.ToModel()
	if err != nil {
		t.Fatal(err)
	}
	if err := check(model); err != nil {
		t.Errorf("running world: %v", err)
	}
}

// Two api edits of one world run at the same time; the edit that is held
// after reading the world must not write its copy over the other's change.
// The held edit carries a state whose encoding blocks: the old code encoded
// outside the lock, between its read and its write, so the other edit
// completed in between and was overwritten.
func TestConcurrentEditsOfOneWorldKeepEachOther(t *testing.T) {
	cases := []struct {
		name string
		// held runs with pin among its states and is stopped inside its world conversion
		held func(repo *StateRepo, pin *blockingState) error
		// other runs while held is stopped there
		other func(repo *StateRepo) error
		check checkBoth
	}{
		{
			name: "world: create room and create world routine",
			held: func(repo *StateRepo, pin *blockingState) error {
				_, access, exists, err := repo.CreateRoom(crudToken, CreateRoomRequest{World: "w", Name: "created room", States: map[string]interface{}{"pin": pin}})
				return crudResult(access, exists, err)
			},
			other: func(repo *StateRepo) error {
				_, access, exists, err := repo.CreateChangeRoutine(crudToken, CreateChangeRoutineRequest{RefType: "world", RefId: "w", Code: "created routine"})
				return crudResult(access, exists, err)
			},
			check: func(world World) error {
				created := false
				for _, room := range world.Rooms {
					created = created || room.Name == "created room"
				}
				if !created {
					return errors.New("the created room is missing")
				}
				if !hasRoutine(world.ChangeRoutines, "created routine") {
					return errors.New("the created world routine is missing")
				}
				return nil
			},
		},
		{
			name: "room: update room and create device in it",
			held: func(repo *StateRepo, pin *blockingState) error {
				_, access, exists, err := repo.UpdateRoom(crudToken, UpdateRoomRequest{
					Id:             "r",
					Name:           "renamed room",
					States:         map[string]interface{}{"pin": pin},
					ChangeRoutines: map[string]ChangeRoutine{"routine-room": {Id: "routine-room", Code: "room code"}},
				})
				return crudResult(access, exists, err)
			},
			other: func(repo *StateRepo) error {
				_, access, exists, err := repo.CreateDevice(crudToken, CreateDeviceRequest{Room: "r", Name: "created device"})
				return crudResult(access, exists, err)
			},
			check: func(world World) error {
				room := world.Rooms["r"]
				if room == nil || room.Name != "renamed room" {
					return errors.New("the room update is missing")
				}
				created := false
				for _, device := range room.Devices {
					created = created || device.Name == "created device"
				}
				if !created {
					return errors.New("the created device is missing")
				}
				if room.Devices["d"] == nil {
					return errors.New("the existing device is missing")
				}
				return nil
			},
		},
		{
			name: "device: update device and create room routine",
			held: func(repo *StateRepo, pin *blockingState) error {
				_, access, exists, err := repo.UpdateDevice(crudToken, UpdateDeviceRequest{
					Id:             "d",
					Name:           "renamed device",
					ExternalRef:    "external-d",
					States:         map[string]interface{}{"pin": pin},
					ChangeRoutines: map[string]ChangeRoutine{"routine-device": {Id: "routine-device", Code: "device code"}},
				})
				return crudResult(access, exists, err)
			},
			other: func(repo *StateRepo) error {
				_, access, exists, err := repo.CreateChangeRoutine(crudToken, CreateChangeRoutineRequest{RefType: "room", RefId: "r", Code: "created routine"})
				return crudResult(access, exists, err)
			},
			check: func(world World) error {
				room := world.Rooms["r"]
				if room == nil || room.Devices["d"] == nil || room.Devices["d"].Name != "renamed device" {
					return errors.New("the device update is missing")
				}
				if !hasRoutine(room.ChangeRoutines, "created routine") {
					return errors.New("the created room routine is missing")
				}
				return nil
			},
		},
		{
			name: "dev api: update room and update device routine",
			held: func(repo *StateRepo, pin *blockingState) error {
				return repo.DevUpdateRoom("w", RoomMsg{
					Id:             "new",
					Name:           "dev room",
					States:         map[string]interface{}{"pin": pin},
					ChangeRoutines: map[string]ChangeRoutine{},
				})
			},
			other: func(repo *StateRepo) error {
				_, access, exists, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-device", Code: "updated routine"})
				return crudResult(access, exists, err)
			},
			check: func(world World) error {
				if world.Rooms["new"] == nil {
					return errors.New("the room added through the dev api is missing")
				}
				room := world.Rooms["r"]
				if room == nil || room.Devices["d"] == nil || room.Devices["d"].ChangeRoutines["routine-device"].Code != "updated routine" {
					return errors.New("the device routine update is missing")
				}
				return nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newWorldStore()
			repo := crudTestRepo(t, store)
			pin := newBlockingState(t)

			heldDone := make(chan error, 1)
			go func() { heldDone <- tc.held(repo, pin) }()
			pin.waitEntered(t)

			otherDone := make(chan error, 1)
			other := goSpawn(func() { otherDone <- tc.other(repo) })
			//the old code let the other edit finish here; the fixed one holds it at the lock the held edit keeps
			waitInStack(t, other, "to its end or to the write lock", func(stack string) bool {
				return stack == "" || strings.Contains(stack, "sync.(*RWMutex).Lock(")
			})
			pin.open()
			for _, done := range []<-chan error{heldDone, otherDone} {
				if err := awaitResult(t, done); err != nil {
					t.Fatal(err)
				}
			}
			assertInStoreAndMemory(t, repo, store, tc.check)
		})
	}
}

// Two api edits of one device both read it before either writes, which the
// old code allowed because the read took only the read lock. A change routine
// run holding the world's mutex keeps both edits waiting until both have begun.
func TestConcurrentEditsOfOneDeviceKeepEachOther(t *testing.T) {
	store := newWorldStore()
	repo := crudTestRepo(t, store)
	repo.Config.JsTimeout = time.Minute
	gate := newScriptGate(t)
	if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 1, Code: fmt.Sprintf(`httpGet(%q);`, gate.url)}); err != nil {
		t.Fatal(err)
	}
	gate.waitEntered(t)

	waitingForTheWorld := func(frame string) func(stack string) bool {
		//the old code waits for the world mutex; the fixed one waits in Stop for the run, or for the write lock behind the first edit
		return func(stack string) bool {
			return strings.Contains(stack, frame) &&
				(strings.Contains(stack, "sync.(*Mutex).Lock(") || strings.Contains(stack, ".(*StateRepo).Stop("))
		}
	}
	serviceDone := make(chan error, 1)
	service := goSpawn(func() {
		_, access, exists, err := repo.CreateService(crudToken, CreateServiceRequest{Device: "d", Name: "created service"})
		serviceDone <- crudResult(access, exists, err)
	})
	waitInStack(t, service, "to the world mutex", waitingForTheWorld(".(*StateRepo).CreateService("))
	routineDone := make(chan error, 1)
	routine := goSpawn(func() {
		_, access, exists, err := repo.CreateChangeRoutine(crudToken, CreateChangeRoutineRequest{RefType: "device", RefId: "d", Code: "created routine"})
		routineDone <- crudResult(access, exists, err)
	})
	waitInStack(t, routine, "to the world mutex or the write lock", waitingForTheWorld(".(*StateRepo).CreateChangeRoutine("))
	gate.open()
	for _, done := range []<-chan error{serviceDone, routineDone} {
		if err := awaitResult(t, done); err != nil {
			t.Fatal(err)
		}
	}
	assertInStoreAndMemory(t, repo, store, func(world World) error {
		device := world.Rooms["r"].Devices["d"]
		created := false
		for _, service := range device.Services {
			created = created || service.Name == "created service"
		}
		if !created {
			return errors.New("the created service is missing")
		}
		if !hasRoutine(device.ChangeRoutines, "created routine") {
			return errors.New("the created device routine is missing")
		}
		return nil
	})
}

// An edit holds the write lock, and with it every command and service run,
// while it waits for running scripts, and it must wait for them only once: the
// first fix read the world under the world mutex up to three times and then
// waited in Stop, and a busy routine took the mutex back in every gap. This
// bounds that scenario only; a routine that ticks just before Stop plus a run
// queued on the world can still hold an edit for about two script timeouts.
func TestAnEditWaitsForRunningScriptsOnce(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		edit  func(repo *StateRepo) error
	}{
		{"update service", ".(*StateRepo).UpdateService(", func(repo *StateRepo) error {
			_, access, exists, err := repo.UpdateService(crudToken, UpdateServiceRequest{Id: "target", Code: "var y = 2;"})
			return crudResult(access, exists, err)
		}},
		{"update device routine", ".(*StateRepo).UpdateChangeRoutine(", func(repo *StateRepo) error {
			_, access, exists, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-device", Code: "updated"})
			return crudResult(access, exists, err)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newWorldStore()
			repo := crudTestRepo(t, store)
			timeout := time.Second
			repo.Config.JsTimeout = timeout
			gate := newScriptGate(t)
			setup, _, err := repo.DevGetWorld("w")
			if err != nil {
				t.Fatal(err)
			}
			device := setup.Rooms["r"].Devices["d"]
			device.Services = map[string]Service{
				"probe":  {Id: "probe", Code: "var x = 1;"},
				"target": {Id: "target", Code: "var y = 1;"},
			}
			if err := repo.DevUpdateDevice("w", "r", device); err != nil {
				t.Fatal(err)
			}
			//every run keeps the world mutex until the interrupt ends it, and the next tick is due by then
			busy := fmt.Sprintf(`httpGet(%q); while (true) {}`, gate.url)
			if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 1, Code: busy}); err != nil {
				t.Fatal(err)
			}
			gate.waitEntered(t)
			gate.open()

			editDone := make(chan error, 1)
			edit := goSpawn(func() { editDone <- tc.edit(repo) })
			waitInStack(t, edit, "to a wait under the write lock", func(stack string) bool {
				return strings.Contains(stack, tc.frame) &&
					(strings.Contains(stack, "sync.(*Mutex).Lock(") || strings.Contains(stack, ".(*StateRepo).Stop("))
			})
			started := time.Now()
			if _, err := repo.RunService("probe", nil); err != nil {
				t.Fatal(err)
			}
			waited := time.Since(started)
			t.Logf("the service run waited %v behind the edit", waited)
			if err := awaitResult(t, editDone); err != nil {
				t.Fatal(err)
			}
			if limit := timeout * 3 / 2; waited > limit {
				t.Fatalf("a service run waited %v behind the edit, more than %v for a script timeout of %v", waited, limit, timeout)
			}
		})
	}
}

// A service run queued on its world and stuck in httpGet holds up an edit, and
// every command behind the edit, until its own budget from its start is spent,
// not a full timeout after it finally got the world.
func TestAQueuedRunInHttpGetHoldsUpAnEditForItsOwnBudget(t *testing.T) {
	store := newWorldStore()
	repo := crudTestRepo(t, store)
	timeout := time.Second
	repo.Config.JsTimeout = timeout
	hang := newScriptGate(t)
	setup, _, err := repo.DevGetWorld("w")
	if err != nil {
		t.Fatal(err)
	}
	device := setup.Rooms["r"].Devices["d"]
	device.Services = map[string]Service{
		"probe":  {Id: "probe", Code: "var x = 1;"},
		"hang":   {Id: "hang", Code: fmt.Sprintf(`httpGet(%q);`, hang.url)},
		"target": {Id: "target", Code: "var y = 1;"},
	}
	if err := repo.DevUpdateDevice("w", "r", device); err != nil {
		t.Fatal(err)
	}
	world := repo.Worlds["w"]
	//stands in for another run of the world, so the hanging run queues for it
	world.mux.Lock()
	started := time.Now()
	hangDone := make(chan error, 1)
	hanging := goSpawn(func() { _, err := repo.RunService("hang", nil); hangDone <- err })
	waitInStack(t, hanging, "to the world mutex", inFrames(".(*StateRepo).RunService(", "sync.(*Mutex).Lock("))
	editDone := make(chan error, 1)
	edit := goSpawn(func() {
		_, access, exists, err := repo.UpdateService(crudToken, UpdateServiceRequest{Id: "target", Code: "var y = 2;"})
		editDone <- crudResult(access, exists, err)
	})
	waitInStack(t, edit, "to the write lock", inFrames(".(*StateRepo).UpdateService(", "sync.(*RWMutex).Lock("))
	probeDone := make(chan time.Duration, 1)
	probe := goSpawn(func() {
		_, _ = repo.RunService("probe", nil)
		probeDone <- time.Since(started)
	})
	waitInStack(t, probe, "to the read lock behind the edit", inFrames(".(*StateRepo).RunService(", "sync.(*RWMutex).RLock("))
	//how long the other run holds the world, not an interleaving
	time.Sleep(time.Until(started.Add(timeout * 4 / 5)))
	world.mux.Unlock()
	var waited time.Duration
	select {
	case waited = <-probeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe did not return")
	}
	t.Logf("the probe returned %v after the hanging run started", waited)
	for _, done := range []<-chan error{hangDone, editDone} {
		if err := awaitResult(t, done); err != nil && err.Error() != "Some code took to long" {
			t.Fatal(err)
		}
	}
	if limit := timeout * 3 / 2; waited > limit {
		t.Fatalf("the probe returned %v after the hanging run started, more than %v for a script timeout of %v", waited, limit, timeout)
	}
}
