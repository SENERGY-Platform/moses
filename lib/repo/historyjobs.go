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
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// HistoryJobRecord is one history run of one environment as the store keeps it:
// the status a reader sees, the definition the run was started against, and the
// point a restarted service resumes from.
//
// Every time in here is stored as a bson datetime, so it comes back in UTC at
// millisecond precision: a resume must not depend on a finer instant.
type HistoryJobRecord struct {
	EnvironmentId string `json:"environment_id" bson:"environment_id"`

	// State is running, done, failed or cancelled - the strings of
	// runtime.HistoryState, kept as a string so the store does not depend on the
	// runtime.
	State string `json:"state" bson:"state"`

	From time.Time `json:"from" bson:"from"`
	To   time.Time `json:"to" bson:"to"`

	StartedAt  time.Time  `json:"started_at" bson:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty" bson:"finished_at,omitempty"`

	// Position is the virtual instant the run has reached.
	Position *time.Time `json:"position,omitempty" bson:"position,omitempty"`

	Published int64  `json:"published" bson:"published"`
	Failed    int64  `json:"failed,omitempty" bson:"failed,omitempty"`
	LastError string `json:"last_error,omitempty" bson:"last_error,omitempty"`
	Error     string `json:"error,omitempty" bson:"error,omitempty"`

	Channels []HistoryChannelRecord `json:"channels,omitempty" bson:"channels,omitempty"`

	// Definition is the environment as it was when the run started; a resume runs
	// against it.
	Definition domain.Environment `json:"definition" bson:"definition"`

	Checkpoint *HistoryCheckpoint `json:"checkpoint,omitempty" bson:"checkpoint,omitempty"`

	// Resumes counts how often a start has picked this run up again without the
	// run reaching a chunk boundary since. It is the guard against a run that
	// breaks the service on every start: after three such resumes the run is
	// closed as failed instead of being tried a fourth time.
	Resumes int64 `json:"resumes,omitempty" bson:"resumes,omitempty"`

	// Set by the store, not by callers.
	UpdatedAtUnix int64 `json:"updated_at_unix" bson:"updated_at_unix"`
}

// HistoryChannelRecord is what became of one channel of the run, the stored
// counterpart of runtime.HistoryChannelStatus.
type HistoryChannelRecord struct {
	ChannelId string `json:"channel_id" bson:"channel_id"`
	AssetId   string `json:"asset_id" bson:"asset_id"`
	Name      string `json:"name" bson:"name"`

	Publishable bool   `json:"publishable" bson:"publishable"`
	Reason      string `json:"reason,omitempty" bson:"reason,omitempty"`

	Published int64  `json:"published" bson:"published"`
	Silent    int64  `json:"silent,omitempty" bson:"silent,omitempty"`
	Failed    int64  `json:"failed,omitempty" bson:"failed,omitempty"`
	LastError string `json:"last_error,omitempty" bson:"last_error,omitempty"`
}

// HistoryCheckpoint is where a run stood at a chunk boundary, complete enough to
// continue from.
type HistoryCheckpoint struct {
	Position time.Time `json:"position" bson:"position"`

	// Ticks is the next step of every grid, keyed by "context:<key>",
	// "<channel id>" or "<channel id>:publish".
	Ticks map[string]int64 `json:"ticks" bson:"ticks"`

	// Channels is keyed by channel id.
	Channels map[string]HistoryChannelMemory `json:"channels" bson:"channels"`

	LastValues map[string]float64 `json:"last_values,omitempty" bson:"last_values,omitempty"`

	State RuntimeState `json:"state" bson:"state"`
}

// Initialise gives a checkpoint the maps a resume writes into, so it can do that
// without checking for nil first. Load does it for what it reads, and anything
// standing in for the store has to do the same. Nil safe: a run that never
// reached a boundary has no checkpoint.
func (this *HistoryCheckpoint) Initialise() {
	if this == nil {
		return
	}
	if this.Ticks == nil {
		this.Ticks = map[string]int64{}
	}
	if this.Channels == nil {
		this.Channels = map[string]HistoryChannelMemory{}
	}
	if this.LastValues == nil {
		this.LastValues = map[string]float64{}
	}
	if this.State.Context == nil {
		this.State.Context = map[string]interface{}{}
	}
	if this.State.Zones == nil {
		this.State.Zones = map[string]map[string]interface{}{}
	}
	if this.State.Assets == nil {
		this.State.Assets = map[string]map[string]interface{}{}
	}
}

// HistoryChannelMemory is what a channel of a run remembers outside the
// environment state.
type HistoryChannelMemory struct {
	// PendingSet tells a channel that has no pending value from one whose
	// pending value is a zero or an empty string, which look the same in Pending
	// alone.
	Pending    interface{} `json:"pending,omitempty" bson:"pending,omitempty"`
	PendingSet bool        `json:"pending_set" bson:"pending_set"`

	LastAttemptUnix int64 `json:"last_attempt_unix" bson:"last_attempt_unix"`

	Held []FrozenHold `json:"held,omitempty" bson:"held,omitempty"`

	Published int64  `json:"published" bson:"published"`
	Silent    int64  `json:"silent" bson:"silent"`
	Failed    int64  `json:"failed" bson:"failed"`
	LastError string `json:"last_error,omitempty" bson:"last_error,omitempty"`
}

// FrozenHold is one frozen fault occurrence a channel is holding: the fault's
// index in the channel's list, when the occurrence began, and the value it
// reports.
type FrozenHold struct {
	Index     int     `json:"index" bson:"index"`
	BeginUnix int64   `json:"begin_unix" bson:"begin_unix"`
	Value     float64 `json:"value" bson:"value"`
}

// HistoryJobs stores one history run per environment, so a restarted service can
// resume it.
type HistoryJobs interface {
	// Save writes the whole record, replacing what is stored for the
	// environment. Used at start and at the end of a run.
	Save(ctx context.Context, record HistoryJobRecord) error

	// Checkpoint updates position, counters, last error and the checkpoint of the
	// stored record without rewriting the definition; ErrNotFound when none exists.
	// A Checkpoint without a position leaves the stored one alone, since resuming
	// from an instant-less checkpoint would replay the whole window; AtBoundary
	// resets the resume count.
	Checkpoint(ctx context.Context, environmentId string, progress HistoryJobProgress) error

	// Load returns the record of one environment, ErrNotFound when there is none.
	// The maps of a stored checkpoint come back initialised, so a resume can
	// write into them without checking for nil first.
	Load(ctx context.Context, environmentId string) (HistoryJobRecord, error)

	// Running returns every record in state running.
	Running(ctx context.Context) ([]HistoryJobRecord, error)

	// Delete tolerates a missing id.
	Delete(ctx context.Context, environmentId string) error
}

// HistoryJobProgress is what one chunk boundary reports: the counters are the
// running totals of the run, not the increments of the chunk.
type HistoryJobProgress struct {
	Position   time.Time
	Published  int64
	Failed     int64
	LastError  string
	Checkpoint HistoryCheckpoint

	// AtBoundary tells a chunk boundary from the write an abort makes. Only a
	// boundary clears the resume count: a run that reached one has got going, so
	// the guard counts resumes that reached no boundary rather than resumes ever.
	AtBoundary bool
}
