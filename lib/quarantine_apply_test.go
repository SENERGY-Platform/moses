/*
 * Copyright 2019 InfAI (CC SES)
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

package lib

import (
	"context"
	"errors"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/crashbrake"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/state"
)

type qEnvironments struct {
	env    domain.Environment
	getErr error
}

func (this *qEnvironments) Put(context.Context, domain.Environment) (int64, error) { return 0, nil }
func (this *qEnvironments) PutIfVersion(context.Context, domain.Environment, int64) (int64, error) {
	return 0, nil
}
func (this *qEnvironments) Get(context.Context, string) (domain.Environment, error) {
	return this.env, this.getErr
}
func (this *qEnvironments) ListByOwner(context.Context, string) ([]domain.Environment, error) {
	return nil, nil
}
func (this *qEnvironments) All(context.Context) ([]domain.Environment, error) { return nil, nil }
func (this *qEnvironments) Delete(context.Context, string) error              { return nil }

type qStates struct {
	loaded  repo.RuntimeState
	saved   *repo.RuntimeState
	saveErr error
}

func (this *qStates) Load(context.Context, string) (repo.RuntimeState, error) {
	return this.loaded, nil
}
func (this *qStates) Save(_ context.Context, s repo.RuntimeState) error {
	if this.saveErr != nil {
		return this.saveErr
	}
	this.saved = &s
	return nil
}
func (this *qStates) Delete(context.Context, string) error { return nil }

type qHistory struct {
	record repo.HistoryJobRecord
	loaded bool
	saved  *repo.HistoryJobRecord
}

func (this *qHistory) Save(_ context.Context, r repo.HistoryJobRecord) error {
	this.saved = &r
	return nil
}
func (this *qHistory) Checkpoint(context.Context, string, repo.HistoryJobProgress) error {
	return nil
}
func (this *qHistory) Load(context.Context, string) (repo.HistoryJobRecord, error) {
	if !this.loaded {
		return repo.HistoryJobRecord{}, repo.ErrNotFound
	}
	return this.record, nil
}
func (this *qHistory) Running(context.Context) ([]repo.HistoryJobRecord, error) { return nil, nil }
func (this *qHistory) Delete(context.Context, string) error                     { return nil }

// A database error other than "gone" holds the environment back this boot and
// keeps the decision for the next one, instead of starting it into the same crash.
func TestApplyQuarantineRetriesOnADatabaseError(t *testing.T) {
	dir := t.TempDir()
	brake, _, err := crashbrake.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	envs := &qEnvironments{getErr: errors.New("mongo is down")}
	decisions := []crashbrake.Decision{{Environment: "env-a", Reason: "boom"}}
	_, held := applyQuarantine(context.Background(), envs, &qStates{}, &qHistory{}, &state.StateRepo{}, brake, decisions)
	if held["env-a"] == nil {
		t.Fatal("a database error must still hold the environment back this boot")
	}

	//the decision must have been kept: the next boot reads it back
	brake.Close()
	brake2, again, err := crashbrake.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake2.Close()
	if len(again) != 1 || again[0].Environment != "env-a" {
		t.Fatalf("a database error must keep the decision pending, got %#v", again)
	}
}

// Quarantining an environment fails its stored history run, so a resume cannot
// restart the crashing script and the status no longer reads running.
func TestApplyQuarantineFailsTheHistoryRun(t *testing.T) {
	history := &qHistory{loaded: true, record: repo.HistoryJobRecord{EnvironmentId: "env-a", State: "running"}}
	ok := applyEnvironmentQuarantine(context.Background(), &qStates{}, history, crashbrake.Decision{Environment: "env-a", Reason: "boom"})
	if !ok {
		t.Fatal("the quarantine should have applied")
	}
	if history.saved == nil || history.saved.State != "failed" {
		t.Fatalf("the history run was not failed: %#v", history.saved)
	}
}

// A decision the store refuses is still returned, so the runtime holds the
// environment back this boot, and it is kept pending with the version it was made
// against.
func TestApplyQuarantineHoldsADecisionTheStoreRefused(t *testing.T) {
	dir := t.TempDir()
	brake, _, err := crashbrake.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	envs := &qEnvironments{env: domain.Environment{Id: "env-a", Version: 7}}
	states := &qStates{saveErr: errors.New("write refused")}
	decisions := []crashbrake.Decision{{Environment: "env-a", Channel: "ch-1", Reason: "boom"}}
	_, held := applyQuarantine(context.Background(), envs, states, &qHistory{}, &state.StateRepo{}, brake, decisions)
	if q := held["env-a"]; q == nil || q.Channel != "ch-1" {
		t.Fatalf("the refused decision must still hold the environment back, got %#v", held)
	}
	brake.Close()
	brake2, again, err := crashbrake.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake2.Close()
	if len(again) != 1 || again[0].Environment != "env-a" || again[0].Version != 7 {
		t.Fatalf("the decision must stay pending with version 7, got %#v", again)
	}
}

// A retried decision made against another version is dropped: the environment
// was edited since the crash, and the edit may be the fix.
func TestApplyQuarantineDropsARetryForAnEditedEnvironment(t *testing.T) {
	dir := t.TempDir()
	brake, _, err := crashbrake.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	envs := &qEnvironments{env: domain.Environment{Id: "env-a", Version: 4}}
	states := &qStates{}
	decisions := []crashbrake.Decision{{Environment: "env-a", Reason: "boom", Version: 3}}
	_, held := applyQuarantine(context.Background(), envs, states, &qHistory{}, &state.StateRepo{}, brake, decisions)
	if len(held) != 0 || states.saved != nil {
		t.Fatalf("a stale decision must neither hold nor store a quarantine, got %#v / %#v", held, states.saved)
	}
	brake.Close()
	brake2, again, err := crashbrake.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer brake2.Close()
	if len(again) != 0 {
		t.Fatalf("a stale decision must not stay pending, got %#v", again)
	}
}
