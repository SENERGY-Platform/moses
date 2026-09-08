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
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/timeseries"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// maxHistoryTicks bounds one run. Every reading is published synchronously and
// the publish pool sends at most as many at once as it has workers, so the
// number of due events still bounds the runtime of the run; a window and a set
// of intervals that multiply out beyond this are refused before the live
// channels are stopped for it.
const maxHistoryTicks = 20_000_000

var (
	// ErrHistoryRunning is returned wherever a history run owns the environment:
	// it stands at a past instant, so a live state change, a snapshot, a command
	// or a second run would all mix the present into it.
	ErrHistoryRunning = errors.New("a history run of this environment is in progress")

	// ErrNoHistory is returned when nothing is known about a history run of this
	// environment: neither the registry in memory nor the store has one.
	ErrNoHistory = errors.New("nothing is known about a history run of this environment")
)

// HistoryRangeError is a window that cannot be served, with the reason. The api
// turns it into a 400.
type HistoryRangeError struct {
	Reason string
}

func (this *HistoryRangeError) Error() string { return this.Reason }

// OccupiedDevice is one platform device of the environment whose first day of
// the window already holds readings. Name is the name of the asset it belongs
// to, and empty where the document gives it none.
type OccupiedDevice struct {
	DeviceId string
	Name     string
}

// String names one device the way every refusal names it, the api's 409 body
// included, so the two cannot drift apart.
func (this OccupiedDevice) String() string {
	if this.Name == "" {
		return "device " + this.DeviceId
	}
	return "device " + this.DeviceId + " (" + this.Name + ")"
}

// HistoryOccupiedError is a window whose first day already holds readings for at
// least one device of the environment: an earlier run over the same window, or a
// window that reaches into live data. The api turns it into a 409, and `force`
// starts the run anyway - the rows cannot be deleted, so the decision to write
// them a second time is the caller's.
type HistoryOccupiedError struct {
	Devices []OccupiedDevice
}

func (this *HistoryOccupiedError) Error() string {
	named := make([]string, 0, len(this.Devices))
	for _, device := range this.Devices {
		named = append(named, device.String())
	}
	return "the first day of the window already holds readings for " + strings.Join(named, ", ")
}

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

// historyJob is one run. status and abortAsked are guarded by mux; everything
// else is written once before the goroutine starts.
type historyJob struct {
	mux    sync.Mutex
	status HistoryStatus
	ctx    context.Context
	cancel context.CancelFunc

	// record is what the store holds for this run: what Save wrote at the start,
	// or what a resume was built from. The definition is in there, so finishing
	// can write the whole document again without reading it back.
	record repo.HistoryJobRecord

	// abortAsked tells an abort somebody asked for from the cancellation a
	// shutdown causes. Only the second one suspends the run for a resume.
	abortAsked bool

	// removed says the environment of this run is being deleted, which decides
	// what becomes of its record: it is deleted rather than written back.
	removed bool
}

// beginAbort marks a run somebody asked to stop, so a shutdown arriving
// afterwards does not store it as suspended and resume it on the next start, and
// reports whether it was still running. Both under the one mutex, so a run that
// ended in between is reported as it ended rather than being stored as cancelled
// over its own outcome.
func (this *historyJob) beginAbort() (HistoryStatus, bool) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.status.State != HistoryRunning {
		return this.statusCopy(), false
	}
	this.abortAsked = true
	return this.statusCopy(), true
}

func (this *historyJob) aborted() bool {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.abortAsked
}

// markRemovedWithEnvironment is the deletion path's mark: the run is aborted and
// its record goes with the environment. Without it a shutdown racing the
// deletion would upsert the record, definition and all, for an environment that
// is not there any more.
func (this *historyJob) markRemovedWithEnvironment() {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.abortAsked = true
	this.removed = true
}

func (this *historyJob) removedWithEnvironment() bool {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.removed
}

func (this *historyJob) definition() domain.Environment {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.record.Definition
}

func (this *historyJob) snapshot() HistoryStatus {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.statusCopy()
}

