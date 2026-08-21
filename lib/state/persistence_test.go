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
	"reflect"
	"sync"
	"testing"

	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// the states of world/room/device are free form (map[string]interface{}) and are
// passed to the js vm and marshalled to json. the previously used mgo driver decoded
// embedded bson documents to bson.M (map) and bson arrays to []interface{}, while the
// default mongo-driver registry would decode them to bson.D (slice of key/value pairs),
// which would change the json representation of the states.
// this test pins the mgo compatible decoding of the used registry.
func TestBsonStateRoundTrip(t *testing.T) {
	world := World{
		Id:    "world_1",
		Owner: "owner_1",
		Name:  "world one",
		States: map[string]interface{}{
			"float":  float64(13.5),
			"string": "foo",
			"bool":   true,
			"int":    42,
			"nested": map[string]interface{}{"a": float64(1), "b": map[string]interface{}{"c": "d"}},
			"list":   []interface{}{float64(1), "two", map[string]interface{}{"three": float64(3)}},
		},
		Rooms: map[string]*Room{
			"room_1": {
				Id:             "room_1",
				Name:           "room one",
				States:         map[string]interface{}{"temp": float64(20)},
				ChangeRoutines: map[string]ChangeRoutine{"routine_2": {Id: "routine_2", Interval: 1, Code: "moses.room.set('temp', 1)"}},
				Devices: map[string]*Device{
					"device_1": {
						Id:             "device_1",
						Name:           "device one",
						ImageUrl:       "http://example.org/image.png",
						ExternalTypeId: "ext_type_1",
						ExternalRef:    "ext_device_1",
						States:         map[string]interface{}{"on": false},
						ChangeRoutines: map[string]ChangeRoutine{"routine_3": {Id: "routine_3", Interval: 1, Code: "moses.device.set('on', true)"}},
						Services: map[string]Service{
							"service_1": {
								Id:             "service_1",
								Name:           "service one",
								ExternalRef:    "ext_service_1",
								SensorInterval: 42,
								Code:           "moses.service.send(1)",
							},
						},
					},
				},
			},
		},
		ChangeRoutines: map[string]ChangeRoutine{
			"routine_1": {Id: "routine_1", Interval: 10, Code: "moses.world.set('float', 1)"},
		},
		mux: &sync.Mutex{},
	}

	temp, err := bson.MarshalWithRegistry(mongoRegistry, world)
	if err != nil {
		t.Fatal(err)
	}

	result := World{}
	err = bson.UnmarshalWithRegistry(mongoRegistry, temp, &result)
	if err != nil {
		t.Fatal(err)
	}

	//embedded documents must be maps, not bson.D
	nested, ok := result.States["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected map[string]interface{} for nested state, got %T (%#v)", result.States["nested"], result.States["nested"])
	}
	if _, ok := nested["b"].(map[string]interface{}); !ok {
		t.Fatalf("expected map[string]interface{} for deeply nested state, got %T", nested["b"])
	}
	list, ok := result.States["list"].([]interface{})
	if !ok {
		t.Fatalf("expected []interface{} for list state, got %T", result.States["list"])
	}
	if _, ok := list[2].(map[string]interface{}); !ok {
		t.Fatalf("expected map[string]interface{} inside list state, got %T", list[2])
	}
	//mgo decoded bson int32 to int; the default registry would produce int32
	if _, ok := result.States["int"].(int); !ok {
		t.Fatalf("expected int for int state, got %T", result.States["int"])
	}

	//the json representation (used by api and js vm) must be unchanged by the bson round trip
	expectedJson, err := json.Marshal(world.States)
	if err != nil {
		t.Fatal(err)
	}
	actualJson, err := json.Marshal(result.States)
	if err != nil {
		t.Fatal(err)
	}
	if string(expectedJson) != string(actualJson) {
		t.Fatalf("states changed by bson round trip:\n%v\n%v", string(expectedJson), string(actualJson))
	}

	//the rest of the model must survive the round trip unchanged; the mutex is not persisted
	if result.mux != nil {
		t.Fatalf("expected unpersisted mutex to be nil after decode, got %#v", result.mux)
	}
	result.mux = world.mux
	if !reflect.DeepEqual(world.Rooms, result.Rooms) {
		a, _ := json.Marshal(world.Rooms)
		b, _ := json.Marshal(result.Rooms)
		t.Fatalf("rooms changed by bson round trip:\n%v\n%v", string(a), string(b))
	}
	if !reflect.DeepEqual(world.ChangeRoutines, result.ChangeRoutines) {
		t.Fatalf("change routines changed by bson round trip:\n%#v\n%#v", world.ChangeRoutines, result.ChangeRoutines)
	}
	if world.Id != result.Id || world.Owner != result.Owner || world.Name != result.Name {
		t.Fatalf("world changed by bson round trip: %#v", result)
	}
}

