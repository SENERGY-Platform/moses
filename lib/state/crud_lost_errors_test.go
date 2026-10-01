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
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
)

var errPersistFailed = errors.New("persist failed")

// crudPersistence fails PersistWorld with persistErr when set and serves
// template for every GetTemplate; it records every world it was asked to store.
type crudPersistence struct {
	mux        sync.Mutex
	persistErr error
	template   RoutineTemplate
	persisted  []World
}

func (this *crudPersistence) PersistWorld(world World) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.persisted = append(this.persisted, world)
	return this.persistErr
}
func (this *crudPersistence) persistedWorlds() []World {
	this.mux.Lock()
	defer this.mux.Unlock()
	return append([]World{}, this.persisted...)
}
func (this *crudPersistence) PersistTemplate(templ RoutineTemplate) error { return nil }
func (this *crudPersistence) LoadWorlds() (map[string]*World, error) {
	return map[string]*World{}, nil
}
func (this *crudPersistence) GetTemplate(id string) (RoutineTemplate, error) {
	return this.template, nil
}
func (this *crudPersistence) GetTemplates() ([]RoutineTemplate, error) { return nil, nil }
func (this *crudPersistence) DeleteWorld(id string) error              { return nil }
func (this *crudPersistence) DeleteTemplate(id string) error           { return nil }

const crudOwner = "crud-owner"

var crudToken = sc_jwt.Token{Sub: crudOwner}

// crudTestRepo runs one world "w" with room "r" and device "d", each carrying
// one change routine ("routine-world", "routine-room", "routine-device").
// Interval 0 keeps the routines indexed without starting a ticker.
func crudTestRepo(t *testing.T, persistence PersistenceInterface) *StateRepo {
	t.Helper()
	msg := WorldMsg{
		Id:             "w",
		Name:           "world",
		Owner:          crudOwner,
		States:         map[string]interface{}{},
		ChangeRoutines: map[string]ChangeRoutine{"routine-world": {Id: "routine-world", Code: "world code"}},
		Rooms: map[string]RoomMsg{
			"r": {
				Id:             "r",
				Name:           "room",
				States:         map[string]interface{}{},
				ChangeRoutines: map[string]ChangeRoutine{"routine-room": {Id: "routine-room", Code: "room code"}},
				Devices: map[string]DeviceMsg{
					"d": {
						Id:             "d",
						Name:           "device",
						ExternalRef:    "external-d",
						States:         map[string]interface{}{},
						ChangeRoutines: map[string]ChangeRoutine{"routine-device": {Id: "routine-device", Code: "device code"}},
					},
				},
			},
		},
	}
	world, err := msg.ToModel()
	if err != nil {
		t.Fatal(err)
	}
	repo := &StateRepo{
		Worlds:      map[string]*World{"w": &world},
		Persistence: persistence,
		Config:      config.Config{JsTimeout: time.Second},
		StateLogger: &recordingConnectionLog{},
		ScriptHttp:  loopbackScriptHTTP(t),
	}
	repo.Start()
	//under the lock, so a cleanup after a failed test cannot stop the routines alongside an update still in Stop
	t.Cleanup(repo.Shutdown)
	return repo
}

var crudRefs = []struct {
	refType, refId, routineId string
}{
	{"world", "w", "routine-world"},
	{"room", "r", "routine-room"},
	{"device", "d", "routine-device"},
}

func TestCreateChangeRoutineReturnsAFailedUpdate(t *testing.T) {
	for _, ref := range crudRefs {
		t.Run(ref.refType, func(t *testing.T) {
			repo := crudTestRepo(t, &crudPersistence{persistErr: errPersistFailed})
			_, access, exists, err := repo.CreateChangeRoutine(crudToken, CreateChangeRoutineRequest{RefType: ref.refType, RefId: ref.refId, Code: "new"})
			if !errors.Is(err, errPersistFailed) {
				t.Fatalf("expected the persist error, got err=%v access=%v exists=%v", err, access, exists)
			}
		})
	}
}

func TestUpdateChangeRoutineReturnsAFailedUpdate(t *testing.T) {
	for _, ref := range crudRefs {
		t.Run(ref.refType, func(t *testing.T) {
			repo := crudTestRepo(t, &crudPersistence{persistErr: errPersistFailed})
			_, access, exists, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: ref.routineId, Code: "changed"})
			if !errors.Is(err, errPersistFailed) {
				t.Fatalf("expected the persist error, got err=%v access=%v exists=%v", err, access, exists)
			}
		})
	}
}