// statusCopy is the copy itself, for a caller that holds mux: nothing of it may
// share a slice or a pointer with the run that keeps going.
func (this *historyJob) statusCopy() HistoryStatus {
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

// historyStatusOfRecord and historyRecordOf are the mapping between the run in
// memory and the run in the store, one function each way and nowhere else: a GET
// after a restart is served out of the record, and everything a run reports about
// itself is stored through the same shape.
func historyStatusOfRecord(record repo.HistoryJobRecord) HistoryStatus {
	result := HistoryStatus{
		EnvironmentId: record.EnvironmentId,
		State:         HistoryState(record.State),
		From:          record.From,
		To:            record.To,
		StartedAt:     record.StartedAt,
		Published:     record.Published,
		Failed:        record.Failed,
		LastError:     record.LastError,
		Error:         record.Error,
	}
	if record.FinishedAt != nil {
		finished := *record.FinishedAt
		result.FinishedAt = &finished
	}
	if record.Position != nil {
		position := *record.Position
		result.Position = &position
	}
	for _, channel := range record.Channels {
		result.Channels = append(result.Channels, HistoryChannelStatus{
			ChannelId:   channel.ChannelId,
			AssetId:     channel.AssetId,
			Name:        channel.Name,
			Publishable: channel.Publishable,
			Reason:      channel.Reason,
			Published:   channel.Published,
			Silent:      channel.Silent,
			Failed:      channel.Failed,
			LastError:   channel.LastError,
		})
	}
	return result
}

// historyRecordOf carries no checkpoint: it is the shape of a run that is over,
// and a finished run has nothing left to resume from.
func historyRecordOf(status HistoryStatus, definition domain.Environment) repo.HistoryJobRecord {
	result := repo.HistoryJobRecord{
		EnvironmentId: status.EnvironmentId,
		State:         string(status.State),
		From:          status.From,
		To:            status.To,
		StartedAt:     status.StartedAt,
		Published:     status.Published,
		Failed:        status.Failed,
		LastError:     status.LastError,
		Error:         status.Error,
		Definition:    definition,
	}
	if status.FinishedAt != nil {
		finished := *status.FinishedAt
		result.FinishedAt = &finished
	}
	if status.Position != nil {
		position := *status.Position
		result.Position = &position
	}
	for _, channel := range status.Channels {
		result.Channels = append(result.Channels, repo.HistoryChannelRecord{
			ChannelId:   channel.ChannelId,
			AssetId:     channel.AssetId,
			Name:        channel.Name,
			Publishable: channel.Publishable,
			Reason:      channel.Reason,
			Published:   channel.Published,
			Silent:      channel.Silent,
			Failed:      channel.Failed,
			LastError:   channel.LastError,
		})
	}
	return result
}

// StartHistory replaces the live state of one environment by the one it would
// have if it had been running since from. The window, the volume, the grid
// identities and whether the first day of the window already holds readings are
// checked before anything is stopped, so a caller that asks for an impossible
// run does not interrupt the simulation for it; force skips the occupancy check
// alone, and token is the caller's Authorization value for it, with the owner's
// exchanged token as the fallback. Everything from the registration on runs
// with the lifecycle mutex held: a Reload or a Remove arriving in between would
// otherwise restart the channels the run just stopped.
func (this *Runtime) StartHistory(id string, from time.Time, force bool, token string) (HistoryStatus, error) {
	from, to, err := validateHistoryWindow(from, time.Now())
	if err != nil {
		return HistoryStatus{}, err
	}

	//a stopped runtime is asked before the wrapper is: during a rollout a POST
	//would otherwise spend the whole check budget on queries for an environment
	//that cannot run, and answer 409 for it
	this.lifecycle.Lock()
	alive := this.running
	this.lifecycle.Unlock()
	if !alive {
		return HistoryStatus{}, repo.ErrNotRunning
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
	if err = checkHistoryGrids(gen); err != nil {
		return HistoryStatus{}, err
	}
	//a run or a backfill that already owns the environment is named out of the
	//registries rather than after a fan-out whose answer nobody reads; the
	//registration below decides it again, under lifecycle
	if busy := this.historyExclusivity(id); busy != nil {
		return HistoryStatus{}, busy
	}
	if !force {
		//the wrapper is asked here and not again under lifecycle: it is a query
		//per channel over the network, and holding lifecycle for that would stop
		//every other Start, Stop, Reload and Remove for as long as it takes. The
		//context is not derived from the runtime's, so the answer does not depend
		//on a shutdown racing the request; the run is registered under lifecycle
		//afterwards, which is where a stopped runtime refuses it.
		checkCtx, cancelCheck := context.WithTimeout(context.Background(), historyOccupiedTimeout)
		err = this.checkHistoryOccupied(checkCtx, gen, from, token)
		cancelCheck()
		if err != nil {
			return HistoryStatus{}, err
		}
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
	if err = checkHistoryGrids(gen); err != nil {
		return HistoryStatus{}, err
	}

	record := repo.HistoryJobRecord{
		EnvironmentId: id,
		State:         string(HistoryRunning),
		From:          from,
		To:            to,
		StartedAt:     time.Now(),
		Definition:    gen.def,
	}
	job, err := this.registerHistory(record)
	if err != nil {
		return HistoryStatus{}, err
	}
	//stored before anything is stopped, and the run is refused if that fails: a
	//run the store does not know cannot be resumed, and at this point the live
	//simulation is still running, so refusing costs nothing
	storeCtx, cancelStore := context.WithTimeout(context.Background(), storeTimeout)
	err = this.historyJobs.Save(storeCtx, record)
	cancelStore()
	if err != nil {
		this.unregisterHistory(id, job)
		return HistoryStatus{}, err
	}

	util.Logger.Info("history run started", "environment", id, "from", from, "to", to,
		"channels", len(gen.sensors), "context_sources", len(gen.def.ContextSources))
	this.beginHistoryRun(job, env, gen, from, to, nil)
	return job.snapshot(), nil
}

// beginHistoryRun takes the environment away from the live simulation and starts
// the run on it, for a request as for a resume. Must be called with lifecycle
// held, the run registered and its record stored. The gate closes first, then
// the tickers and the command dispatches in flight are waited for, and only then
// is the state replaced, so nothing of the present lands in it.
func (this *Runtime) beginHistoryRun(job *historyJob, env *environment, gen *generation, from time.Time, to time.Time, resume *repo.HistoryCheckpoint) {
	env.markUnderHistory()
	this.stopRunners(env.id)
	env.commands.Wait()
	env.resetForHistory()
	if resume != nil {
		//the checkpoint carries the seeded state of the interrupted run and
		//everything it computed after it, so seeding again would say nothing
		env.restoreForHistory(*resume)
	} else {
		//seeded with the window start, not with now: the run stands at from, and a
		//governed context key seeded from today would put the future into its
		//first tick
		env.seed(gen, from)
	}
	go this.runHistoryJob(job, env, gen, from, to, resume)
}

// registerHistory takes the exclusivity decision and puts the run into the
// registry. It must be called with lifecycle held.
//
// Both registries are held while it decides, history before backfill, which is
// the order every other place that holds both uses. The worker count is taken
// under the same mutex as the stop flag, so a run can never be registered after
// Stop began waiting for the workers.
func (this *Runtime) registerHistory(record repo.HistoryJobRecord) (*historyJob, error) {
	id := record.EnvironmentId
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
	//taken from the record rather than built here, so a resumed run reports the
	//counters and the position it had reached instead of starting at zero
	status := historyStatusOfRecord(record)
	status.State = HistoryRunning
	status.FinishedAt = nil
	job := &historyJob{cancel: cancel, status: status, record: record}
	job.ctx = ctx
	this.histories[id] = job
	this.historyWorkers.Add(1)
	return job, nil
}

// historyExclusivity peeks what registerHistory decides, for a caller that has
// not stopped anything yet: a run or a backfill that owns the environment is
// named here rather than after a fan-out of wrapper queries whose answer is
// then thrown away. It reads the same two registries in the same order and
// takes neither decision, so a run registered in between is still refused where
// it counts.
func (this *Runtime) historyExclusivity(id string) error {
	this.historyMux.Lock()
	running, known := this.histories[id]
	this.historyMux.Unlock()
	if known && running.snapshot().State == HistoryRunning {
		return ErrHistoryRunning
	}
	this.backfillMux.Lock()
	backfilling, known := this.backfills[id]
	this.backfillMux.Unlock()
	if known && backfilling.snapshot().State == BackfillRunning {
		return ErrBackfillRunning
	}
	return nil
}

// unregisterHistory takes a registered run back out, for a start that refuses it
// after the registration. Nothing has been stopped for it at that point, so the
// live simulation carries on as if the run had never been asked for.
func (this *Runtime) unregisterHistory(id string, job *historyJob) {
	this.historyMux.Lock()
	if this.histories[id] == job {
		delete(this.histories, id)
	}
	this.historyMux.Unlock()
	this.historyWorkers.Done()
	job.cancel()
}

// runHistoryJob is the run in two phases. Only the first is counted by
// historyWorkers: the second needs the lifecycle mutex, which Stop holds while
// it waits for those workers, so counting it would deadlock.
func (this *Runtime) runHistoryJob(job *historyJob, env *environment, gen *generation, from time.Time, to time.Time, resume *repo.HistoryCheckpoint) {
	result, err := this.runHistoryEngine(job, env, gen, from, to, resume)
	this.finishHistory(job, env, gen.def.Id, result, err)
}

// runHistoryEngine is the counted phase. A bug in the simulation of one
// environment must not take the service down with it, and the caller polling the
// status is the one who needs to hear about it.
func (this *Runtime) runHistoryEngine(job *historyJob, env *environment, gen *generation, from time.Time, to time.Time, resume *repo.HistoryCheckpoint) (result HistoryResult, err error) {
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
	id := gen.def.Id
	checkpoint := func(progress repo.HistoryJobProgress) error {
		//deliberately not the run's context: the checkpoint of an abort or a
		//shutdown is written after that context has been cancelled, and it is the
		//one that says where a resume continues
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		return this.historyJobs.Checkpoint(ctx, id, progress)
	}
	//chaseTheClock: to was the present when the run was asked for, so the run has
	//to close the time it spends simulating rather than hand over across it
	return this.historyEngine(job.ctx, env, gen, from, to, chaseTheClock, progress, resume, checkpoint)
}

// finishHistory stores the outcome, hands the environment back to the live
// simulation and only then reports the run as over. It runs after every outcome,
// because an environment left owned by a run would never tick again.
//
// The order is the point: the state is flushed before the runners start, the
// terminal record is written before the handover because the handover can take
// minutes and a record left running that long would be resumed by the next
// start as a run that is over, and the status turns last, which is what makes
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

	//a shutdown does not end the run, it suspends it: the last checkpoint is where
	//the next start continues, so the stored record stays running. An abort
	//somebody asked for is not a suspension, whatever the runtime is doing at the
	//time, or a deleted or aborted run would come back on every start.
	suspended := !this.running && errors.Is(runErr, context.Canceled) && !job.aborted()
	//and the record of an environment that is being deleted goes rather than being
	//written back, whether or not this runtime is still running: read here, so a
	//shutdown racing the deletion cannot upsert the definition of an environment
	//that is gone
	removed := job.removedWithEnvironment()

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
	//built before it is published: the engine has returned, so nothing else writes
	//this status, and the store is written while it still says running - a caller
	//that reads done and then asks again after a restart has to find the same
	//outcome, not a document of a run that was still going
	final := job.snapshot()
	final.State = state
	final.Error = message
	final.FinishedAt = &finished
	if !broke {
		//a run that panicked returns nothing, so the counters the progress reports
		//left behind are the last thing known about it
		final.Published = result.Published
		final.Failed = result.Failed
		final.LastError = result.LastError
		final.Channels = result.Channels
		if !result.Position.IsZero() {
			position := result.Position
			final.Position = &position
		}
		if !result.End.IsZero() {
			final.To = result.End
		}
	}

	switch {
	case suspended:
		//the record stays as it is, running and with its last checkpoint: that is
		//what the next start continues from
		util.Logger.Info("history run suspended for restart", "environment", id,
			"position", final.Position, "from", final.From, "to", final.To,
			"published", final.Published)
	case removed:
		//no document of a run of an environment that is not there any more. This is
		//the second delete the deletion path already does, from the one place that
		//knows a write of this run is still to come.
		this.forgetHistoryRecord(id)
	default:
		this.storeFinishedHistory(id, final, job.definition())
	}

	restarted := false
	if this.running && !removed {
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
			//the definition was deleted while the run was ending, so the record that
			//was just written goes again
			this.forgetHistoryRecord(id)
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

	if suspended {
		//the status stays running, exactly as the store has it: the next start
		//continues this run, and this process is on its way out
		return
	}
	job.update(func(current *HistoryStatus) { *current = final })
	util.Logger.Info("history run finished", "environment", id, "state", string(state),
		"published", result.Published, "failed", result.Failed, "restarted", restarted)
}

// historyTerminalSaveWaits are the waits before the second and third attempt at
// the terminal write, historyTerminalSaveTimeout the budget of one; three
// attempts because that write keeps the next start from resuming a run that is
// over, each bounded well below storeTimeout because the retry holds lifecycle.
var historyTerminalSaveWaits = []time.Duration{200 * time.Millisecond, time.Second}

const historyTerminalSaveTimeout = 3 * time.Second

// storeFinishedHistory writes the run as it ended, retrying a failure. A write
// that still does not land is an ERROR and nothing more: the environment goes
// back to the live simulation either way, and the status in memory is what a
// caller of this instance reads.
func (this *Runtime) storeFinishedHistory(id string, status HistoryStatus, definition domain.Environment) {
	record := historyRecordOf(status, definition)
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), historyTerminalSaveTimeout)
		err := this.historyJobs.Save(ctx, record)
		cancel()
		if err == nil {
			return
		}
		if attempt >= len(historyTerminalSaveWaits) {
			util.Logger.Error("unable to store the finished history run", attributes.ErrorKey, err,
				"environment", id, "state", string(status.State), "attempts", attempt+1)
			return
		}
		util.Logger.Warn("unable to store the finished history run, trying again",
			attributes.ErrorKey, err, "environment", id, "state", string(status.State), "attempt", attempt+1)
		time.Sleep(historyTerminalSaveWaits[attempt])
	}
}

