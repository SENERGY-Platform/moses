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
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
)

var otherToken = sc_jwt.Token{Sub: "someone-else"}

// pathTestRepo is crudTestRepo with service "s" on device "d" and a ticker
// for routine-world whose interval never fires within a test.
func pathTestRepo(t *testing.T) (*StateRepo, *worldStore) {
	t.Helper()
	store := newWorldStore()
	repo := crudTestRepo(t, store)
	setup, _, err := repo.DevGetWorld("w")
	if err != nil {
		t.Fatal(err)
	}
	device := setup.Rooms["r"].Devices["d"]
	device.Services = map[string]Service{"s": {Id: "s", Name: "service", Code: "var x = 1;"}}
	if err := repo.DevUpdateDevice("w", "r", device); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "routine-world", Interval: 3600, Code: "world code"}); err != nil {
		t.Fatal(err)
	}
	return repo, store
}

// breakWorld gives world "w" a state json cannot encode, so every conversion of the whole world fails.
func breakWorld(repo *StateRepo) {
	world := repo.Worlds["w"]
	world.mux.Lock()
	defer world.mux.Unlock()
	world.States["broken"] = math.Inf(1)
}

// firstTicker returns the ticker of routine-world; Start replaces it, so a different pointer means the routines were restarted.
func firstTicker(t *testing.T, repo *StateRepo) *time.Ticker {
	t.Helper()
	repo.mux.RLock()
	defer repo.mux.RUnlock()
	if len(repo.changeRoutinesTickers) != 1 {
		t.Fatalf("expected the one ticker of routine-world, found %d", len(repo.changeRoutinesTickers))
	}
	return repo.changeRoutinesTickers[0]
}

// crudMutator is one api mutator reduced to the three values the api maps to a status.
type crudMutator struct {
	name string
	// worldLevel mutators read the whole world first, so a world that does not convert is an error before access is known
	worldLevel bool
	valid      string
	call       func(repo *StateRepo, token sc_jwt.Token, id string) (access bool, exists bool, err error)
}

var crudMutators = []crudMutator{
	{"UpdateWorld", true, "w", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.UpdateWorld(token, UpdateWorldRequest{Id: id, Name: "renamed", States: map[string]interface{}{}})
		return access, exists, err
	}},
	{"DeleteWorld", true, "w", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		return repo.DeleteWorld(token, id)
	}},
	{"CreateRoom", true, "w", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.CreateRoom(token, CreateRoomRequest{World: id, Name: "created"})
		return access, exists, err
	}},
	{"UpdateRoom", false, "r", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.UpdateRoom(token, UpdateRoomRequest{Id: id, Name: "renamed"})
		return access, exists, err
	}},
	{"DeleteRoom", false, "r", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.DeleteRoom(token, id)
		return access, exists, err
	}},
	{"CreateDevice", false, "r", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.CreateDevice(token, CreateDeviceRequest{Room: id, Name: "created"})
		return access, exists, err
	}},
	{"UpdateDevice", false, "d", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.UpdateDevice(token, UpdateDeviceRequest{Id: id, Name: "renamed", Services: map[string]Service{"s": {Id: "s", Code: "var x = 2;"}}})
		return access, exists, err
	}},
	{"DeleteDevice", false, "d", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.DeleteDevice(token, id)
		return access, exists, err
	}},
	{"CreateService", false, "d", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.CreateService(token, CreateServiceRequest{Device: id, Name: "created"})
		return access, exists, err
	}},
	{"UpdateService", false, "s", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.UpdateService(token, UpdateServiceRequest{Id: id, Code: "var x = 2;"})
		return access, exists, err
	}},
	{"DeleteService", false, "s", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.DeleteService(token, id)
		return access, exists, err
	}},
	{"CreateChangeRoutine world", true, "w", createRoutine("world")},
	{"CreateChangeRoutine room", false, "r", createRoutine("room")},
	{"CreateChangeRoutine device", false, "d", createRoutine("device")},
	{"UpdateChangeRoutine world", true, "routine-world", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.UpdateChangeRoutine(token, UpdateChangeRoutineRequest{Id: id, Code: "changed"})
		return access, exists, err
	}},
	{"UpdateChangeRoutine room", false, "routine-room", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.UpdateChangeRoutine(token, UpdateChangeRoutineRequest{Id: id, Code: "changed"})
		return access, exists, err
	}},
	{"UpdateChangeRoutine device", false, "routine-device", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.UpdateChangeRoutine(token, UpdateChangeRoutineRequest{Id: id, Code: "changed"})
		return access, exists, err
	}},
	{"DeleteChangeRoutine world", true, "routine-world", deleteRoutine},
	{"DeleteChangeRoutine room", false, "routine-room", deleteRoutine},
	{"DeleteChangeRoutine device", false, "routine-device", deleteRoutine},
}

