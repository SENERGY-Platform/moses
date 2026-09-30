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
	"sync"
	"testing"
	"time"
)

// ReadWorlds converts every world of the caller while that world's scripts may
// be running. The conversion writes into the room and device state maps, so it
// has to hold the world mutex a script run holds; this only fails under -race.
func TestReadWorldsDoesNotRaceARunningScript(t *testing.T) {
	repo := crudTestRepo(t, &crudPersistence{})
	repo.mux.RLock()
	world := repo.Worlds["w"]
	room := world.Rooms["r"]
	device := room.Devices["d"]
	repo.mux.RUnlock()

	code := `for (var i = 0; i < 20; i++) {
		moses.world.state.set("w" + (i % 3), i);
		moses.room.state.set("r" + (i % 3), i);
		moses.device.state.set("d" + (i % 3), i);
	}`
	api := repo.getJsDeviceApi(world, room, device)

	stop := make(chan struct{})
	var scripts sync.WaitGroup
	scripts.Add(1)
	go func() {
		defer scripts.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := run(code, api, time.Second, world.mux, nil, world.Id, "race"); err != nil {
				t.Errorf("the script failed: %v", err)
				return
			}
		}
	}()

	deadline := time.Now().Add(300 * time.Millisecond)
	reads := 0
	for time.Now().Before(deadline) {
		worlds, err := repo.ReadWorlds(crudToken)
		if err != nil {
			t.Errorf("ReadWorlds: %v", err)
			break
		}
		if len(worlds) != 1 || worlds[0].Id != "w" {
			t.Errorf("expected the one world w, got %d worlds", len(worlds))
			break
		}
		reads++
	}
	close(stop)
	scripts.Wait()
	if reads == 0 {
		t.Fatal("ReadWorlds never ran")
	}
}
