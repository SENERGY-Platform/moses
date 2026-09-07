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
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// historyJobWindowStart is the window every fixture below runs against. Whole
// milliseconds throughout: the store keeps a time to the millisecond, and a
// fixture with nanoseconds would fail the round trip for a reason that has
// nothing to do with the record.
var historyJobWindowStart = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// historyJobRecord carries every value kind a run stores, so a field that does
// not survive the store fails here rather than in a resume.
func historyJobRecord(environmentId string, state string) HistoryJobRecord {
	finishedAt := time.Date(2026, 3, 2, 11, 22, 33, int(444*time.Millisecond), time.UTC)
	position := historyJobWindowStart.Add(3 * time.Hour)
	return HistoryJobRecord{
		EnvironmentId: environmentId,
		State:         state,
		From:          historyJobWindowStart,
		To:            historyJobWindowStart.Add(48 * time.Hour),
		StartedAt:     time.Date(2026, 3, 2, 10, 0, 0, int(125*time.Millisecond), time.UTC),
		FinishedAt:    &finishedAt,
		Position:      &position,
		Published:     4711,
		Failed:        3,
		LastError:     "the platform refused a reading",
		Error:         "",
		Channels: []HistoryChannelRecord{
			{ChannelId: "channel-1", AssetId: "asset-1", Name: "getTemperatureService",
				Publishable: true, Published: 2880, Silent: 17, Failed: 3, LastError: "refused"},
			{ChannelId: "channel-2", AssetId: "asset-1", Name: "getEnergyService",
				Publishable: false, Reason: "no service ref", Silent: 2880},
		},
		Definition: historyJobDefinition(environmentId),
		Checkpoint: historyJobCheckpoint(environmentId, position),
		//a run this incarnation picked up twice already, which is what the guard
		//against a run that never finishes counts
		Resumes: 2,
	}
}

// historyJobDefinition is the environment the run was started against: two zones
// and nested initial states, which is what a resume rebuilds its generation
// from.
func historyJobDefinition(environmentId string) domain.Environment {
	result := testEnvironment(environmentId, "Metallbau", "owner-1")
	result.Zones = append(result.Zones, domain.Zone{
		Id:   "zone-2",
		Name: "Halle 2",
		Type: domain.ZoneHall,
		Tags: []string{"logistics"},
		InitialStates: map[string]interface{}{
			"humidity": 51.5,
			"nested":   map[string]interface{}{"gate": map[string]interface{}{"open": false, "count": 3}},
		},
		Zones:  []domain.Zone{},
		Assets: []domain.Asset{},
	})
	return result
}

func historyJobCheckpoint(environmentId string, position time.Time) *HistoryCheckpoint {
	return &HistoryCheckpoint{
		Position: position,
		//keyed by identity rather than by index, and a context key may carry a
		//dot, so the keys of a stored checkpoint are asserted as they come
		Ticks: map[string]int64{
			"context:outdoor_temperature": 361,
			"context:tarif.day":           12,
			"channel-1":                   360,
			"channel-1:publish":           120,
		},
		Channels: map[string]HistoryChannelMemory{
			"channel-1": {
				Pending:         21.5,
				PendingSet:      true,
				LastAttemptUnix: position.Unix() - 30,
				Held: []FrozenHold{
					{Index: 0, BeginUnix: position.Unix() - 600, Value: 230.25},
					{Index: 2, BeginUnix: position.Unix() - 60, Value: 0},
				},
				Published: 2880, Silent: 17, Failed: 3, LastError: "refused",
			},
			"channel-2": {
				Pending:         "running",
				PendingSet:      true,
				LastAttemptUnix: 0,
				Silent:          2880,
			},
		},
		LastValues: map[string]float64{"channel-1": 21.5, "asset-1.kwh": 290508.57080252626},
		State: RuntimeState{
			EnvironmentId: environmentId,
			Context: map[string]interface{}{
				"outdoor_temperature": 12.5,
				"nested":              map[string]interface{}{"inner": map[string]interface{}{"deep": 1.25}},
			},
			Zones:          map[string]map[string]interface{}{"zone-1": {"humidity": 40.0}},
			Assets:         map[string]map[string]interface{}{"asset-1": {"kwh": 290508.57080252626, "rpm": int64(21)}},
			Anchors:        map[string]int64{"channel-1": 1772409600},
			LastPublished:  map[string]PublishedValue{"channel-1": {Value: 21.5, AtUnix: position.Unix() - 30}},
			ScheduleRuns:   map[string]ScheduleRun{"channel-2": {StartUnix: position.Unix() - 3600, CycleOffset: 2, PassUnix: position.Unix() - 3600, Open: true}},
			MeterExchanges: map[string]float64{"channel-1|1772409600": 1234.5},
			Approaching: map[string]map[string]Approach{
				"zone-1": {"humidity": {From: 40, Target: 55, StartUnix: position.Unix() - 120, TauSeconds: 900}},
			},
		},
	}
}