func (this *Runtime) forgetHistoryRecord(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := this.historyJobs.Delete(ctx, id); err != nil {
		util.Logger.Error("unable to delete the history run of an environment that is gone",
			attributes.ErrorKey, err, "environment", id)
	}
}

// maxHistoryResumes is how often a run may be picked up again without reaching a
// chunk boundary in between - a run that reaches one has got going, and the store
// clears the count there. A run that takes the service down on every start would
// otherwise be resumed for ever, and a window nobody can finish is worth less
// than a service that starts.
const maxHistoryResumes = 3

// resumableHistories is the list of runs this start continues. It runs before
// the environments are built, so an environment whose run is resumed starts no
// live runner that would publish the present into the window; a job list that
// cannot be read leaves the service starting normally with nothing resumed.
func (this *Runtime) resumableHistories(ctx context.Context) []repo.HistoryJobRecord {
	records, err := this.historyJobs.Running(ctx)
	if err != nil {
		//already reported by the store, with the reason
		return nil
	}
	result := make([]repo.HistoryJobRecord, 0, len(records))
	for _, record := range records {
		if record.Resumes >= maxHistoryResumes {
			this.closeExhaustedHistory(record)
			continue
		}
		result = append(result, record)
	}
	return result
}

// closeExhaustedHistory ends a run that has been resumed as often as it may be.
// Its environment starts live as any other, since nothing is going to take it
// away again.
func (this *Runtime) closeExhaustedHistory(record repo.HistoryJobRecord) {
	finished := time.Now()
	record.State = string(HistoryFailed)
	record.Error = "the run was resumed three times without reaching a chunk boundary"
	record.FinishedAt = &finished
	record.Checkpoint = nil
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := this.historyJobs.Save(ctx, record); err != nil {
		util.Logger.Error("unable to close a history run that was resumed too often",
			attributes.ErrorKey, err, "environment", record.EnvironmentId)
		return
	}
	util.Logger.Error("a history run was resumed three times without reaching a chunk boundary, it is closed as failed",
		"environment", record.EnvironmentId, "resumes", record.Resumes)
}