func createRoutine(refType string) func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
	return func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
		_, access, exists, err := repo.CreateChangeRoutine(token, CreateChangeRoutineRequest{RefType: refType, RefId: id, Code: "created"})
		return access, exists, err
	}
}

func deleteRoutine(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
	_, access, exists, err := repo.DeleteChangeRoutine(token, id)
	return access, exists, err
}

type crudPath struct {
	name    string
	prepare func(repo *StateRepo, store *worldStore)
	token   sc_jwt.Token
	missing bool
	// expect returns the access and exists flags the api sees and checks the error
	expect func(t *testing.T, mutator crudMutator, access bool, exists bool, err error)
	// restarts tells whether the path reached the world, which stops and restarts every routine
	restarts bool
}

func expectFlags(t *testing.T, access bool, exists bool, wantAccess bool, wantExists bool) {
	t.Helper()
	if access != wantAccess || exists != wantExists {
		t.Fatalf("expected access=%v exists=%v, got access=%v exists=%v", wantAccess, wantExists, access, exists)
	}
}

var crudPaths = []crudPath{
	{
		name:  "success",
		token: crudToken,
		expect: func(t *testing.T, mutator crudMutator, access bool, exists bool, err error) {
			if err != nil {
				t.Fatal(err)
			}
			expectFlags(t, access, exists, true, true)
		},
		restarts: true,
	},
	{
		name:  "denied",
		token: otherToken,
		expect: func(t *testing.T, mutator crudMutator, access bool, exists bool, err error) {
			if err != nil {
				t.Fatal(err)
			}
			expectFlags(t, access, exists, false, true)
		},
	},
	{
		name:    "not found",
		token:   crudToken,
		missing: true,
		expect: func(t *testing.T, mutator crudMutator, access bool, exists bool, err error) {
			if err != nil {
				t.Fatal(err)
			}
			expectFlags(t, access, exists, false, false)
		},
	},
	{
		name:    "world does not convert",
		token:   crudToken,
		prepare: func(repo *StateRepo, store *worldStore) { breakWorld(repo) },
		expect: func(t *testing.T, mutator crudMutator, access bool, exists bool, err error) {
			if err == nil || errors.Is(err, errPersistFailed) {
				t.Fatalf("expected the conversion error, got %v", err)
			}
			expectFlags(t, access, exists, !mutator.worldLevel, true)
		},
		restarts: true,
	},
	{
		name:    "store fails",
		token:   crudToken,
		prepare: func(repo *StateRepo, store *worldStore) { store.fail.Store(true) },
		expect: func(t *testing.T, mutator crudMutator, access bool, exists bool, err error) {
			if !errors.Is(err, errPersistFailed) {
				t.Fatalf("expected the store error, got %v", err)
			}
			expectFlags(t, access, exists, true, true)
		},
		restarts: true,
	},
}