func TestHistoryJobSaveAndLoadRoundTripTheWholeRecord(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	want := historyJobRecord("env-1", HistoryJobRunning)

	if err := jobs.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	//set by the store, not by the caller
	if got.UpdatedAtUnix == 0 {
		t.Error("expected the store to stamp the write")
	}
	want.UpdatedAtUnix = got.UpdatedAtUnix
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the stored record came back changed:\nwant %#v\ngot  %#v", want, got)
	}
	//the types matter and not only the values: a resume feeds these back into
	//the js vm, which sees exactly what the registry produces
	if _, ok := got.Checkpoint.Channels["channel-1"].Pending.(float64); !ok {
		t.Errorf("expected a numeric pending value to come back as float64, got %T",
			got.Checkpoint.Channels["channel-1"].Pending)
	}
	if _, ok := got.Checkpoint.Channels["channel-2"].Pending.(string); !ok {
		t.Errorf("expected a string pending value to come back as string, got %T",
			got.Checkpoint.Channels["channel-2"].Pending)
	}
	if got.Checkpoint.Ticks["context:tarif.day"] != 12 {
		t.Errorf("a context key with a dot has to survive as a checkpoint key, got %#v", got.Checkpoint.Ticks)
	}
	//the definition is what a resume rebuilds the generation from
	if len(got.Definition.Zones) != 2 {
		t.Fatalf("the definition came back with %d zones", len(got.Definition.Zones))
	}
	nested, ok := got.Definition.Zones[1].InitialStates["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected a nested initial state, got %T", got.Definition.Zones[1].InitialStates["nested"])
	}
	if _, ok = nested["gate"].(map[string]interface{}); !ok {
		t.Errorf("expected a map one level deeper, got %T", nested["gate"])
	}
}

// A pending value of zero is not the same as no pending value: the first
// publishes a zero on resume, the second publishes nothing, and Pending alone
// cannot tell them apart.
func TestHistoryJobKeepsAZeroPendingValueApartFromNone(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	record := historyJobRecord("env-1", HistoryJobRunning)
	record.Checkpoint.Channels["channel-zero"] = HistoryChannelMemory{Pending: float64(0), PendingSet: true}
	record.Checkpoint.Channels["channel-none"] = HistoryChannelMemory{}

	if err := jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	zero := got.Checkpoint.Channels["channel-zero"]
	if !zero.PendingSet {
		t.Error("a channel with a pending zero has to come back with pending_set")
	}
	if value, ok := zero.Pending.(float64); !ok || value != 0 {
		t.Errorf("expected a pending zero to survive as float64(0), got %#v", zero.Pending)
	}
	none := got.Checkpoint.Channels["channel-none"]
	if none.PendingSet || none.Pending != nil {
		t.Errorf("a channel without a pending value has to stay empty, got %#v", none)
	}
}