// mgo encoded nil maps/slices as empty documents/arrays; the used registry must keep that,
// so that a decoded world never contains nil maps which the js api writes to
func TestBsonNilMaps(t *testing.T) {
	temp, err := bson.MarshalWithRegistry(mongoRegistry, World{Id: "world_1"})
	if err != nil {
		t.Fatal(err)
	}
	result := World{}
	err = bson.UnmarshalWithRegistry(mongoRegistry, temp, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.States == nil || result.Rooms == nil || result.ChangeRoutines == nil {
		t.Fatalf("expected empty maps instead of nil: %#v", result)
	}
	if len(result.States) != 0 || len(result.Rooms) != 0 || len(result.ChangeRoutines) != 0 {
		t.Fatalf("expected empty maps: %#v", result)
	}
	temp, err = bson.MarshalWithRegistry(mongoRegistry, RoutineTemplate{Id: "templ_1"})
	if err != nil {
		t.Fatal(err)
	}
	templ := RoutineTemplate{}
	err = bson.UnmarshalWithRegistry(mongoRegistry, temp, &templ)
	if err != nil {
		t.Fatal(err)
	}
	if templ.Parameter == nil || len(templ.Parameter) != 0 {
		t.Fatalf("expected empty parameter list, got %#v", templ.Parameter)
	}
}

// existing production data was written with the mgo driver; the bson field names must not change.
// Service.Code has no bson tag and relies on the lowercased field name.
func TestBsonFieldNames(t *testing.T) {
	world := World{
		Id:     "world_1",
		Owner:  "owner_1",
		Name:   "world one",
		States: map[string]interface{}{"temp": float64(20)},
		Rooms: map[string]*Room{"room_1": {
			Id: "room_1",
			Devices: map[string]*Device{"device_1": {
				Id:             "device_1",
				ImageUrl:       "image",
				ExternalTypeId: "type",
				ExternalRef:    "ref",
				Services:       map[string]Service{"service_1": {Id: "service_1", SensorInterval: 1, Code: "code"}},
			}},
		}},
		ChangeRoutines: map[string]ChangeRoutine{"routine_1": {Id: "routine_1", Interval: 1, Code: "code"}},
		mux:            &sync.Mutex{},
	}
	temp, err := bson.MarshalWithRegistry(mongoRegistry, world)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]interface{}{}
	err = bson.UnmarshalWithRegistry(mongoRegistry, temp, &doc)
	if err != nil {
		t.Fatal(err)
	}
	expectWorldKeys := []string{"id", "owner", "name", "states", "rooms", "change_routines"}
	assertKeys(t, "world", doc, expectWorldKeys)
	if _, ok := doc["mux"]; ok {
		t.Fatal("mutex must not be persisted")
	}
	if len(doc) != len(expectWorldKeys) {
		t.Fatalf("unexpected world keys: %#v", doc)
	}
	rooms, ok := doc["rooms"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected rooms encoding: %T %#v", doc["rooms"], doc["rooms"])
	}
	room, ok := rooms["room_1"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected room encoding: %T %#v", rooms["room_1"], rooms["room_1"])
	}
	assertKeys(t, "room", room, []string{"id", "name", "states", "devices", "change_routines"})
	devices, ok := room["devices"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected devices encoding: %T %#v", room["devices"], room["devices"])
	}
	device, ok := devices["device_1"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected device encoding: %T %#v", devices["device_1"], devices["device_1"])
	}
	assertKeys(t, "device", device, []string{"id", "name", "image_url", "external_type_id", "external_ref", "states", "change_routines", "services"})
	services, ok := device["services"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected services encoding: %T %#v", device["services"], device["services"])
	}
	service, ok := services["service_1"].(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected service encoding: %T %#v", services["service_1"], services["service_1"])
	}
	assertKeys(t, "service", service, []string{"id", "name", "external_ref", "sensor_interval", "code"})

	templ := RoutineTemplate{Id: "templ_1", Name: "n", Description: "d", Template: "t", Parameter: []string{"p"}}
	temp, err = bson.MarshalWithRegistry(mongoRegistry, templ)
	if err != nil {
		t.Fatal(err)
	}
	doc = map[string]interface{}{}
	err = bson.UnmarshalWithRegistry(mongoRegistry, temp, &doc)
	if err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "template", doc, []string{"id", "name", "description", "template", "parameter"})
}

