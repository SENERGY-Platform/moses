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

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/repo"
)

// The store filters the resume list on this one string. If the two ever drift,
// every run would be stored as unresumable and nothing would say so.
func TestTheStoredRunningStateIsTheStateOfARunningRun(t *testing.T) {
	if repo.HistoryJobRunning != string(HistoryRunning) {
		t.Errorf("the store looks for %q, a running run says %q", repo.HistoryJobRunning, HistoryRunning)
	}
}

func fakeJobRecord(id string) repo.HistoryJobRecord {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return repo.HistoryJobRecord{
		EnvironmentId: id,
		State:         repo.HistoryJobRunning,
		From:          from,
		To:            from.Add(24 * time.Hour),
		StartedAt:     from,
		Definition:    testEnvironment(id),
		Checkpoint: &repo.HistoryCheckpoint{
			Position: from,
			Ticks:    map[string]int64{"ch-1": 1},
			Channels: map[string]repo.HistoryChannelMemory{"ch-1": {Pending: 1.5, PendingSet: true}},
		},
	}
}

// The engine keeps mutating the maps it checkpointed, so a fake that stored the
// same maps would let the end of a run change what an earlier boundary wrote -
// and a resume test would silently compare against the wrong state.
func TestTheFakeHistoryJobStoreCopiesWhatItIsGiven(t *testing.T) {
	jobs := newFakeHistoryJobs()
	record := fakeJobRecord("env-1")
	if err := jobs.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	record.Checkpoint.Ticks["ch-1"] = 999
	record.Checkpoint.Channels["ch-2"] = repo.HistoryChannelMemory{}
	record.Definition.Zones[0].Id = "changed"

	stored, ok := jobs.recordFor("env-1")
	if !ok {
		t.Fatal("nothing was stored")
	}
	if stored.Checkpoint.Ticks["ch-1"] != 1 || len(stored.Checkpoint.Channels) != 1 {
		t.Errorf("the stored checkpoint followed the caller's maps: %#v", stored.Checkpoint)
	}
	if stored.Definition.Zones[0].Id != testZoneId {
		t.Errorf("the stored definition followed the caller's tree: %#v", stored.Definition.Zones[0])
	}
	//set by the store, as the real one does
	if stored.UpdatedAtUnix == 0 {
		t.Error("expected the fake to stamp the write")
	}
	//and the values come back in the types mongodb would hand back
	if _, isFloat := stored.Checkpoint.Channels["ch-1"].Pending.(float64); !isFloat {
		t.Errorf("expected a numeric pending value as float64, got %T", stored.Checkpoint.Channels["ch-1"].Pending)
	}

	//Load hands out what the real one hands out, maps of the checkpoint
	//included: a resume writes into them without checking for nil
	loaded, err := jobs.Load(context.Background(), "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Checkpoint.LastValues == nil || loaded.Checkpoint.State.Context == nil ||
		loaded.Checkpoint.State.Zones == nil || loaded.Checkpoint.State.Assets == nil {
		t.Errorf("expected an initialised checkpoint, got %#v", loaded.Checkpoint)
	}
	//and it is a copy: a test that changes what it loaded must not change the store
	loaded.Checkpoint.Ticks["ch-1"] = 5
	if again, _ := jobs.recordFor("env-1"); again.Checkpoint.Ticks["ch-1"] != 1 {
		t.Errorf("Load handed out the stored maps themselves: %#v", again.Checkpoint)
	}
}

func TestTheFakeHistoryJobStoreCheckpointsLikeTheRealOne(t *testing.T) {
	jobs := newFakeHistoryJobs()
	ctx := context.Background()
	record := fakeJobRecord("env-1")
	if err := jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	position := record.From.Add(2 * time.Hour)
	progress := repo.HistoryJobProgress{
		Position:   position,
		Published:  42,
		Failed:     1,
		LastError:  "refused",
		Checkpoint: repo.HistoryCheckpoint{Position: position, Ticks: map[string]int64{"ch-1": 7200}},
	}
	if err := jobs.Checkpoint(ctx, "env-1", progress); err != nil {
		t.Fatal(err)
	}
	stored, _ := jobs.recordFor("env-1")
	if stored.Position == nil || !stored.Position.Equal(position) || stored.Published != 42 || stored.Failed != 1 {
		t.Errorf("the progress did not arrive: %#v", stored)
	}
	if stored.Checkpoint == nil || stored.Checkpoint.Ticks["ch-1"] != 7200 {
		t.Errorf("the checkpoint did not arrive: %#v", stored.Checkpoint)
	}
	//a checkpoint never rewrites the definition of the run
	if stored.Definition.Id != "env-1" || len(stored.Definition.Zones) != 1 {
		t.Errorf("the definition was touched: %#v", stored.Definition)
	}
	if jobs.checkpointCount() != 1 || len(jobs.checkpointsOf("env-1")) != 1 {
		t.Errorf("expected one recorded checkpoint, got %d", jobs.checkpointCount())
	}
	if err := jobs.Checkpoint(ctx, "env-unknown", progress); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("expected ErrNotFound for an environment without a record, got %v", err)
	}
	//the rule of the real store: a checkpoint without an instant is not a chunk
	//boundary, so the last real one stays
	if err := jobs.Checkpoint(ctx, "env-1", repo.HistoryJobProgress{Published: 43}); err != nil {
		t.Fatal(err)
	}
	stored, _ = jobs.recordFor("env-1")
	if stored.Published != 43 || stored.Position != nil {
		t.Errorf("the progress of a checkpoint without instants came back as %#v", stored)
	}
	if stored.Checkpoint == nil || stored.Checkpoint.Ticks["ch-1"] != 7200 {
		t.Errorf("the last real boundary has to stay, got %#v", stored.Checkpoint)
	}
}