// resumeHistories continues the runs the start collected. It must be called with
// lifecycle held, once the environments are up and before the flusher runs: a
// flush in between would write the live state the run is about to replace.
func (this *Runtime) resumeHistories(ctx context.Context, records []repo.HistoryJobRecord) {
	for _, record := range records {
		this.resumeHistory(ctx, record)
	}
}

// resumeHistory picks one stored run up again. The generation is built from the
// definition the run was started against - an edit made meanwhile takes effect at
// the handover, which reads the current one - while the datasets behind it are
// loaded fresh.
func (this *Runtime) resumeHistory(ctx context.Context, record repo.HistoryJobRecord) {
	id := record.EnvironmentId
	this.mux.RLock()
	env, running := this.envs[id]
	this.mux.RUnlock()
	if !running || env == nil {
		//nothing to continue on, and what that means decides whether the run ends
		this.closeHistoryOfGoneEnvironment(record)
		return
	}

	seriesCtx, cancelSeries := context.WithTimeout(ctx, seriesLoadTimeout)
	gen := newGeneration(record.Definition, this.loadSeries(seriesCtx, record.Definition))
	cancelSeries()

	job, err := this.registerHistory(record)
	if err != nil {
		util.Logger.Error("unable to resume the history run", attributes.ErrorKey, err, "environment", id)
		//the start built this environment for the run and gave it no runners, so it
		//is handed to the live simulation here - otherwise it would stand still
		//until the next edit
		this.giveUpOnResuming(env)
		return
	}

	//counted after the registration and before the run starts: a resume that never
	//happened must not spend one of the three the guard allows, and a run that
	//takes the service down while it is picked up has to be counted before it can.
	//A write that fails only loses the count: the run is worth more than the guard.
	record.Resumes++
	countCtx, cancelCount := context.WithTimeout(context.Background(), storeTimeout)
	countErr := this.historyJobs.Save(countCtx, record)
	cancelCount()
	if countErr != nil {
		util.Logger.Error("unable to count the resume of the history run, it is resumed anyway",
			attributes.ErrorKey, countErr, "environment", id)
	}
	position := record.From
	if record.Checkpoint != nil {
		position = record.Checkpoint.Position
	}
	this.beginHistoryRun(job, env, gen, record.From, record.To, record.Checkpoint)
	util.Logger.Info("history run resumed", "environment", id, "position", position,
		"from", record.From, "to", record.To, "checkpointed", record.Checkpoint != nil)
}