// Every mutator returns the flags and errors the api maps to a status, on
// every path: 200, 401, 404, and 500 for a world that does not convert or a
// store that fails. The flags are those of the code before the edits became
// atomic, including that a world level edit reports a conversion error with
// access=false.
func TestEveryMutatorReturnsTheSameResultsOnEveryPath(t *testing.T) {
	for _, mutator := range crudMutators {
		for _, path := range crudPaths {
			t.Run(mutator.name+"/"+path.name, func(t *testing.T) {
				repo, store := pathTestRepo(t)
				if path.prepare != nil {
					path.prepare(repo, store)
				}
				id := mutator.valid
				if path.missing {
					id = "unknown"
				}
				access, exists, err := mutator.call(repo, path.token, id)
				path.expect(t, mutator, access, exists, err)
			})
		}
	}
}

// Only a path that reaches the world stops and restarts the routines, which
// resets every ticker: a denied or unknown target leaves them running as they
// were, a conversion or store failure restarts them from the unchanged worlds.
func TestOnlyAnEditThatReachesTheWorldRestartsTheRoutines(t *testing.T) {
	for _, mutator := range crudMutators {
		for _, path := range crudPaths {
			if path.name == "success" {
				//a successful edit may delete the world or the routine that owns the ticker
				continue
			}
			t.Run(mutator.name+"/"+path.name, func(t *testing.T) {
				repo, store := pathTestRepo(t)
				if path.prepare != nil {
					path.prepare(repo, store)
				}
				before := firstTicker(t, repo)
				id := mutator.valid
				if path.missing {
					id = "unknown"
				}
				_, _, _ = mutator.call(repo, path.token, id)
				restarted := firstTicker(t, repo) != before
				if restarted != path.restarts {
					t.Fatalf("expected restarted=%v, got %v", path.restarts, restarted)
				}
			})
		}
	}
}

