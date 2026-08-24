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
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/bson"
)

// the container is started once for the whole package and only when the first
// test that needs it runs, so that -short costs nothing.
//
// lib/test/server.MongoDB() is deliberately not used here: package server also
// contains server.New(), which imports lib, and lib will import this package
// through the api. That would be an import cycle for an in package test, and
// this test has to be in package repo to reach the collections directly.
var mongoOnce sync.Once
var mongoUrl string
var mongoErr error
var mongoTerminate func()

var databaseCounter atomic.Int64

func TestMain(m *testing.M) {
	code := m.Run()
	if mongoTerminate != nil {
		mongoTerminate()
	}
	os.Exit(code)
}

func startMongo() (string, error) {
	mongoOnce.Do(func() {
		log.Println("start mongo")
		ctx := context.Background()
		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "mongo:7.0",
				ExposedPorts: []string{"27017/tcp"},
				WaitingFor: wait.ForAll(
					wait.ForLog("Waiting for connections"),
					wait.ForListeningPort("27017/tcp"),
				),
				Tmpfs: map[string]string{"/data/db": "rw"},
			},
			Started: true,
		})
		if err != nil {
			mongoErr = err
			return
		}
		mongoTerminate = func() {
			log.Println("DEBUG: remove container mongo", container.Terminate(context.Background()))
		}
		port, err := container.MappedPort(ctx, "27017/tcp")
		if err != nil {
			mongoErr = err
			return
		}
		mongoUrl = "mongodb://localhost:" + port.Port()
	})
	return mongoUrl, mongoErr
}