// giveUpOnResuming starts the live simulation of an environment the start left
// under a run that then could not be resumed. The generation it already carries
// is the current definition, which is what the start built it from.
func (this *Runtime) giveUpOnResuming(env *environment) {
	this.mux.RLock()
	gen := env.gen
	this.mux.RUnlock()
	if gen == nil {
		return
	}
	env.endHistory()
	this.startEnvironment(context.Background(), gen.def)
	this.rebuildIndex()
}

// closeHistoryOfGoneEnvironment ends the stored run of an environment this
// service is not running - but only when its definition is really gone. An
// environment that merely failed to start comes back on the next start, and its
// run is still resumable.
func (this *Runtime) closeHistoryOfGoneEnvironment(record repo.HistoryJobRecord) {
	id := record.EnvironmentId
	readCtx, cancelRead := context.WithTimeout(context.Background(), storeTimeout)
	_, err := this.environments.Get(readCtx, id)
	cancelRead()
	switch {
	case errors.Is(err, repo.ErrNotFound):
	case err != nil:
		util.Logger.Warn("unable to check whether the environment of a stored history run still exists, the run is left as it is",
			attributes.ErrorKey, err, "environment", id)
		return
	default:
		util.Logger.Warn("the environment of a stored history run exists but is not running here, the run is left as it is",
			"environment", id)
		return
	}

	finished := time.Now()
	record.State = string(HistoryCancelled)
	record.Error = "the environment no longer exists"
	record.FinishedAt = &finished
	record.Checkpoint = nil
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := this.historyJobs.Save(ctx, record); err != nil {
		util.Logger.Error("unable to close the history run of a missing environment",
			attributes.ErrorKey, err, "environment", record.EnvironmentId)
		return
	}
	util.Logger.Info("the environment of a stored history run is not running here, the run is closed as cancelled",
		"environment", record.EnvironmentId)
}