// CreateWorld writes a new world without reading one, so only the store can fail it.
func TestCreateWorldReturnsAFailedStore(t *testing.T) {
	repo, store := pathTestRepo(t)
	store.fail.Store(true)
	if _, err := repo.CreateWorld(crudToken, CreateWorldRequest{Name: "new"}); !errors.Is(err, errPersistFailed) {
		t.Fatalf("expected the store error, got %v", err)
	}
	store.fail.Store(false)
	world, err := repo.CreateWorld(crudToken, CreateWorldRequest{Name: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := store.stored(t, world.Id); !stored {
		t.Fatal("the created world was not stored")
	}
}

// An edit whose own values do not convert fails after the routines were
// stopped, which restarts them; the store is never asked.
func TestAnEditThatDoesNotConvertRestartsTheRoutines(t *testing.T) {
	cases := []struct {
		name string
		edit func(repo *StateRepo) error
		want string
	}{
		{"update world", func(repo *StateRepo) error {
			_, _, _, err := repo.UpdateWorld(crudToken, UpdateWorldRequest{Id: "w", States: map[string]interface{}{"broken": math.Inf(1)}})
			return err
		}, ""},
		{"dev update room", func(repo *StateRepo) error {
			return repo.DevUpdateRoom("w", RoomMsg{Id: "r2", States: map[string]interface{}{"broken": math.Inf(1)}})
		}, ""},
		{"dev update device without room", func(repo *StateRepo) error {
			return repo.DevUpdateDevice("w", "", DeviceMsg{Id: "d2"})
		}, "missing room id"},
		{"dev update device in an unknown room", func(repo *StateRepo) error {
			return repo.DevUpdateDevice("w", "nope", DeviceMsg{Id: "d2"})
		}, "unknown room id: nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, store := pathTestRepo(t)
			writes := len(store.history())
			before := firstTicker(t, repo)
			err := tc.edit(repo)
			if err == nil || (tc.want != "" && err.Error() != tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if firstTicker(t, repo) == before {
				t.Fatal("the routines were not restarted after the edit failed behind Stop")
			}
			if later := store.history()[writes:]; len(later) != 0 {
				t.Fatalf("%d writes reached the store", len(later))
			}
		})
	}
}

// An unknown or missing world is refused before anything is stopped.
func TestADevEditOfAnUnknownWorldStopsNothing(t *testing.T) {
	cases := []struct {
		name string
		edit func(repo *StateRepo) error
		want string
	}{
		{"room without world", func(repo *StateRepo) error { return repo.DevUpdateRoom("", RoomMsg{}) }, "missing world id"},
		{"room in unknown world", func(repo *StateRepo) error { return repo.DevUpdateRoom("nope", RoomMsg{}) }, "unknown world id"},
		{"device without world", func(repo *StateRepo) error { return repo.DevUpdateDevice("", "r", DeviceMsg{}) }, "missing world id"},
		{"device in unknown world", func(repo *StateRepo) error { return repo.DevUpdateDevice("nope", "r", DeviceMsg{}) }, "unknown world id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := pathTestRepo(t)
			before := firstTicker(t, repo)
			if err := tc.edit(repo); err == nil || err.Error() != tc.want {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if firstTicker(t, repo) != before {
				t.Fatal("the routines were restarted for an edit that never reached a world")
			}
		})
	}
}

// The reads keep their results for owner, other user and unknown id.
func TestTheReadsReturnTheSameResultsOnEveryPath(t *testing.T) {
	reads := []struct {
		name  string
		valid string
		read  func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error)
	}{
		{"ReadWorld", "w", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
			_, access, exists, err := repo.ReadWorld(token, id)
			return access, exists, err
		}},
		{"ReadRoom", "r", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
			room, access, exists, err := repo.ReadRoom(token, id)
			if access && (room.World != "w" || room.Room.Name != "room") {
				return access, exists, errors.New("wrong room")
			}
			return access, exists, err
		}},
		{"ReadDevice", "d", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
			device, access, exists, err := repo.ReadDevice(token, id)
			if access && (device.World != "w" || device.Room != "r" || device.Device.Name != "device") {
				return access, exists, errors.New("wrong device")
			}
			return access, exists, err
		}},
		{"ReadService", "s", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
			service, access, exists, err := repo.ReadService(token, id)
			if access && (service.World != "w" || service.Room != "r" || service.Device != "d" || service.Service.Code != "var x = 1;") {
				return access, exists, errors.New("wrong service")
			}
			return access, exists, err
		}},
		{"ReadChangeRoutine", "routine-room", func(repo *StateRepo, token sc_jwt.Token, id string) (bool, bool, error) {
			routine, access, exists, err := repo.ReadChangeRoutine(token, id)
			if access && (routine.RefType != "room" || routine.RefId != "r" || routine.Code != "room code") {
				return access, exists, errors.New("wrong routine")
			}
			return access, exists, err
		}},
	}
	for _, read := range reads {
		t.Run(read.name, func(t *testing.T) {
			repo, _ := pathTestRepo(t)
			access, exists, err := read.read(repo, crudToken, read.valid)
			if err != nil {
				t.Fatal(err)
			}
			expectFlags(t, access, exists, true, true)
			access, exists, err = read.read(repo, otherToken, read.valid)
			if err != nil {
				t.Fatal(err)
			}
			expectFlags(t, access, exists, false, true)
			access, exists, err = read.read(repo, crudToken, "unknown")
			if err != nil {
				t.Fatal(err)
			}
			expectFlags(t, access, exists, false, false)
		})
	}
}