func TestHistoryJobCheckpointUpdatesTheProgressAndLeavesTheDefinitionAlone(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	record := historyJobRecord("env-1", HistoryJobRunning)
	if err := jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}

	position := historyJobWindowStart.Add(9 * time.Hour)
	progress := HistoryJobProgress{
		Position:   position,
		Published:  9999,
		Failed:     7,
		LastError:  "a later failure",
		Checkpoint: *historyJobCheckpoint("env-1", position),
	}
	progress.Checkpoint.Ticks["channel-1"] = 1080
	progress.Checkpoint.State.Context["outdoor_temperature"] = 15.75
	if err := jobs.Checkpoint(ctx, "env-1", progress); err != nil {
		t.Fatal(err)
	}

	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Position == nil || !got.Position.Equal(position) {
		t.Errorf("the position has to move to the checkpoint, got %v", got.Position)
	}
	if got.Published != 9999 || got.Failed != 7 || got.LastError != "a later failure" {
		t.Errorf("the counters did not move: %d published, %d failed, %q", got.Published, got.Failed, got.LastError)
	}
	if got.Checkpoint == nil || !got.Checkpoint.Position.Equal(position) || got.Checkpoint.Ticks["channel-1"] != 1080 {
		t.Errorf("the checkpoint came back as %#v", got.Checkpoint)
	}
	if got.Checkpoint.State.Context["outdoor_temperature"] != 15.75 {
		t.Errorf("the state of the checkpoint came back as %#v", got.Checkpoint.State.Context)
	}
	//the run was started against this definition, and an edit made while it
	//runs must not change the definition it resumes against
	if !reflect.DeepEqual(got.Definition, record.Definition) {
		t.Errorf("a checkpoint must not touch the definition:\nwant %#v\ngot  %#v", record.Definition, got.Definition)
	}
	//nor the fields only the end of a run writes
	if got.State != HistoryJobRunning || !got.From.Equal(record.From) || !got.To.Equal(record.To) {
		t.Errorf("a checkpoint must not touch state or window, got %q %v %v", got.State, got.From, got.To)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(*record.FinishedAt) {
		t.Errorf("a checkpoint must not touch finished_at, got %v", got.FinishedAt)
	}
	if got.Resumes != record.Resumes {
		t.Errorf("a checkpoint must not touch the resume count, got %d", got.Resumes)
	}
	if len(got.Channels) != len(record.Channels) {
		t.Errorf("a checkpoint must not touch the channel results, got %#v", got.Channels)
	}
}

// The resume guard counts the resumes a run made without reaching a chunk
// boundary, so a boundary clears the count - a long run that keeps making
// progress must not be closed over the resumes it needed on the way.
func TestHistoryJobCheckpointAtABoundaryClearsTheResumeCount(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	record := historyJobRecord("env-1", HistoryJobRunning)
	if err := jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	position := historyJobWindowStart.Add(9 * time.Hour)
	progress := HistoryJobProgress{
		Position:   position,
		Published:  9999,
		Checkpoint: *historyJobCheckpoint("env-1", position),
		AtBoundary: true,
	}
	if err := jobs.Checkpoint(ctx, "env-1", progress); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Resumes != 0 {
		t.Errorf("a boundary has to clear the resume count, it stands at %d", got.Resumes)
	}
	//and nothing else of the record moved with it
	if got.State != HistoryJobRunning || got.Checkpoint == nil || !got.Checkpoint.Position.Equal(position) {
		t.Errorf("the boundary write touched more than the count and the checkpoint: %#v", got)
	}
	if !reflect.DeepEqual(got.Definition, record.Definition) {
		t.Errorf("the boundary write touched the definition of the run")
	}
}

