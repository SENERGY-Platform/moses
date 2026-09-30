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
)

// storedRoutines returns the change routines the given ref holds in a stored world.
func storedRoutines(t *testing.T, world World, refType string) map[string]ChangeRoutine {
	t.Helper()
	switch refType {
	case "world":
		return world.ChangeRoutines
	case "room":
		return world.Rooms["r"].ChangeRoutines
	case "device":
		return world.Rooms["r"].Devices["d"].ChangeRoutines
	}
	t.Fatalf("unknown ref type %v", refType)
	return nil
}

func TestDeleteChangeRoutineRemovesTheRoutine(t *testing.T) {
	for _, ref := range crudRefs {
		t.Run(ref.refType, func(t *testing.T) {
			persistence := &crudPersistence{}
			repo := crudTestRepo(t, persistence)

			_, access, exists, err := repo.DeleteChangeRoutine(crudToken, ref.routineId)
			if err != nil || !access || !exists {
				t.Fatalf("delete: err=%v access=%v exists=%v", err, access, exists)
			}

			stored := persistence.persistedWorlds()
			if len(stored) != 1 {
				t.Fatalf("expected the delete to store the world once, got %d", len(stored))
			}
			routines := storedRoutines(t, stored[0], ref.refType)
			if _, ok := routines[ref.routineId]; ok {
				t.Error("the stored world still holds the deleted routine")
			}
			//the routines of the other refs are untouched
			for _, other := range crudRefs {
				if other.refType == ref.refType {
					continue
				}
				if _, ok := storedRoutines(t, stored[0], other.refType)[other.routineId]; !ok {
					t.Errorf("deleting %v also removed %v", ref.routineId, other.routineId)
				}
			}

			if _, indexed := repo.getChangeRoutineFromIndex(ref.routineId); indexed {
				t.Error("the deleted routine is still indexed")
			}
			_, _, exists, err = repo.ReadChangeRoutine(crudToken, ref.routineId)
			if err != nil || exists {
				t.Errorf("expected the deleted routine to read as not found, got exists=%v err=%v", exists, err)
			}
		})
	}
}

// A routine that ticked before the delete must not tick after it. Every run
// persists the world through state.set, so the persist count shows each run.
func TestDeleteChangeRoutineStopsTheRunningRoutine(t *testing.T) {
	for _, ref := range crudRefs {
		t.Run(ref.refType, func(t *testing.T) {
			t.Parallel()
			persistence := &crudPersistence{}
			repo := crudTestRepo(t, persistence)

			_, _, _, err := repo.UpdateChangeRoutine(crudToken, UpdateChangeRoutineRequest{Id: ref.routineId, Interval: 1, Code: `moses.world.state.set("ran", 1);`})
			if err != nil {
				t.Fatal(err)
			}
			afterUpdate := len(persistence.persistedWorlds())
			deadline := time.Now().Add(5 * time.Second)
			for len(persistence.persistedWorlds()) == afterUpdate {
				if time.Now().After(deadline) {
					t.Fatal("the routine never ran, so this test cannot tell whether the delete stops it")
				}
				time.Sleep(50 * time.Millisecond)
			}

			_, _, _, err = repo.DeleteChangeRoutine(crudToken, ref.routineId)
			if err != nil {
				t.Fatal(err)
			}
			afterDelete := len(persistence.persistedWorlds())
			//more than two intervals, so a routine still ticking would have run
			time.Sleep(2500 * time.Millisecond)
			if runs := len(persistence.persistedWorlds()) - afterDelete; runs != 0 {
				t.Fatalf("the deleted routine ran %d more times", runs)
			}
		})
	}
}