// An index that does not match the worlds gives the errors it always gave.
func TestAnInconsistentIndexIsAnError(t *testing.T) {
	cases := []struct {
		name       string
		corrupt    func(repo *StateRepo)
		call       func(repo *StateRepo) (bool, bool, error)
		wantAccess bool
		wantExists bool
		want       string
	}{
		{"device without room", func(repo *StateRepo) { delete(repo.deviceRoomIndex, "d") }, func(repo *StateRepo) (bool, bool, error) {
			_, access, exists, err := repo.UpdateDevice(crudToken, UpdateDeviceRequest{Id: "d"})
			return access, exists, err
		}, false, false, "inconsistent deviceRoomIndex"},
		{"service without world", func(repo *StateRepo) { delete(repo.deviceWorldIndex, "d") }, func(repo *StateRepo) (bool, bool, error) {
			_, access, exists, err := repo.UpdateService(crudToken, UpdateServiceRequest{Id: "s"})
			return access, exists, err
		}, false, false, "inconsistent deviceWorldIndex"},
		{"service without room", func(repo *StateRepo) { delete(repo.deviceRoomIndex, "d") }, func(repo *StateRepo) (bool, bool, error) {
			_, access, exists, err := repo.DeleteService(crudToken, "s")
			return access, exists, err
		}, false, false, "inconsistent deviceRoomIndex"},
		{"routine of an unknown ref type", func(repo *StateRepo) {
			repo.changeRoutineIndex["bogus"] = ChangeRoutineIndexElement{Id: "bogus", RefType: "zone", RefId: "w"}
		}, func(repo *StateRepo) (bool, bool, error) {
			_, access, exists, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: "bogus"})
			return access, exists, err
		}, true, true, "unknown ref type"},
		{"routine missing from its world", func(repo *StateRepo) {
			repo.changeRoutineIndex["ghost"] = ChangeRoutineIndexElement{Id: "ghost", RefType: "world", RefId: "w"}
		}, func(repo *StateRepo) (bool, bool, error) {
			_, access, exists, err := repo.DeleteChangeRoutine(crudToken, "ghost")
			return access, exists, err
		}, true, true, "inconsistent routine id existence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := pathTestRepo(t)
			repo.mux.Lock()
			tc.corrupt(repo)
			repo.mux.Unlock()
			access, exists, err := tc.call(repo)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			expectFlags(t, access, exists, tc.wantAccess, tc.wantExists)
		})
	}
}

// After Shutdown a delete the owner may make is refused with the shutdown error.
func TestDeleteWorldAfterShutdownIsRefused(t *testing.T) {
	repo, store := pathTestRepo(t)
	repo.Shutdown()
	access, exists, err := repo.DeleteWorld(crudToken, "w")
	if !errors.Is(err, errShutDown) {
		t.Fatalf("expected the shutdown error, got %v", err)
	}
	expectFlags(t, access, exists, true, true)
	if _, stored := store.stored(t, "w"); !stored {
		t.Fatal("the world was deleted after the shutdown")
	}
}

// CreateDeviceByType asks the device manager outside the lock and then puts
// the device into the world it re-reads under the lock.
func TestCreateDeviceByTypeAddsTheDeviceToTheCurrentWorld(t *testing.T) {
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/device-types/"):
			_, _ = w.Write([]byte(`{"id":"dt","services":[{"id":"external-service","name":"svc"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/devices":
			_, _ = w.Write([]byte(`{"id":"external-device","device_type_id":"dt"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(manager.Close)
	repo, store := pathTestRepo(t)
	repo.Config.DeviceManagerUrl = manager.URL
	result, access, exists, err := repo.CreateDeviceByType(crudToken, CreateDeviceByTypeRequest{DeviceTypeId: "dt", Room: "r", Name: "typed"})
	if err != nil {
		t.Fatal(err)
	}
	expectFlags(t, access, exists, true, true)
	if result.World != "w" || result.Room != "r" || result.Device.ExternalRef != "external-device" || len(result.Device.Services) != 1 {
		t.Fatalf("unexpected result %+v", result)
	}
	world, _ := store.stored(t, "w")
	device := world.Rooms["r"].Devices[result.Device.Id]
	if device == nil || device.ExternalTypeId != "dt" || world.Rooms["r"].Devices["d"] == nil {
		t.Fatal("the typed device is not stored next to the existing one")
	}
}