// A progress without instants would otherwise store the year 1 twice: as the
// position a reader shows, and as the boundary a resume continues from - and the
// second one replays the whole window.
func TestHistoryJobCheckpointWithoutAPositionStoresNoneAndKeepsTheBoundary(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	record := historyJobRecord("env-1", HistoryJobRunning)
	boundary := record.Checkpoint.Position
	if err := jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Checkpoint(ctx, "env-1", HistoryJobProgress{Published: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Position != nil {
		t.Errorf("expected no position at all, got %v", got.Position)
	}
	if got.Published != 1 {
		t.Errorf("the counters have to move anyway, got %d", got.Published)
	}
	if got.Checkpoint == nil || !got.Checkpoint.Position.Equal(boundary) {
		t.Errorf("the last real boundary has to stay, got %#v", got.Checkpoint)
	}
}

// A checkpoint of a run nothing knows the window of cannot be stored: the caller
// has to learn that the record is gone rather than see the write succeed.
func TestHistoryJobCheckpointOfAnUnknownEnvironmentIsNotFound(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	err := store.HistoryJobs().Checkpoint(ctx, "env-unknown", HistoryJobProgress{
		Position:   historyJobWindowStart,
		Checkpoint: *historyJobCheckpoint("env-unknown", historyJobWindowStart),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	//and it must not have created anything
	if count := countDocuments(t, store, "history_jobs"); count != 0 {
		t.Errorf("a refused checkpoint must not create a document, got %d", count)
	}
}

func TestLoadingTheJobOfAnUnknownEnvironmentIsNotFound(t *testing.T) {
	store := testStore(t)
	_, err := store.HistoryJobs().Load(testContext(t), "env-unknown")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// Running is the resume list of a starting service: a run that ended must not
// be started again.
func TestHistoryJobRunningListsOnlyRunningRecords(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	for id, state := range map[string]string{
		"env-running-a": HistoryJobRunning,
		"env-running-b": HistoryJobRunning,
		"env-done":      "done",
		"env-failed":    "failed",
		"env-cancelled": "cancelled",
	} {
		if err := jobs.Save(ctx, historyJobRecord(id, state)); err != nil {
			t.Fatal(err)
		}
	}
	running, err := jobs.Running(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, record := range running {
		ids[record.EnvironmentId] = true
		if record.Checkpoint == nil || record.Checkpoint.Ticks == nil || record.Definition.Id != record.EnvironmentId {
			t.Errorf("a record to resume has to carry its checkpoint and definition, got %#v", record)
		}
	}
	if len(running) != 2 || !ids["env-running-a"] || !ids["env-running-b"] {
		t.Errorf("expected the two running records, got %#v", ids)
	}
}

func TestHistoryJobRunningIsEmptyAndNotAnErrorWithoutAnyJob(t *testing.T) {
	store := testStore(t)
	running, err := store.HistoryJobs().Running(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 || running == nil {
		t.Errorf("expected an empty list, got %#v", running)
	}
}

func TestDeletingAHistoryJobRemovesIt(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	if err := jobs.Save(ctx, historyJobRecord("env-1", HistoryJobRunning)); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Delete(ctx, "env-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Load(ctx, "env-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound after the delete, got %v", err)
	}
	if count := countDocuments(t, store, "history_jobs"); count != 0 {
		t.Errorf("expected no document left, got %d", count)
	}
}

func TestDeletingAHistoryJobToleratesAMissingId(t *testing.T) {
	store := testStore(t)
	if err := store.HistoryJobs().Delete(testContext(t), "env-unknown"); err != nil {
		t.Errorf("deleting what is not there is not an error, got %v", err)
	}
}

// A second run of the same environment replaces the first rather than adding a
// document, or Load and Running would pick an arbitrary one of the copies.
func TestSavingAHistoryJobTwiceReplacesIt(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	if err := jobs.Save(ctx, historyJobRecord("env-1", HistoryJobRunning)); err != nil {
		t.Fatal(err)
	}
	second := historyJobRecord("env-1", "done")
	second.Published = 12
	second.Checkpoint = nil
	if err := jobs.Save(ctx, second); err != nil {
		t.Fatal(err)
	}
	if count := countDocuments(t, store, "history_jobs"); count != 1 {
		t.Errorf("expected one document per environment, got %d", count)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" || got.Published != 12 {
		t.Errorf("the second write has to replace the record, got %#v", got)
	}
	//a replaced record drops what the new one does not carry: a run that ended
	//without a checkpoint must not read back with the checkpoint of the last one
	if got.Checkpoint != nil {
		t.Errorf("expected no checkpoint, got %#v", got.Checkpoint)
	}
}

func TestSavingAHistoryJobWithoutAnEnvironmentIdIsRefused(t *testing.T) {
	store := testStore(t)
	if err := store.HistoryJobs().Save(testContext(t), historyJobRecord("", HistoryJobRunning)); !errors.Is(err, ErrMissingId) {
		t.Errorf("expected ErrMissingId, got %v", err)
	}
	if err := store.HistoryJobs().Checkpoint(testContext(t), " ", HistoryJobProgress{}); !errors.Is(err, ErrMissingId) {
		t.Errorf("expected ErrMissingId, got %v", err)
	}
}

// A resume writes into these maps, and a checkpoint that was stored without one
// of them would otherwise panic on the first write rather than on the read.
func TestLoadingAJobInitialisesTheMapsOfItsCheckpoint(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	record := historyJobRecord("env-1", HistoryJobRunning)
	record.Checkpoint = &HistoryCheckpoint{Position: historyJobWindowStart}
	if err := jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Checkpoint == nil {
		t.Fatal("the checkpoint is gone")
	}
	if got.Checkpoint.Ticks == nil || got.Checkpoint.Channels == nil || got.Checkpoint.LastValues == nil {
		t.Errorf("expected initialised maps, got %#v", got.Checkpoint)
	}
	if got.Checkpoint.State.Context == nil || got.Checkpoint.State.Zones == nil || got.Checkpoint.State.Assets == nil {
		t.Errorf("expected an initialised state, got %#v", got.Checkpoint.State)
	}
	//a record without a checkpoint is a run that never reached a boundary, and
	//that difference is what decides whether it can be resumed at all
	record.Checkpoint = nil
	if err = jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	if got, err = jobs.Load(ctx, "env-1"); err != nil {
		t.Fatal(err)
	}
	if got.Checkpoint != nil {
		t.Errorf("expected no checkpoint at all, got %#v", got.Checkpoint)
	}
}

// The store keeps a time to the millisecond. A resume reads the position back
// and continues from it, so what it loses is asserted here rather than
// discovered in a series.
func TestHistoryJobTimesAreKeptToTheMillisecondAndAsInstants(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	record := historyJobRecord("env-1", HistoryJobRunning)
	//a whole millisecond in another zone: the instant has to survive, the zone
	//does not have to
	local := time.Date(2026, 3, 1, 1, 0, 0, int(250*time.Millisecond), berlin)
	record.From = local
	//below a millisecond the store rounds
	fraction := historyJobWindowStart.Add(1500 * time.Microsecond)
	record.Checkpoint.Position = fraction
	if err = jobs.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.From.Equal(local) {
		t.Errorf("the instant moved: %v against %v", got.From, local)
	}
	if got.Checkpoint.Position.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("expected the store to keep milliseconds at most, got %v", got.Checkpoint.Position)
	}
	if got.Checkpoint.Position.Unix() != fraction.Unix() {
		t.Errorf("the second of the position moved: %v against %v", got.Checkpoint.Position, fraction)
	}
}

// Two writers on one record are the ordinary case while a run checkpoints and
// the api asks for the status; the unique index and the retry of the upsert are
// what keep it one document.
func TestConcurrentHistoryJobWritesKeepOneDocument(t *testing.T) {
	store := testStore(t)
	ctx := testContext(t)
	jobs := store.HistoryJobs()
	record := historyJobRecord("env-1", HistoryJobRunning)

	wg := sync.WaitGroup{}
	failures := make(chan error, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			//the record is only read here, never written: Save copies before it
			//stamps
			if err := jobs.Save(ctx, record); err != nil {
				failures <- err
			}
			position := historyJobWindowStart.Add(time.Duration(i) * time.Hour)
			err := jobs.Checkpoint(ctx, "env-1", HistoryJobProgress{
				Position:   position,
				Published:  int64(i),
				Checkpoint: *historyJobCheckpoint("env-1", position),
			})
			//a checkpoint that arrives before the first save is legitimately
			//refused; anything else is not
			if err != nil && !errors.Is(err, ErrNotFound) {
				failures <- err
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if count := countDocuments(t, store, "history_jobs"); count != 1 {
		t.Errorf("expected one document per environment, got %d", count)
	}
	got, err := jobs.Load(ctx, "env-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EnvironmentId != "env-1" || got.Definition.Id != "env-1" {
		t.Errorf("the record came back as %#v", got)
	}
}