func TestDeleteChangeRoutineReturnsAFailedUpdate(t *testing.T) {
	for _, ref := range crudRefs {
		t.Run(ref.refType, func(t *testing.T) {
			repo := crudTestRepo(t, &crudPersistence{persistErr: errPersistFailed})
			_, access, exists, err := repo.DeleteChangeRoutine(crudToken, ref.routineId)
			if !errors.Is(err, errPersistFailed) {
				t.Fatalf("expected the persist error, got err=%v access=%v exists=%v", err, access, exists)
			}
		})
	}
}

// The control for the three tests above: with a working persistence the same
// calls succeed, so the error they expect comes from the persist step.
func TestChangeRoutineCrudSucceedsWithAWorkingPersistence(t *testing.T) {
	for _, ref := range crudRefs {
		t.Run(ref.refType, func(t *testing.T) {
			repo := crudTestRepo(t, &crudPersistence{})
			if _, access, exists, err := repo.CreateChangeRoutine(crudToken, CreateChangeRoutineRequest{RefType: ref.refType, RefId: ref.refId}); err != nil || !access || !exists {
				t.Fatalf("create: err=%v access=%v exists=%v", err, access, exists)
			}
			if _, access, exists, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: ref.routineId}); err != nil || !access || !exists {
				t.Fatalf("update: err=%v access=%v exists=%v", err, access, exists)
			}
			if _, access, exists, err := repo.DeleteChangeRoutine(crudToken, ref.routineId); err != nil || !access || !exists {
				t.Fatalf("delete: err=%v access=%v exists=%v", err, access, exists)
			}
		})
	}
}

// A template mustache cannot parse must fail the request instead of storing
// the empty string Render returns alongside its error as the routine code.
func TestChangeRoutineByTemplateReturnsARenderError(t *testing.T) {
	unparsable := RoutineTemplate{Id: "broken", Template: "{{#section}} never closed"}

	t.Run("create", func(t *testing.T) {
		persistence := &crudPersistence{template: unparsable}
		repo := crudTestRepo(t, persistence)
		_, _, _, err := repo.CreateChangeRoutineByTemplate(crudToken, CreateChangeRoutineByTemplateRequest{TemplId: "broken", RefType: "world", RefId: "w"})
		if err == nil {
			t.Fatal("expected the render error")
		}
		if stored := persistence.persistedWorlds(); len(stored) != 0 {
			t.Fatalf("expected nothing to be stored, got %d worlds", len(stored))
		}
	})

	t.Run("update", func(t *testing.T) {
		persistence := &crudPersistence{template: unparsable}
		repo := crudTestRepo(t, persistence)
		_, _, _, err := repo.UpdateChangeRoutineByTemplate(crudToken, UpdateChangeRoutineByTemplateRequest{TemplId: "broken", RoutineId: "routine-world"})
		if err == nil {
			t.Fatal("expected the render error")
		}
		if stored := persistence.persistedWorlds(); len(stored) != 0 {
			t.Fatalf("expected nothing to be stored, got %d worlds", len(stored))
		}
		routine, _, _, err := repo.ReadChangeRoutine(crudToken, "routine-world")
		if err != nil || routine.Code != "world code" {
			t.Fatalf("expected the routine code to be unchanged, got %q err=%v", routine.Code, err)
		}
	})
}

// A state json cannot encode (here +Inf, which CleanStates does not replace)
// makes the world conversion fail; the dev updates must return that instead of
// storing the half converted world, which has lost its id, states and routines.
func TestDevUpdateRefusesAWorldThatDoesNotConvert(t *testing.T) {
	t.Run("room", func(t *testing.T) {
		persistence := &crudPersistence{}
		repo := crudTestRepo(t, persistence)
		repo.Worlds["w"].States["broken"] = math.Inf(1)
		err := repo.DevUpdateRoom("w", RoomMsg{Id: "r", Name: "renamed"})
		if err == nil {
			t.Fatal("expected the conversion error")
		}
		if stored := persistence.persistedWorlds(); len(stored) != 0 {
			t.Fatalf("expected nothing to be stored, got world with id %q", stored[0].Id)
		}
		if _, ok := repo.Worlds[""]; ok {
			t.Fatal("a world without id was added to the running worlds")
		}
	})

	t.Run("device", func(t *testing.T) {
		persistence := &crudPersistence{}
		repo := crudTestRepo(t, persistence)
		repo.Worlds["w"].States["broken"] = math.Inf(1)
		err := repo.DevUpdateDevice("w", "r", DeviceMsg{Id: "d", Name: "renamed"})
		if err == nil {
			t.Fatal("expected the conversion error")
		}
		if stored := persistence.persistedWorlds(); len(stored) != 0 {
			t.Fatalf("expected nothing to be stored, got world with id %q", stored[0].Id)
		}
		if _, ok := repo.Worlds[""]; ok {
			t.Fatal("a world without id was added to the running worlds")
		}
	})
}