// testStore gives every test its own database, so that a test cannot see what
// another one wrote and the counts asserted below stay meaningful.
func testStore(t *testing.T) *Mongo {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	url, err := startMongo()
	if err != nil {
		t.Fatal(err)
	}
	//the url is passed without a scheme on purpose: the legacy config allows it
	//and the store has to keep tolerating it
	store, err := NewMongo(config.Config{
		MongoUrl:                  sb_config_types.Secret(url[len("mongodb://"):]),
		MongoTable:                fmt.Sprintf("moses_repo_test_%d", databaseCounter.Add(1)),
		EnvironmentCollectionName: "environments",
		StateCollectionName:       "environment_states",
		DatasetCollectionName:     "datasets",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// testEnvironment is deliberately full of the value kinds that a free form state
// can hold: the mgo compatible registry is what makes them come back unchanged.
func testEnvironment(id string, name string, owner string) domain.Environment {
	return domain.Environment{
		Id:    id,
		Name:  name,
		Type:  domain.IndustrialSite,
		Owner: owner,
		Seed:  4711,
		Context: map[string]interface{}{
			"outdoor_temperature": 12.5,
			"weekday":             "monday",
			"holiday":             false,
			"cycles":              int64(9007199254740993),
			"count":               7,
			"nested":              map[string]interface{}{"inner": map[string]interface{}{"deep": 1.25}},
			"list":                []interface{}{"a", 1.5, true},
		},
		Zones: []domain.Zone{{
			Id:            "zone-1",
			Name:          "Halle 1",
			Type:          domain.ZoneHall,
			Tags:          []string{"production", "loud"},
			InitialStates: map[string]interface{}{"humidity": 40.0, "nested": map[string]interface{}{"a": "b"}},
			Zones: []domain.Zone{{
				Id:            "zone-1-1",
				Name:          "Nebenraum",
				Type:          domain.ZoneRoom,
				Tags:          []string{},
				InitialStates: map[string]interface{}{},
				Zones:         []domain.Zone{},
				Assets:        []domain.Asset{},
			}},
			Assets: []domain.Asset{{
				Id:             "asset-1",
				Name:           "Kompressor",
				Kind:           domain.AssetMachine,
				ExternalRef:    "urn:infai:ses:device:7283f08c",
				ExternalTypeId: "urn:infai:ses:device-type:dc5bf705",
				InitialStates:  map[string]interface{}{"kwh": 290508.57080252626, "rpm": int64(21)},
				Channels: []domain.Channel{{
					Id:               "channel-1",
					Name:             "getTemperatureService",
					Direction:        domain.Sensor,
					ExternalRef:      "urn:infai:ses:service:38657ee1",
					CharacteristicId: "urn:infai:ses:characteristic:degree",
					Unit:             "°C",
					IntervalSeconds:  30,
					Source:           domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{Code: "moses.service.send(1);"}},
				}},
			}},
		}},
	}
}

func countDocuments(t *testing.T, store *Mongo, collectionName string) int64 {
	t.Helper()
	count, err := store.client.Database(store.database).Collection(collectionName).CountDocuments(testContext(t), bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func TestMongoPutAndGetPreserveTheWholeTree(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	want := testEnvironment("env-1", "Metallbau", "owner-1")

	err := store.Put(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the stored tree came back changed:\nwant %#v\ngot  %#v", want, got)
	}
}

func TestMongoGetDecodesFreeFormValuesTheWayTheLegacyDriverDid(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	//the types matter, not only the values: the js vm and the json marshalling of
	//the api see exactly what the registry produces. without the mgo compatible
	//registry a stored array comes back as primitive.A instead of []interface{},
	//which is a different type for every type switch in the runtime.
	nested, ok := got.Context["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a map[string]interface{}, got %T", got.Context["nested"])
	}
	if _, ok := nested["inner"].(map[string]interface{}); !ok {
		t.Errorf("expected a map[string]interface{} one level deeper, got %T", nested["inner"])
	}
	if _, ok := got.Context["list"].([]interface{}); !ok {
		t.Errorf("expected a []interface{}, got %T", got.Context["list"])
	}
	if _, ok := got.Context["count"].(int); !ok {
		t.Errorf("expected a small number to come back as int, got %T", got.Context["count"])
	}
}

func TestMongoGetReportsAnUnknownEnvironmentAsNotFound(t *testing.T) {
	store := testStore(t)
	_, err := store.Get(testContext(t), "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected an error wrapping ErrNotFound, got %v", err)
	}
}

func TestMongoPutTwiceStoresOneDocument(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	env := testEnvironment("env-1", "Metallbau", "owner-1")
	for run := 0; run < 3; run++ {
		err := store.Put(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
	}
	if count := countDocuments(t, store, "environments"); count != 1 {
		t.Errorf("expected 1 document after repeated puts, got %d", count)
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, env) {
		t.Errorf("a repeated put changed the document:\nwant %#v\ngot  %#v", env, got)
	}
}

func TestMongoPutReplacesTheDocumentInsteadOfMergingIt(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}
	//a removed zone must be gone, not merged with what was stored before
	smaller := testEnvironment("env-1", "Metallbau", "owner-1")
	smaller.Zones = []domain.Zone{}
	err = store.Put(ctx, smaller)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Zones) != 0 {
		t.Errorf("expected the zones to be gone, got %#v", got.Zones)
	}
}

func TestMongoPutRejectsAnEnvironmentWithoutId(t *testing.T) {
	store := testStore(t)
	err := store.Put(testContext(t), testEnvironment("  ", "Metallbau", "owner-1"))
	if !errors.Is(err, ErrMissingId) {
		t.Errorf("expected an error wrapping ErrMissingId, got %v", err)
	}
	if count := countDocuments(t, store, "environments"); count != 0 {
		t.Errorf("expected nothing to be written, got %d documents", count)
	}
}

func TestMongoListByOwnerReturnsOnlyThatOwnersEnvironmentsOrderedByName(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	for _, env := range []domain.Environment{
		testEnvironment("env-3", "zeta", "owner-1"),
		testEnvironment("env-1", "Alpha", "owner-1"),
		testEnvironment("env-2", "beta", "owner-1"),
		testEnvironment("env-4", "aaa", "owner-2"),
	} {
		err := store.Put(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
	}

	list, err := store.ListByOwner(ctx, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, env := range list {
		names = append(names, env.Name)
	}
	//case insensitive: a plain mongodb sort would put "Alpha" and "beta" the
	//other way round, which is not what a user reading a list expects
	if !reflect.DeepEqual(names, []string{"Alpha", "beta", "zeta"}) {
		t.Errorf("expected the owner's environments ordered by name, got %v", names)
	}
}

func TestMongoListByOwnerOrdersEqualNamesById(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	for _, id := range []string{"env-c", "env-a", "env-b"} {
		err := store.Put(ctx, testEnvironment(id, "same name", "owner-1"))
		if err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.ListByOwner(ctx, "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, env := range list {
		ids = append(ids, env.Id)
	}
	if !reflect.DeepEqual(ids, []string{"env-a", "env-b", "env-c"}) {
		t.Errorf("expected equal names to be ordered by id, got %v", ids)
	}
}

func TestMongoListByOwnerReturnsAnEmptyListForAnUnknownOwner(t *testing.T) {
	store := testStore(t)
	list, err := store.ListByOwner(testContext(t), "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("expected an empty list, got %#v", list)
	}
}

func TestMongoAllReturnsEveryEnvironment(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	for _, env := range []domain.Environment{
		testEnvironment("env-1", "a", "owner-1"),
		testEnvironment("env-2", "b", "owner-2"),
	} {
		err := store.Put(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 environments, got %d", len(list))
	}
}

func TestMongoAllSkipsAnUndecodableDocument(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	err := store.Put(ctx, testEnvironment("env-1", "readable", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}
	//zones as a string instead of a list: the driver returns an error for this
	//document, and a single one of those must not keep the service from starting
	//with the environments that are readable
	_, err = store.environmentCollection().InsertOne(ctx, bson.M{"id": "env-broken", "name": "broken", "zones": "not a list"})
	if err != nil {
		t.Fatal(err)
	}

	list, err := store.All(ctx)
	if err != nil {
		t.Fatalf("a broken document must not fail the load: %v", err)
	}
	if len(list) != 1 || list[0].Id != "env-1" {
		t.Errorf("expected only the readable environment, got %#v", list)
	}
}

func TestMongoDeleteRemovesTheDefinitionAndTheState(t *testing.T) {
	store := testStore(t)
	states := store.States()
	ctx := testContext(t)
	err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}
	err = states.Save(ctx, RuntimeState{EnvironmentId: "env-1", Context: map[string]interface{}{"temperature": 21.5}})
	if err != nil {
		t.Fatal(err)
	}

	err = store.Delete(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "env-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected the definition to be gone, got %v", err)
	}
	if count := countDocuments(t, store, "environment_states"); count != 0 {
		t.Errorf("expected the runtime state to be gone, got %d documents", count)
	}
}

func TestMongoDeleteAcceptsAnEnvironmentThatDoesNotExist(t *testing.T) {
	store := testStore(t)
	err := store.Delete(testContext(t), "does-not-exist")
	if err != nil {
		t.Errorf("deleting something that does not exist must not be an error: %v", err)
	}
}

func TestMongoSaveAndLoadPreserveTheState(t *testing.T) {
	store := testStore(t)
	states := store.States()
	ctx := testContext(t)
	want := RuntimeState{
		EnvironmentId: "env-1",
		Context:       map[string]interface{}{"outdoor_temperature": 12.5, "nested": map[string]interface{}{"a": "b"}},
		Zones:         map[string]map[string]interface{}{"zone-1": {"humidity": 40.0}},
		Assets:        map[string]map[string]interface{}{"asset-1": {"kwh": 290508.57080252626, "rpm": int64(21)}},
	}
	err := states.Save(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := states.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	//the store sets the timestamp, so it is compared separately
	if got.UpdatedAtUnix == 0 {
		t.Error("expected the store to set updated_at_unix")
	}
	got.UpdatedAtUnix = 0
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the stored state came back changed:\nwant %#v\ngot  %#v", want, got)
	}
}

func TestMongoSaveTwiceStoresOneState(t *testing.T) {
	store := testStore(t)
	states := store.States()
	ctx := testContext(t)
	for run := 0; run < 3; run++ {
		err := states.Save(ctx, RuntimeState{EnvironmentId: "env-1", Context: map[string]interface{}{"run": int64(run)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if count := countDocuments(t, store, "environment_states"); count != 1 {
		t.Errorf("expected 1 state document after repeated saves, got %d", count)
	}
	got, err := states.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Context, map[string]interface{}{"run": int64(2)}) {
		t.Errorf("expected the last saved state, got %#v", got.Context)
	}
}

func TestMongoLoadReturnsAnEmptyStateWhenNothingIsStored(t *testing.T) {
	store := testStore(t)
	got, err := store.States().Load(testContext(t), "env-1")
	if err != nil {
		t.Fatalf("an environment without state is not an error: %v", err)
	}
	want := RuntimeState{
		EnvironmentId: "env-1",
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected an empty state with usable maps, got %#v", got)
	}
}

func TestMongoSaveRejectsAStateWithoutEnvironmentId(t *testing.T) {
	store := testStore(t)
	err := store.States().Save(testContext(t), RuntimeState{EnvironmentId: " "})
	if !errors.Is(err, ErrMissingId) {
		t.Errorf("expected an error wrapping ErrMissingId, got %v", err)
	}
	if count := countDocuments(t, store, "environment_states"); count != 0 {
		t.Errorf("expected nothing to be written, got %d documents", count)
	}
}

func TestMongoStateDeleteKeepsTheDefinition(t *testing.T) {
	store := testStore(t)
	states := store.States()
	ctx := testContext(t)
	err := store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
	if err != nil {
		t.Fatal(err)
	}
	err = states.Save(ctx, RuntimeState{EnvironmentId: "env-1", Context: map[string]interface{}{"temperature": 21.5}})
	if err != nil {
		t.Fatal(err)
	}

	err = states.Delete(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if count := countDocuments(t, store, "environment_states"); count != 0 {
		t.Errorf("expected the state to be gone, got %d documents", count)
	}
	if _, err := store.Get(ctx, "env-1"); err != nil {
		t.Errorf("the definition must survive a state delete: %v", err)
	}
}

func TestMongoConcurrentPutOfTheSameEnvironmentStoresOneDocument(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	//an upsert on a unique index can be rejected with a duplicate key error when
	//two writers race on a document that does not exist yet
	errs := make(chan error, 20)
	wg := sync.WaitGroup{}
	for run := 0; run < 20; run++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Put(ctx, testEnvironment("env-1", "Metallbau", "owner-1"))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent put failed: %v", err)
		}
	}
	if count := countDocuments(t, store, "environments"); count != 1 {
		t.Errorf("expected 1 document after concurrent puts, got %d", count)
	}
}

func TestMongoConcurrentSaveOfTheSameStateStoresOneDocument(t *testing.T) {
	store := testStore(t)
	states := store.States()
	ctx := testContext(t)
	errs := make(chan error, 20)
	wg := sync.WaitGroup{}
	for run := 0; run < 20; run++ {
		wg.Add(1)
		go func(run int) {
			defer wg.Done()
			errs <- states.Save(ctx, RuntimeState{EnvironmentId: "env-1", Context: map[string]interface{}{"run": int64(run)}})
		}(run)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent save failed: %v", err)
		}
	}
	if count := countDocuments(t, store, "environment_states"); count != 1 {
		t.Errorf("expected 1 state document after concurrent saves, got %d", count)
	}
}