// loadHistoryRecord is the fallback of the two status calls: after a restart the
// registry knows nothing, and the store is what still does. ErrNotFound is the
// one error that becomes ErrNoHistory; anything else is handed on, so a store
// that is unreachable is not reported as "there was no run".
func (this *Runtime) loadHistoryRecord(id string) (repo.HistoryJobRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	record, err := this.historyJobs.Load(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.HistoryJobRecord{}, ErrNoHistory
	}
	if err != nil {
		return repo.HistoryJobRecord{}, err
	}
	return record, nil
}

// HistoryStatusOf returns what is known about the history run of one
// environment, out of the registry or, for a run of a previous incarnation, out
// of the store.
func (this *Runtime) HistoryStatusOf(id string) (HistoryStatus, error) {
	this.historyMux.Lock()
	job, known := this.histories[id]
	this.historyMux.Unlock()
	if known {
		return job.snapshot(), nil
	}
	record, err := this.loadHistoryRecord(id)
	if err != nil {
		return HistoryStatus{}, err
	}
	return historyStatusOfRecord(record), nil
}

// CancelHistory ends a running history run and reports where it stood. It does
// not wait: the run stops at its next due event and hands the environment back
// itself. A stored run the registry does not know is closed in the store; after
// a Start that resumed every running record it cannot still be going.
func (this *Runtime) CancelHistory(id string) (HistoryStatus, error) {
	this.historyMux.Lock()
	job, known := this.histories[id]
	this.historyMux.Unlock()
	if known {
		//marked before the cancellation, or a shutdown racing it could still read
		//the run as suspended and resume it on the next start. A run that is over
		//is only reported: storing cancelled over it would rewrite the outcome of
		//a run that really finished, which is what the store branch below does not
		//do either.
		status, running := job.beginAbort()
		if !running {
			return status, nil
		}
		//stored before the cancellation too, rather than left to the terminal write
		//at the end of the run: a write that fails there would leave the record
		//running, and the next start would resume a run somebody stopped. The
		//terminal write replaces this with the full result.
		this.storeAbortedHistory(job)
		job.cancel()
		return job.snapshot(), nil
	}

	record, err := this.loadHistoryRecord(id)
	if err != nil {
		return HistoryStatus{}, err
	}
	status := historyStatusOfRecord(record)
	if status.State != HistoryRunning {
		return status, nil
	}
	finished := time.Now()
	status.State = HistoryCancelled
	status.FinishedAt = &finished
	stored := historyRecordOf(status, record.Definition)
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err = this.historyJobs.Save(ctx, stored); err != nil {
		util.Logger.Error("unable to close a stored history run this runtime does not run",
			attributes.ErrorKey, err, "environment", id)
		return HistoryStatus{}, err
	}
	return status, nil
}

