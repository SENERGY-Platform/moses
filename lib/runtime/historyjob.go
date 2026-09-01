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
	"fmt"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// maxHistoryTicks bounds one run. Every reading is published synchronously, so
// the number of due events is the runtime of the run; a window and a set of
// intervals that multiply out beyond this are refused before the live channels
// are stopped for it.
const maxHistoryTicks = 20_000_000

var (
	// ErrHistoryRunning is returned wherever a history run owns the environment:
	// it stands at a past instant, so a live state change, a snapshot, a command
	// or a second run would all mix the present into it.
	ErrHistoryRunning = errors.New("a history run of this environment is in progress")

	// ErrNoHistory is returned when nothing is known about a history run of this
	// environment. The registry is in memory, so this is also the honest answer
	// after a restart.
	ErrNoHistory = errors.New("nothing is known about a history run of this environment")
)

// HistoryRangeError is a window that cannot be served, with the reason. The api
// turns it into a 400.
type HistoryRangeError struct {
	Reason string
}

func (this *HistoryRangeError) Error() string { return this.Reason }

// HistoryState is where a run stands.
type HistoryState string

const (
	HistoryRunning   HistoryState = "running"
	HistoryDone      HistoryState = "done"
	HistoryFailed    HistoryState = "failed"
	HistoryCancelled HistoryState = "cancelled"
)