// The resume guard counts the resumes that reached no chunk boundary, so the
// fake has to clear the count exactly where the real store does: at a boundary
// and nowhere else.
func TestTheFakeHistoryJobStoreClearsTheResumeCountOnlyAtABoundary(t *testing.T) {
	jobs := newFakeHistoryJobs()
	ctx := context.Background()
	record := fakeJobRecord("env-1")
	record.Resumes = 2
	if err := jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	position := record.From.Add(time.Hour)
	progress := repo.HistoryJobProgress{
		Position:   position,
		Checkpoint: repo.HistoryCheckpoint{Position: position, Ticks: map[string]int64{"ch-1": 3600}},
	}
	if err := jobs.Checkpoint(ctx, "env-1", progress); err != nil {
		t.Fatal(err)
	}
	if stored, _ := jobs.recordFor("env-1"); stored.Resumes != 2 {
		t.Errorf("the write of an abort left the resume count at %d, expected the stored 2", stored.Resumes)
	}
	progress.AtBoundary = true
	if err := jobs.Checkpoint(ctx, "env-1", progress); err != nil {
		t.Fatal(err)
	}
	stored, _ := jobs.recordFor("env-1")
	if stored.Resumes != 0 {
		t.Errorf("a boundary has to clear the resume count, it stands at %d", stored.Resumes)
	}
	if stored.Checkpoint == nil || stored.Checkpoint.Ticks["ch-1"] != 3600 || stored.State != repo.HistoryJobRunning {
		t.Errorf("the boundary write touched more than the count and the checkpoint: %#v", stored)
	}
}

// This is the seam a resume test uses: it interrupts the run at the n-th chunk
// boundary, and what that boundary wrote has to be stored by then.
func TestTheFakeHistoryJobStoreCallsTheCheckpointHook(t *testing.T) {
	jobs := newFakeHistoryJobs()
	ctx := context.Background()
	if err := jobs.Save(ctx, fakeJobRecord("env-1")); err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("stopped after the second checkpoint")
	numbers := []int{}
	jobs.onCheckpoint = func(n int) error {
		numbers = append(numbers, n)
		//reads back into the fake from inside the hook, which a hook that stops
		//a run does too
		if _, ok := jobs.recordFor("env-1"); !ok {
			t.Error("the record is not readable from the hook")
		}
		if n == 2 {
			return interrupted
		}
		return nil
	}
	for i := 1; i <= 3; i++ {
		position := time.Date(2026, 3, 1, i, 0, 0, 0, time.UTC)
		err := jobs.Checkpoint(ctx, "env-1", repo.HistoryJobProgress{
			Position:   position,
			Published:  int64(i),
			Checkpoint: repo.HistoryCheckpoint{Position: position},
		})
		if i == 2 && !errors.Is(err, interrupted) {
			t.Fatalf("expected the hook's error on the second checkpoint, got %v", err)
		}
		if i != 2 && err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
		if i == 2 {
			//the boundary the hook fired at is stored: a resume continues from it
			stored, _ := jobs.recordFor("env-1")
			if stored.Position == nil || !stored.Position.Equal(position) {
				t.Errorf("the interrupted checkpoint was not stored, got %v", stored.Position)
			}
		}
	}
	if len(numbers) != 3 || numbers[0] != 1 || numbers[1] != 2 || numbers[2] != 3 {
		t.Errorf("the hook has to be called with the count of the checkpoint, got %v", numbers)
	}
}
