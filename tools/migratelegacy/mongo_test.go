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

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// the container is started once for the whole package and only when the first
// test that needs it runs, so that -short costs nothing. Same shape as
// lib/repo/mongo_test.go, which this mirrors.
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

// testConfig gives every test its own database, so that one test cannot see what
// another wrote.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	url, err := startMongo()
	if err != nil {
		t.Fatal(err)
	}
	return config.Config{
		MongoUrl:                  sb_config_types.Secret(url),
		MongoTable:                fmt.Sprintf("moses_migration_test_%d", databaseCounter.Add(1)),
		WorldCollectionName:       "worlds",
		TemplateCollectionName:    "templates",
		EnvironmentCollectionName: "environments",
		StateCollectionName:       "environment_states",
		DatasetCollectionName:     "datasets",
	}
}

func testStores(t *testing.T) (state.MongoPersistence, *repo.Mongo) {
	t.Helper()
	conf := testConfig(t)
	legacy, err := state.NewMongoPersistence(conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	environments, err := repo.NewMongo(conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(environments.Close)
	return legacy, environments
}

func mongoTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestMigrateAgainstMongo runs the code path the operator runs, against a real
// database, on a legacy world that was written by the legacy store itself: the
// dry run first, then the apply, then a second apply.
//
// It is one test rather than four because the sequence is the point - a dry run
// that writes nothing followed by an apply that writes once and a re-run that
// writes nothing more - and because each of them needs the state the previous
// one left behind.
func TestMigrateAgainstMongo(t *testing.T) {
	legacy, environments := testStores(t)
	ctx := mongoTestContext(t)

	world := testWorld("4d273cd0-838f-4f84-9974-e56d18245255", "Industry")
	world.Owner = "aae7e87b-63a2-477f-afb4-caa0db84e3fa"
	world.Rooms["room-1"].Devices["device-1"].States = map[string]interface{}{"kwh": 290508.57080252626}
	if err := legacy.PersistWorld(*world); err != nil {
		t.Fatal(err)
	}

	//the dry run has to leave the environment store untouched
	out, code := runMigration(t, legacy, environments, dryRunOptions())
	if code != exitClean {
		t.Fatalf("the dry run failed with %d\n%s", code, out)
	}
	if !strings.Contains(out, "would create") {
		t.Errorf("the dry run does not announce the write:\n%s", out)
	}
	if _, err := environments.Get(ctx, world.Id); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("the dry run wrote something: %v", err)
	}

	out, code = runMigration(t, legacy, environments, applyOptions())
	if code != exitClean {
		t.Fatalf("the apply failed with %d\n%s", code, out)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("the apply does not report the write:\n%s", out)
	}

	stored, err := environments.Get(ctx, world.Id)
	if err != nil {
		t.Fatalf("the environment was not stored: %v", err)
	}
	if stored.Name != "Industry" || stored.Owner != "aae7e87b-63a2-477f-afb4-caa0db84e3fa" {
		t.Errorf("name or owner were not carried over: %q / %q", stored.Name, stored.Owner)
	}
	if len(stored.Zones) != 1 || len(stored.Zones[0].Assets) != 1 || len(stored.Zones[0].Assets[0].Channels) != 1 {
		t.Fatalf("unexpected shape: %+v", stored.Zones)
	}
	asset := stored.Zones[0].Assets[0]
	//these three are the whole point of the migration: they keep the platform
	//device, its device type and the existing timeseries attached
	if asset.ExternalRef != "urn:infai:ses:device:7283f08c" {
		t.Errorf("the device reference was not preserved verbatim: %q", asset.ExternalRef)
	}
	if asset.ExternalTypeId != "urn:infai:ses:device-type:dc5bf705" {
		t.Errorf("the device type reference was not preserved verbatim: %q", asset.ExternalTypeId)
	}
	if asset.Channels[0].ExternalRef != "urn:infai:ses:service:38657ee1" {
		t.Errorf("the service reference was not preserved verbatim: %q", asset.Channels[0].ExternalRef)
	}
	//the legacy javascript went through mongo twice and has to be unchanged
	if asset.Channels[0].Source.Script == nil || asset.Channels[0].Source.Script.Code != "moses.service.send(1);" {
		t.Errorf("the service code was not carried into the channel source: %+v", asset.Channels[0].Source)
	}
	if asset.InitialStates["kwh"] != 290508.57080252626 {
		t.Errorf("the device state did not survive the round trip: %#v", asset.InitialStates["kwh"])
	}

	//a user corrects what the conversion had to guess, and the environment
	//accumulates runtime state
	stored.Zones[0].Type = "room"
	stored.Name = "Industry (corrected)"
	if err := environments.Put(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := environments.States().Save(ctx, repo.RuntimeState{
		EnvironmentId: world.Id,
		Assets:        map[string]map[string]interface{}{asset.Id: {"kwh": 999999.5}},
	}); err != nil {
		t.Fatal(err)
	}

	out, code = runMigration(t, legacy, environments, applyOptions())
	if code != exitClean {
		t.Fatalf("the second apply failed with %d\n%s", code, out)
	}
	if !strings.Contains(out, string(actionSkipped)) || !strings.Contains(out, "already exists") {
		t.Errorf("the second apply does not report a skip:\n%s", out)
	}
	again, err := environments.Get(ctx, world.Id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Name != "Industry (corrected)" || again.Zones[0].Type != "room" {
		t.Errorf("the second run overwrote the manual corrections: %q / %q", again.Name, again.Zones[0].Type)
	}
	runtimeState, err := environments.States().Load(ctx, world.Id)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeState.Assets[asset.Id]["kwh"] != 999999.5 {
		t.Errorf("the runtime state was lost: %#v", runtimeState.Assets)
	}

	//and the legacy world is still there, unchanged: the legacy runtime keeps
	//running on it until the separate runtime switch
	worlds, err := legacy.LoadWorlds()
	if err != nil {
		t.Fatal(err)
	}
	legacyWorld, ok := worlds[world.Id]
	if !ok {
		t.Fatalf("the legacy world is gone, %d worlds are left", len(worlds))
	}
	if legacyWorld.Name != "Industry" || len(legacyWorld.ChangeRoutines) != 1 {
		t.Errorf("the legacy world was modified: %q with %d change routines", legacyWorld.Name, len(legacyWorld.ChangeRoutines))
	}
	if legacyWorld.Rooms["room-1"].Devices["device-1"].Services["service-1"].Code != "moses.service.send(1);" {
		t.Errorf("the legacy service code was modified: %q", legacyWorld.Rooms["room-1"].Devices["device-1"].Services["service-1"].Code)
	}
}

// a world that cannot be stored has to be reported without stopping the others,
// and without leaving anything behind in the environment store.
func TestMigrateAgainstMongoReportsAnInvalidWorldAndWritesTheRest(t *testing.T) {
	legacy, environments := testStores(t)
	ctx := mongoTestContext(t)

	broken := testWorld("broken-world", "Aaa without a device type")
	broken.Rooms["room-1"].Devices["device-1"].ExternalTypeId = ""
	if err := legacy.PersistWorld(*broken); err != nil {
		t.Fatal(err)
	}
	if err := legacy.PersistWorld(*testWorld("intact-world", "Zzz intact")); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	code, err := migrate(ctx, out, legacy, environments, applyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if code != exitProblem {
		t.Errorf("expected exit %d, got %d\n%s", exitProblem, code, out.String())
	}
	if _, err := environments.Get(ctx, "broken-world"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("the invalid world was written anyway: %v", err)
	}
	if _, err := environments.Get(ctx, "intact-world"); err != nil {
		t.Errorf("the intact world was not written: %v", err)
	}
}
