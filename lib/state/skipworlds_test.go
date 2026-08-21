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
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
)

// recordingConnectionLog is a connectionlog.Logger that only remembers what it
// was told. The device connect log is the one external call StartDevice makes.
type recordingConnectionLog struct {
	connected []string
}

func (this *recordingConnectionLog) LogDeviceConnect(id string) error {
	this.connected = append(this.connected, id)
	return nil
}
func (this *recordingConnectionLog) LogDeviceDisconnect(id string) error { return nil }
func (this *recordingConnectionLog) LogHubConnect(gateway string) error  { return nil }
func (this *recordingConnectionLog) LogHubDisconnect(id string) error    { return nil }

// testWorld builds a world with one room, one device and one service through
// the msg conversion, because that is what gives the world its mutex.
//
// The intervals are an hour: this test is about what gets started, not about
// what a change routine computes, and a routine that actually fired would need
// a persistence and a connector behind it.
func testWorld(t *testing.T, id string, externalDeviceRef string) *World {
	t.Helper()
	msg := WorldMsg{
		Id:     id,
		Name:   id,
		States: map[string]interface{}{},
		ChangeRoutines: map[string]ChangeRoutine{
			"routine-" + id: {Id: "routine-" + id, Interval: 3600, Code: `moses.world.state.set("x", 1);`},
		},
		Rooms: map[string]RoomMsg{
			"room-" + id: {
				Id:     "room-" + id,
				Name:   "room",
				States: map[string]interface{}{},
				Devices: map[string]DeviceMsg{
					"device-" + id: {
						Id:          "device-" + id,
						Name:        "device",
						ExternalRef: externalDeviceRef,
						States:      map[string]interface{}{},
						Services: map[string]Service{
							"service-" + id: {
								Id:             "service-" + id,
								Name:           "service",
								ExternalRef:    "external-service-" + id,
								SensorInterval: 3600,
								Code:           `moses.service.send(1);`,
							},
						},
					},
				},
			},
		},
	}
	world, err := msg.ToModel()
	if err != nil {
		t.Fatal(err)
	}
	return &world
}

// TestSkipWorldIdsKeepsTheLegacyRuntimeOutOfAMigratedWorld is the cutover.
//
// Both runtimes publish under the same platform device and service ids, so a
// world that also exists as an environment must not be started here: it would
// send every value twice and let two scripts write the same state.
func TestSkipWorldIdsKeepsTheLegacyRuntimeOutOfAMigratedWorld(t *testing.T) {
	logger := &recordingConnectionLog{}
	repo := &StateRepo{
		Worlds: map[string]*World{
			"migrated": testWorld(t, "migrated", "external-device-migrated"),
			"legacy":   testWorld(t, "legacy", "external-device-legacy"),
		},
		Config:       config.Config{JsTimeout: time.Second, ProtocolSegmentName: "payload"},
		StateLogger:  logger,
		SkipWorldIds: map[string]bool{"migrated": true},
	}

	repo.Start()
	defer repo.Stop()

	//one change routine plus one sensor service, for the not migrated world only
	if len(repo.changeRoutinesTickers) != 2 {
		t.Errorf("expected 2 tickers for the one world that is not migrated, got %d", len(repo.changeRoutinesTickers))
	}
	if _, started := repo.externalRefDeviceIndex["external-device-legacy"]; !started {
		t.Error("expected the device of the world that was not migrated to be started")
	}
	if _, started := repo.externalRefDeviceIndex["external-device-migrated"]; started {
		t.Error("the device of the migrated world was started by the legacy runtime, it would now publish twice")
	}
	if _, indexed := repo.changeRoutineIndex["routine-migrated"]; indexed {
		t.Error("expected the change routines of the migrated world not to be indexed")
	}
	if _, indexed := repo.serviceDeviceIndex["service-migrated"]; indexed {
		t.Error("expected the services of the migrated world not to be indexed")
	}
	//the device is not logged as online either, which is what tells the platform
	//that the legacy runtime is not the one simulating it
	for _, id := range logger.connected {
		if id == "external-device-migrated" {
			t.Error("expected the migrated device not to be logged as connected by the legacy runtime")
		}
	}

	//a command for the migrated device finds nothing here and must not panic:
	//the wiring offers it to the new runtime first anyway
	answered := false
	repo.HandleCommand("external-device-migrated", "external-service-migrated", nil, func(interface{}) { answered = true })
	if answered {
		t.Error("expected the legacy runtime not to answer for a migrated device")
	}
}

// TestWithoutSkipWorldIdsEveryWorldStarts is the control: the assertions above
// only mean something if the world would otherwise have been started.
func TestWithoutSkipWorldIdsEveryWorldStarts(t *testing.T) {
	repo := &StateRepo{
		Worlds: map[string]*World{
			"migrated": testWorld(t, "migrated", "external-device-migrated"),
			"legacy":   testWorld(t, "legacy", "external-device-legacy"),
		},
		Config:      config.Config{JsTimeout: time.Second, ProtocolSegmentName: "payload"},
		StateLogger: &recordingConnectionLog{},
	}

	repo.Start()
	defer repo.Stop()

	if len(repo.changeRoutinesTickers) != 4 {
		t.Errorf("expected 4 tickers for two worlds, got %d", len(repo.changeRoutinesTickers))
	}
	if _, started := repo.externalRefDeviceIndex["external-device-migrated"]; !started {
		t.Error("expected every device to be started without a skip set")
	}
}