// HistoryStatus is the whole run. It is a copy: the reader never holds a
// reference into a run that keeps going.
//
// State done means the live simulation is running again on the state the run
// arrived at. Failed and cancelled mean it is running again on the partial state
// the run had reached, which is a consistent state of an earlier instant and not
// a rollback - see docs/history-run.md.
type HistoryStatus struct {
	EnvironmentId string       `json:"environment_id"`
	State         HistoryState `json:"state"`

	From time.Time `json:"from"`

	// To is where the run ended, which is the present at the moment it stopped
	// simulating: it moves forward past the instant of the request while the run
	// chases the time it spent simulating.
	To time.Time `json:"to"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Position is the virtual instant the run has reached.
	Position *time.Time `json:"position,omitempty"`

	// Published and Failed count the steps of the publish grids of every channel:
	// the ones that sent at least one reading, and the ones the platform refused.
	Published int64 `json:"published"`
	Failed    int64 `json:"failed,omitempty"`

	// LastError is the most recent publish failure of the run, kept so a run that
	// mostly worked still says what went wrong. Error is what broke the run
	// itself.
	LastError string `json:"last_error,omitempty"`
	Error     string `json:"error,omitempty"`

	// Channels is what became of every channel the run drove. It is filled when
	// the run ends and stays empty for one that broke, since a run that panicked
	// returns nothing. It is what tells "the environment publishes nothing" from
	// "every channel of it was unpublishable", which look identical in the
	// totals.
	Channels []HistoryChannelStatus `json:"channels,omitempty"`
}

// historyJob is one run. status is guarded by mux; everything else is written
// once before the goroutine starts.
type historyJob struct {
	mux    sync.Mutex
	status HistoryStatus
	ctx    context.Context
	cancel context.CancelFunc
}

func (this *historyJob) snapshot() HistoryStatus {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := this.status
	result.Channels = append([]HistoryChannelStatus{}, this.status.Channels...)
	if this.status.Position != nil {
		position := *this.status.Position
		result.Position = &position
	}
	if this.status.FinishedAt != nil {
		finished := *this.status.FinishedAt
		result.FinishedAt = &finished
	}
	return result
}

func (this *historyJob) update(change func(status *HistoryStatus)) {
	this.mux.Lock()
	defer this.mux.Unlock()
	change(&this.status)
}

// StartHistory replaces the live state of one environment by the one it would
// have if it had been running since from.
//
// The window and the volume are checked before anything is stopped, so a caller
// that asks for an impossible run does not interrupt the simulation for it.
// Everything after that runs with the lifecycle mutex held, up to the point
// where the run is registered and the state is replaced: a Reload or a Remove
// arriving in between would otherwise restart the channels the run just stopped.
func (this *Runtime) StartHistory(id string, from time.Time) (HistoryStatus, error) {
	from, to, err := validateHistoryWindow(from, time.Now())
	if err != nil {
		return HistoryStatus{}, err
	}

	this.mux.RLock()
	env, running := this.envs[id]
	var gen *generation
	if running {
		gen = env.gen
	}
	this.mux.RUnlock()
	if !running || gen == nil {
		return HistoryStatus{}, repo.ErrNotRunning
	}
	if err = checkHistoryVolume(gen, from, to); err != nil {
		return HistoryStatus{}, err
	}

	this.lifecycle.Lock()
	defer this.lifecycle.Unlock()
	if !this.running {
		return HistoryStatus{}, repo.ErrNotRunning
	}
	//read again under lifecycle: the environment may have been removed while the
	//window was being checked, and starting a run on it would resurrect its state
	this.mux.RLock()
	env, running = this.envs[id]
	if running {
		gen = env.gen
	}
	this.mux.RUnlock()
	if !running || gen == nil {
		return HistoryStatus{}, repo.ErrNotRunning
	}
	//and the volume is asked again, of the generation that will actually be run:
	//a reload between the two reads can have replaced an hourly document by a one
	//second one, and the check is a walk over the channels rather than work worth
	//saving
	if err = checkHistoryVolume(gen, from, to); err != nil {
		return HistoryStatus{}, err
	}

	job, err := this.registerHistory(id, from, to)
	if err != nil {
		return HistoryStatus{}, err
	}

	//the gate closes first, then the two kinds of work in flight are waited for:
	//the tickers, which end with the environment context, and the command
	//dispatches, which do not run on it at all. Only then is the state replaced,
	//so nothing of the present can land in the state the run starts from.
	env.markUnderHistory()
	this.stopRunners(id)
	env.commands.Wait()
	env.resetForHistory()
	//seeded with the window start, not with now: the run stands at from, and a
	//governed context key seeded from today would put the future into its first
	//tick
	env.seed(gen, from)

	util.Logger.Info("history run started", "environment", id, "from", from, "to", to,
		"channels", len(gen.sensors), "context_sources", len(gen.def.ContextSources))
	go this.runHistoryJob(job, env, gen, from, to)
	return job.snapshot(), nil
}

// registerHistory takes the exclusivity decision and puts the run into the
// registry. It must be called with lifecycle held.
//
// Both registries are held while it decides, history before backfill, which is
// the order every other place that holds both uses. The worker count is taken
// under the same mutex as the stop flag, so a run can never be registered after
// Stop began waiting for the workers.
func (this *Runtime) registerHistory(id string, from time.Time, to time.Time) (*historyJob, error) {
	this.historyMux.Lock()
	defer this.historyMux.Unlock()
	if this.historiesStopped {
		return nil, repo.ErrNotRunning
	}
	if previous, known := this.histories[id]; known && previous.snapshot().State == HistoryRunning {
		return nil, ErrHistoryRunning
	}

	this.backfillMux.Lock()
	backfilling := false
	if previous, known := this.backfills[id]; known && previous.snapshot().State == BackfillRunning {
		backfilling = true
	}
	this.backfillMux.Unlock()
	if backfilling {
		//the job publishes into a window of the past and the run would publish
		//into the same one, from an environment whose state it is replacing
		return nil, ErrBackfillRunning
	}

	//derived from the runtime context, so a shutdown ends the run as it ends a
	//ticker; cancel is additionally held so a deleted environment can end it
	base := this.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	job := &historyJob{
		cancel: cancel,
		status: HistoryStatus{
			EnvironmentId: id,
			State:         HistoryRunning,
			From:          from,
			To:            to,
			StartedAt:     time.Now(),
		},
	}
	job.ctx = ctx
	this.histories[id] = job
	this.historyWorkers.Add(1)
	return job, nil
}

// runHistoryJob is the run in two phases. Only the first is counted by
// historyWorkers: the second needs the lifecycle mutex, which Stop holds while
// it waits for those workers, so counting it would deadlock.
func (this *Runtime) runHistoryJob(job *historyJob, env *environment, gen *generation, from time.Time, to time.Time) {
	result, err := this.runHistoryEngine(job, env, gen, from, to)
	this.finishHistory(job, env, gen.def.Id, result, err)
}

// runHistoryEngine is the counted phase. A bug in the simulation of one
// environment must not take the service down with it, and the caller polling the
// status is the one who needs to hear about it.
func (this *Runtime) runHistoryEngine(job *historyJob, env *environment, gen *generation, from time.Time, to time.Time) (result HistoryResult, err error) {
	//registered first so that it runs last: the worker stays counted until the
	//panic above it has been turned into an error
	defer this.historyWorkers.Done()
	defer func() {
		problem := recover()
		if problem == nil {
			return
		}
		util.Logger.Error("the history run panicked", "environment", gen.def.Id, "panic", fmt.Sprint(problem))
		err = fmt.Errorf("the history run panicked: %v", problem)
	}()

	progress := func(at time.Time, published int64, failed int64, lastError string) {
		position := at
		job.update(func(current *HistoryStatus) {
			current.Position = &position
			current.Published = published
			current.Failed = failed
			current.LastError = lastError
		})
	}
	//chaseTheClock: to was the present when the run was asked for, so the run has
	//to close the time it spends simulating rather than hand over across it
	return this.historyEngine(job.ctx, env, gen, from, to, chaseTheClock, progress)
}

// finishHistory hands the environment back to the live simulation and only then
// reports the run as over. It runs after every outcome - success, failure, panic
// and cancellation alike - because an environment left marked as owned by a run
// would refuse every state change and never tick again.
//
// The order is the point. The state the run arrived at is flushed before the
// live runners start, so the ramp is on disk even if the process dies in the
// next second; the definition is read again, so an edit made during the run
// takes effect now; and the status turns to done last, which is what makes
// "done" mean "the simulation is running again".
func (this *Runtime) finishHistory(job *historyJob, env *environment, id string, result HistoryResult, runErr error) {
	//released here rather than only on an abort: a run that ended normally would
	//otherwise leave its context, and the goroutine the parent keeps for it,
	//alive for as long as the runtime runs
	defer job.cancel()

	this.lifecycle.Lock()
	defer this.lifecycle.Unlock()

	env.endHistory()
	//forced, and before the runners start: a flush that was already in flight may
	//have cleared the flag while holding a state older than this one, and the
	//write below is serialised behind it, so this is what lands last
	env.markDirty()
	this.flush(env)

	restarted := false
	if this.running {
		//not derived from this.ctx, for the reason Reload gives: a read arriving
		//while the service shuts down should find a cancelled runtime rather than
		//fail with a context error that reads like a database problem
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		def, err := this.environments.Get(ctx, id)
		cancel()
		switch {
		case errors.Is(err, repo.ErrNotFound):
			util.Logger.Info("the environment no longer exists, it is not restarted after the history run", "environment", id)
			this.removeEnvironment(id)
		case err != nil:
			//the state is flushed and the environment is released; the next reload
			//or restart starts it again
			util.Logger.Error("unable to read the environment after the history run, it is not restarted",
				attributes.ErrorKey, err, "environment", id)
		default:
			restarted = this.startEnvironment(context.Background(), def)
			this.rebuildIndex()
		}
	}

	//read off what the engine returned rather than off the context: a run that
	//finished normally milliseconds before an abort or a shutdown reached it did
	//finish, and reporting it as cancelled would say its window is incomplete
	state := HistoryDone
	message := ""
	broke := false
	switch {
	case runErr == nil:
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		//a budget that ran out ends the run the same way an abort does: the window
		//is incomplete, and nothing about the simulation broke
		state = HistoryCancelled
	default:
		state = HistoryFailed
		message = runErr.Error()
		broke = true
	}
	finished := time.Now()
	job.update(func(current *HistoryStatus) {
		current.State = state
		current.Error = message
		current.FinishedAt = &finished
		if broke {
			//a run that panicked returns nothing, so the counters the progress
			//reports left behind are the last thing known about it
			return
		}
		current.Published = result.Published
		current.Failed = result.Failed
		current.LastError = result.LastError
		current.Channels = result.Channels
		if !result.Position.IsZero() {
			position := result.Position
			current.Position = &position
		}
		if !result.End.IsZero() {
			current.To = result.End
		}
	})
	util.Logger.Info("history run finished", "environment", id, "state", string(state),
		"published", result.Published, "failed", result.Failed, "restarted", restarted)
}

// HistoryStatusOf returns what is known about the history run of one
// environment.
func (this *Runtime) HistoryStatusOf(id string) (HistoryStatus, error) {
	this.historyMux.Lock()
	job, known := this.histories[id]
	this.historyMux.Unlock()
	if !known {
		return HistoryStatus{}, ErrNoHistory
	}
	return job.snapshot(), nil
}

// CancelHistory ends a running history run and reports where it stood. It does
// not wait: the run stops at its next due event and hands the environment back
// itself, which is what leaves the simulation running on the partial state.
func (this *Runtime) CancelHistory(id string) (HistoryStatus, error) {
	this.historyMux.Lock()
	job, known := this.histories[id]
	this.historyMux.Unlock()
	if !known {
		return HistoryStatus{}, ErrNoHistory
	}
	job.cancel()
	return job.snapshot(), nil
}

// cancelHistory ends a run without reporting anything, for a caller that is
// deleting the environment.
func (this *Runtime) cancelHistory(id string) {
	this.historyMux.Lock()
	job, known := this.histories[id]
	this.historyMux.Unlock()
	if known {
		job.cancel()
	}
}

// historyRunning reports whether a run currently owns one environment. It is the
// lifecycle side of environment.underHistory: a caller holding the lifecycle
// mutex cannot take the environment mutex to ask.
func (this *Runtime) historyRunning(id string) bool {
	this.historyMux.Lock()
	job, known := this.histories[id]
	this.historyMux.Unlock()
	return known && job.snapshot().State == HistoryRunning
}

// stopHistories refuses any further run and ends the running ones. It must be
// called before Stop waits for the workers: the flag and the worker count share
// historyMux, so a run registered concurrently either sees the flag or is
// counted before the wait starts, never neither.
func (this *Runtime) stopHistories() {
	this.historyMux.Lock()
	this.historiesStopped = true
	jobs := make([]*historyJob, 0, len(this.histories))
	for _, job := range this.histories {
		jobs = append(jobs, job)
	}
	this.historyMux.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
}

// minHistorySpan is the shortest window worth the operation. A run discards the
// live state of the environment, and nobody destroys a state to reconstruct a
// few hundred milliseconds of it, so such a request is a mistake and is named as
// one rather than served.
const minHistorySpan = time.Minute

// validateHistoryWindow refuses a window that cannot be run. The end is not a
// parameter: a run always ends at the present, because its result is the live
// state.
func validateHistoryWindow(from time.Time, now time.Time) (time.Time, time.Time, error) {
	if from.IsZero() {
		return from, now, &HistoryRangeError{Reason: "from is required, as an RFC3339 timestamp"}
	}
	if !from.Before(now) {
		return from, now, &HistoryRangeError{Reason: "from has to lie in the past; a history run ends at the present"}
	}
	if now.Sub(from) < minHistorySpan {
		return from, now, &HistoryRangeError{Reason: fmt.Sprintf(
			"the window spans %v, less than the %v a history run covers; it replaces the live state, which is not worth doing for that", now.Sub(from), minHistorySpan)}
	}
	if from.Before(minBackfillTime) {
		return from, now, &HistoryRangeError{Reason: "from lies before " + minBackfillTime.Format(time.RFC3339) + ", which is not a window of this platform"}
	}
	if now.Sub(from) > MaxBackfillSpan {
		return from, now, &HistoryRangeError{Reason: fmt.Sprintf(
			"the window spans %v, more than the %v a history run covers", now.Sub(from), MaxBackfillSpan)}
	}
	return from, now, nil
}

// checkHistoryVolume refuses a run before the live channels are stopped for it.
//
// Every channel that computes is counted, whether or not it can publish: the run
// executes it either way, and the cost of the run is the number of due events
// rather than the number of readings. A channel publishing on change is counted
// on its evaluation grid, a split channel on both of its grids, and the context
// sources on theirs.
//
// The steps the run adds while it chases the clock at the end are not counted
// here; historyChasesOn bounds them instead, to about twice the pass they
// follow.
func checkHistoryVolume(gen *generation, from time.Time, to time.Time) error {
	refuse := &HistoryRangeError{Reason: fmt.Sprintf(
		"this window and the intervals of this environment come to more than %d simulation steps; start later or widen the intervals",
		maxHistoryTicks)}

	total := int64(0)
	for _, binding := range gen.sensors {
		total += historyTicksOf(binding, from, to)
		if total > maxHistoryTicks {
			return refuse
		}
	}
	for _, source := range gen.def.ContextSources {
		total += backfillTicks(source.IntervalSeconds, from, to)
		if total > maxHistoryTicks {
			return refuse
		}
	}
	return nil
}

// historyTicksOf is how many due events one channel has over the window, on the
// same grids runHistory puts it on.
func historyTicksOf(binding channelBinding, from time.Time, to time.Time) int64 {
	if binding.cov != nil {
		//the heartbeat rides on the evaluation grid rather than having one of its
		//own, exactly as it does in the run
		return backfillTicks(binding.cov.evalSeconds, from, to)
	}
	total := int64(0)
	if binding.sourceInterval > 0 {
		total += backfillTicks(binding.sourceInterval, from, to)
	}
	if channelPublishes(binding.channel) {
		total += backfillTicks(binding.channel.IntervalSeconds, from, to)
	}
	return total
}