func assertKeys(t *testing.T, name string, doc map[string]interface{}, keys []string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := doc[key]; !ok {
			t.Fatalf("missing bson field %v.%v in %#v", name, key, doc)
		}
	}
}

// the not found error of GetTemplate() is interpreted by the callers (ReadTemplate(), UpdateTemplate())
func TestTemplateNotFoundSentinel(t *testing.T) {
	//ErrNotFound is its own sentinel: a raw mongo.ErrNoDocuments from some other operation
	//must not be mistaken for "this template does not exist". GetTemplate() translates it.
	if errors.Is(mongo.ErrNoDocuments, ErrNotFound) {
		t.Fatal("ErrNotFound must not be satisfied by a raw mongo.ErrNoDocuments")
	}
	repo := &StateRepo{Persistence: notFoundPersistence{}}

	//not found
	_, exists, err := repo.ReadTemplate(sc_jwt.Token{Sub: "user"}, "unknown")
	if err != nil || exists {
		t.Fatalf("expected exists=false without error, got exists=%v err=%v", exists, err)
	}
	_, exists, err = repo.UpdateTemplate(sc_jwt.Token{Sub: "user"}, UpdateTemplateRequest{Id: "unknown"})
	if err != nil || exists {
		t.Fatalf("expected exists=false without error, got exists=%v err=%v", exists, err)
	}

	//wrapped not found
	repoWrapped := &StateRepo{Persistence: notFoundPersistence{wrap: true}}
	_, exists, err = repoWrapped.ReadTemplate(sc_jwt.Token{Sub: "user"}, "unknown")
	if err != nil || exists {
		t.Fatalf("expected exists=false without error for wrapped error, got exists=%v err=%v", exists, err)
	}

	//other errors must be propagated
	repoErr := &StateRepo{Persistence: notFoundPersistence{err: errors.New("some other error")}}
	_, _, err = repoErr.ReadTemplate(sc_jwt.Token{Sub: "user"}, "unknown")
	if err == nil {
		t.Fatal("expected error to be propagated")
	}

	//default templates are served without persistence access
	for id := range defaultTemplates {
		_, exists, err = repo.ReadTemplate(sc_jwt.Token{Sub: "user"}, id)
		if err != nil || !exists {
			t.Fatalf("expected default template %v to exist, got exists=%v err=%v", id, exists, err)
		}
	}
}

type notFoundPersistence struct {
	wrap bool
	err  error
}

func (this notFoundPersistence) PersistWorld(world World) error { return nil }
func (this notFoundPersistence) PersistTemplate(templ RoutineTemplate) error {
	return nil
}
func (this notFoundPersistence) LoadWorlds() (map[string]*World, error) {
	return map[string]*World{}, nil
}
func (this notFoundPersistence) GetTemplate(id string) (templ RoutineTemplate, err error) {
	if this.err != nil {
		return templ, this.err
	}
	if this.wrap {
		return templ, fmt.Errorf("unable to read template %v: %w", id, ErrNotFound)
	}
	return templ, ErrNotFound
}
func (this notFoundPersistence) GetTemplates() ([]RoutineTemplate, error) { return nil, nil }
func (this notFoundPersistence) DeleteWorld(id string) error              { return nil }
func (this notFoundPersistence) DeleteTemplate(id string) error           { return nil }