// storeAbortedHistory writes the run as cancelled where it stands, before the
// run is cancelled, so a terminal write that fails afterwards cannot leave a
// running record; Running() filters on the state, so the engine's own abort
// checkpoint written into it afterwards resumes nothing. A failure is an ERROR:
// the abort is decided either way.
func (this *Runtime) storeAbortedHistory(job *historyJob) {
	status := job.snapshot()
	finished := time.Now()
	status.State = HistoryCancelled
	status.FinishedAt = &finished
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := this.historyJobs.Save(ctx, historyRecordOf(status, job.definition())); err != nil {
		util.Logger.Error("unable to store the abort of the history run, its record stays as it is",
			attributes.ErrorKey, err, "environment", status.EnvironmentId)
	}
}

// cancelHistory ends a run without reporting anything, for a caller that is
// deleting the environment.
func (this *Runtime) cancelHistory(id string) {
	this.historyMux.Lock()
	job, known := this.histories[id]
	this.historyMux.Unlock()
	if known {
		//the environment is going away, so this run must not be resumed even if the
		//service is shutting down at the same time, and its record is deleted
		//rather than written back when the run ends
		job.markRemovedWithEnvironment()
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
	//truncated to the precision a bson datetime keeps: the window of the record a
	//resume runs against has to be the window the run itself used, or every
	//instant of the resumed half would sit a fraction of a millisecond off
	from = from.Truncate(time.Millisecond)
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

// checkHistoryGrids refuses an environment whose grids a checkpoint could not
// tell apart, before the live state is thrown away for it: the document itself
// validates - a channel called "context:k" next to a context source k, say - so
// the answer is a 400 rather than a run that fails after stopping the runners.
func checkHistoryGrids(gen *generation) error {
	if err := historyDistinctGrids(historyGridsOf(gen, historyContextKeys(gen))); err != nil {
		return &HistoryRangeError{Reason: err.Error()}
	}
	return nil
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

// The bounds of the occupancy check: the first day from the window start is what
// tells an earlier run over the same window from a window that reaches into live
// data, while the end of any window of an environment that ever ran live always
// holds readings. The queries are limit-1 reads, so the fan-out is what the
// wrapper feels rather than the work, and the whole check has to answer inside
// the api server's write timeout, since a caller waits on the POST for it.
const (
	historyOccupiedSpan    = 24 * time.Hour
	historyOccupiedQueries = 16
	historyOccupiedTimeout = 5 * time.Second
)

// ErrHistoryCheckTimeout is a check that did not finish inside its budget. It is
// its own error because the api answers it with a 503: nothing is known about
// the window, and the caller either retries or forces the run.
var ErrHistoryCheckTimeout = errors.New("the timescale did not answer in time, so the history window could not be checked; send force: true to start the run without the check")

// occupancyQuery is one channel's question, kept in the order of the generation
// so a refusal names the devices in the order of the document.
type occupancyQuery struct {
	deviceId  string
	serviceId string
	column    string
	assetId   string
}

// checkHistoryOccupied refuses a run whose first day already holds readings, for
// every channel that could publish one, and only for a run somebody asked for:
// a resume continues a run that wrote into its own window on purpose. A 400 or
// a 404 the wrapper answers for one channel leaves that channel unchecked with a
// WARN, since a column its validator rejects or a device the token cannot read
// would otherwise block every run of that environment. Every other failure -
// another status, a network error, a spent budget, a token exchange that failed
// - refuses the run, because the check matters exactly where something was
// written before.
func (this *Runtime) checkHistoryOccupied(ctx context.Context, gen *generation, from time.Time, token string) error {
	if this.fetcher == nil || (token == "" && this.ownerToken == nil) {
		util.Logger.Warn("the history window is not checked against the timescale, no wrapper is configured",
			"environment", gen.def.Id)
		return nil
	}

	queries := []occupancyQuery{}
	for _, binding := range gen.sensors {
		shape, _, publishable, err := this.historyChannelShape(binding)
		if err != nil {
			//a device repository that cannot be read says nothing about this
			//channel, and a window nobody could check is what the refusal is for
			return fmt.Errorf("unable to check the history window against the timescale: %w", err)
		}
		if !publishable {
			//a channel that cannot publish writes nothing, so nothing of it can
			//collide - and asking about it would need a time shape it has none of
			continue
		}
		queries = append(queries, occupancyQuery{
			deviceId:  binding.asset.externalRef,
			serviceId: binding.channel.ExternalRef,
			//the flattened column name the ingestion writes: the root prefixes
			//the value path, exactly as the platform origin queries it
			column:  shape.RootName + "." + strings.Join(shape.ValuePath, "."),
			assetId: binding.asset.id,
		})
	}
	if len(queries) == 0 {
		return nil
	}

	end := from.Add(historyOccupiedSpan)
	ownerAsked := false
	if token == "" {
		//no caller token: a resume or an internal call reads with the owner's,
		//the way the platform origin does
		exchanged, err := this.ownerToken(gen.def.Owner)
		if err != nil {
			return fmt.Errorf("unable to check the history window against the timescale: %w", err)
		}
		token, ownerAsked = exchanged, true
	}

	occupied := make([]bool, len(queries))
	failures := make([]error, len(queries))
	all := make([]int, len(queries))
	for i := range all {
		all[i] = i
	}
	this.askOccupancy(ctx, queries, all, token, from, end, occupied, failures)

	//a 404 is a device this token cannot read, and the devices of an environment
	//are created with the token of whoever provisioned them - so the owner's
	//exchanged token is the second try, exchanged once and only where a channel
	//needs it. A second 404 leaves that channel unchecked below.
	denied := []int{}
	for i := range failures {
		if wrapperStatusOf(failures[i]) == http.StatusNotFound {
			denied = append(denied, i)
		}
	}
	if len(denied) > 0 && !ownerAsked && this.ownerToken != nil {
		exchanged, err := this.ownerToken(gen.def.Owner)
		if err != nil {
			return fmt.Errorf("unable to check the history window against the timescale: %w", err)
		}
		if exchanged != token {
			this.askOccupancy(ctx, queries, denied, exchanged, from, end, occupied, failures)
		}
	}

	//the first refusal in query order rather than the first to arrive, so the
	//answer reads the same way on every run
	var refusal error
	for i := range failures {
		if failures[i] == nil {
			continue
		}
		status := wrapperStatusOf(failures[i])
		switch {
		case errors.Is(failures[i], context.DeadlineExceeded), errors.Is(failures[i], context.Canceled):
			//a spent budget wins over every other failure: with the queries cut
			//off nothing is known about the window, and the answer says so
			//rather than naming one wrapper call
			return ErrHistoryCheckTimeout
		case status == http.StatusBadRequest, status == http.StatusNotFound:
			util.Logger.Warn("the timescale-wrapper refused the history window query, this channel is not checked",
				attributes.ErrorKey, failures[i], "environment", gen.def.Id,
				"device", queries[i].deviceId, "service", queries[i].serviceId, "status", status)
		case refusal == nil:
			refusal = fmt.Errorf("unable to check the history window against the timescale: %w", failures[i])
		}
	}
	if refusal != nil {
		return refusal
	}

	names := assetNamesOf(gen.def)
	result := &HistoryOccupiedError{}
	reported := map[string]bool{}
	for i, query := range queries {
		//one entry per device: several channels of one device hold readings of
		//the same device, and naming it twice says nothing more
		if !occupied[i] || reported[query.deviceId] {
			continue
		}
		reported[query.deviceId] = true
		result.Devices = append(result.Devices, OccupiedDevice{
			DeviceId: query.deviceId, Name: names[query.assetId],
		})
	}
	if len(result.Devices) > 0 {
		return result
	}
	return nil
}

// askOccupancy asks the wrapper about the queries the indices name, at most
// historyOccupiedQueries of them at a time, and writes every answer at the
// query's own index - so a second pass over a subset overwrites exactly those.
// Every goroutine writes one element of its own, which is what lets the caller
// read both slices once they have all returned.
func (this *Runtime) askOccupancy(ctx context.Context, queries []occupancyQuery, indices []int, token string, from time.Time, end time.Time, occupied []bool, failures []error) {
	slots := make(chan struct{}, historyOccupiedQueries)
	waiting := sync.WaitGroup{}
	for _, index := range indices {
		waiting.Add(1)
		go func(i int) {
			defer waiting.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			query := queries[i]
			//no pre-check of the budget: the client hands the context to the
			//request and reports a spent one as its own error
			occupied[i], failures[i] = this.fetcher.HasReadings(ctx, token,
				query.deviceId, query.serviceId, query.column, from, end)
		}(index)
	}
	waiting.Wait()
}

// wrapperStatusOf is the http status a wrapper answer carried, or 0 for an error
// that is not one - a network failure or a spent budget.
func wrapperStatusOf(err error) int {
	status := &timeseries.StatusError{}
	if errors.As(err, &status) {
		return status.Status
	}
	return 0
}

// assetNamesOf maps asset id to asset name, so a refusal names a device in the
// words of the document rather than by its platform id alone.
func assetNamesOf(def domain.Environment) map[string]string {
	result := map[string]string{}
	var walk func(zones []domain.Zone)
	walk = func(zones []domain.Zone) {
		for _, zone := range zones {
			for _, asset := range zone.Assets {
				result[asset.Id] = asset.Name
			}
			//nested zones carry assets too; a name missing here shows up as a
			//device without one in the 409 body
			walk(zone.Zones)
		}
	}
	walk(def.Zones)
	return result
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
