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
	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// A backfill computes an environment over a window that has already passed and
// publishes the readings with the timestamps they would have had, so a model
// can be trained on weeks of data as soon as the environment is defined. It
// runs the same arithmetic as the live simulation, driven by a different
// clock - profileValue and replayValue are functions of the instant alone, so
// seed plus window determine the result - but touches nothing the live
// simulation owns; see runBackfillChannel.

// MaxBackfillSpan bounds one window. A year and a day, so that "the last twelve
// months" is expressible without the caller having to reason about leap years.
const MaxBackfillSpan = 366 * 24 * time.Hour

// maxBackfillEvents bounds one job. Every reading is published synchronously,
// because the postgres write only happens synchronously under Sync qos, so the
// number of events is the runtime of the job. A window and an interval that
// multiply out beyond this are refused up front rather than accepted into a job
// that would still be running tomorrow.
const maxBackfillEvents = 2_000_000

// backfillFutureTolerance is how far ahead of this server's clock a window may
// end. A caller computes "now" on its own clock, so an exactly-now window would
// otherwise be a coin toss.
const backfillFutureTolerance = time.Minute

// backfillLogEvery is how often a running channel reports progress.
const backfillLogEvery = 10000

// minBackfillTime keeps a window inside the range int64 nanoseconds can express
// (1678..2262), which the event time entry relies on, and rejects the mistyped
// year that would otherwise be accepted as a valid window.
var minBackfillTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var (
	// ErrBackfillRunning is returned when a job for this environment is still
	// running. Two jobs over overlapping windows would write two readings per
	// instant, and timescale keeps both.
	ErrBackfillRunning = errors.New("a backfill of this environment is already running")

	// ErrNoBackfill is returned when nothing is known about a backfill of this
	// environment. The registry is in memory, so this is also the honest answer
	// after a restart: the job may have completed, may have been interrupted
	// halfway, and this instance cannot tell which.
	ErrNoBackfill = errors.New("nothing is known about a backfill of this environment")
)

// BackfillRangeError is a window that cannot be served, with the reason. The api
// turns it into a 400.
type BackfillRangeError struct {
	Reason string
}

func (this *BackfillRangeError) Error() string { return this.Reason }

// BackfillState is where a job stands.
type BackfillState string

const (
	BackfillRunning   BackfillState = "running"
	BackfillDone      BackfillState = "done"
	BackfillFailed    BackfillState = "failed"
	BackfillCancelled BackfillState = "cancelled"
)

// BackfillChannelStatus is what became of one channel of the environment. A
// channel that was not backfilled says why, because "no data appeared" is
// otherwise indistinguishable from a channel that published nothing.
type BackfillChannelStatus struct {
	ChannelId string `json:"channel_id"`
	AssetId   string `json:"asset_id"`
	Name      string `json:"name"`

	// Backfillable is false when SkipReason says why not.
	Backfillable bool   `json:"backfillable"`
	SkipReason   string `json:"skip_reason,omitempty"`

	// Published counts the readings that reached the platform, Silent the steps
	// that sent nothing - a tick that produced no value at all (a dataset
	// outside its own time range), or one whose value did not move far enough
	// for a channel publishing on change - and Failed the ones the platform
	// refused. The three add up to the steps of the channel's grid.
	Published int64 `json:"published"`
	Silent    int64 `json:"silent,omitempty"`
	Failed    int64 `json:"failed,omitempty"`

	// LastError is the most recent publish failure of this channel, kept so a
	// job that mostly worked still says what went wrong.
	LastError string `json:"last_error,omitempty"`
}

// BackfillStatus is the whole job. It is a copy: the reader never holds a
// reference into a job that keeps running.
type BackfillStatus struct {
	EnvironmentId string        `json:"environment_id"`
	State         BackfillState `json:"state"`

	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// ChannelsTotal counts every channel of the environment, backfillable or
	// not; ChannelsDone counts the ones that are finished with.
	ChannelsTotal int `json:"channels_total"`
	ChannelsDone  int `json:"channels_done"`

	// CurrentChannel and Position are where the job stands right now.
	CurrentChannel string     `json:"current_channel,omitempty"`
	Position       *time.Time `json:"position,omitempty"`

	Published int64  `json:"published"`
	Error     string `json:"error,omitempty"`

	Channels []BackfillChannelStatus `json:"channels"`
}

// backfillJob is one running job. status is guarded by mux; everything else is
// written once before the goroutine starts.
type backfillJob struct {
	mux    sync.Mutex
	status BackfillStatus
	cancel context.CancelFunc
	done   chan struct{}
}

func (this *backfillJob) snapshot() BackfillStatus {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := this.status
	result.Channels = append([]BackfillChannelStatus{}, this.status.Channels...)
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

func (this *backfillJob) update(change func(status *BackfillStatus)) {
	this.mux.Lock()
	defer this.mux.Unlock()
	change(&this.status)
}

// StartBackfill validates a window and starts a job for it.
//
// The validation is synchronous and works on the definition alone, so a caller
// learns about an impossible window at once. Which channels can actually take a
// historical timestamp is decided inside the job, because it needs the device
// types and that is a network read per asset.
func (this *Runtime) StartBackfill(id string, from time.Time, to time.Time) (BackfillStatus, error) {
	from, to, err := validateBackfillWindow(from, to, time.Now())
	if err != nil {
		return BackfillStatus{}, err
	}

	this.mux.RLock()
	env, running := this.envs[id]
	var gen *generation
	if running {
		gen = env.gen
	}
	this.mux.RUnlock()
	if !running || gen == nil {
		return BackfillStatus{}, repo.ErrNotRunning
	}

	channels := backfillChannels(gen.def)
	if err = checkBackfillVolume(channels, from, to); err != nil {
		return BackfillStatus{}, err
	}

	//both registries under one nesting, history first, so that a run and a job
	//starting at the same moment cannot both see the other as absent. Every
	//other place that holds both takes them in this order.
	this.historyMux.Lock()
	defer this.historyMux.Unlock()
	if previous, known := this.histories[id]; known && previous.snapshot().State == HistoryRunning {
		//the environment stands at a past instant and its readings are being
		//written for that window; a job publishing into the same window on top of
		//it would leave two rows per instant
		return BackfillStatus{}, ErrHistoryRunning
	}

	this.backfillMux.Lock()
	//the stop flag and the worker count are taken under one mutex on purpose:
	//Stop sets the flag before it waits, so a job can never be registered after
	//that wait began
	if this.backfillsStopped {
		this.backfillMux.Unlock()
		return BackfillStatus{}, repo.ErrNotRunning
	}
	if previous, known := this.backfills[id]; known && previous.snapshot().State == BackfillRunning {
		this.backfillMux.Unlock()
		return BackfillStatus{}, ErrBackfillRunning
	}
	//derived from the runtime context, so a shutdown ends the job as it ends a
	//ticker; cancel is additionally held so a deleted environment can end it
	base := this.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	job := &backfillJob{
		cancel: cancel,
		done:   make(chan struct{}),
		status: BackfillStatus{
			EnvironmentId: id,
			State:         BackfillRunning,
			From:          from,
			To:            to,
			StartedAt:     time.Now(),
			ChannelsTotal: len(channels),
			Channels:      []BackfillChannelStatus{},
		},
	}
	this.backfills[id] = job
	this.backfillWorkers.Add(1)
	this.backfillMux.Unlock()

	util.Logger.Info("backfill started", "environment", id, "from", from, "to", to, "channels", len(channels))
	go this.runBackfill(ctx, job, gen, channels, from, to)
	return job.snapshot(), nil
}

// BackfillStatusOf returns what is known about the backfill of one environment.
func (this *Runtime) BackfillStatusOf(id string) (BackfillStatus, error) {
	this.backfillMux.Lock()
	job, known := this.backfills[id]
	this.backfillMux.Unlock()
	if !known {
		return BackfillStatus{}, ErrNoBackfill
	}
	return job.snapshot(), nil
}

// cancelBackfill ends a running job of one environment. It does not wait: the
// caller is deleting the environment and the job stops at its next reading.
func (this *Runtime) cancelBackfill(id string) {
	this.backfillMux.Lock()
	job, known := this.backfills[id]
	this.backfillMux.Unlock()
	if !known {
		return
	}
	job.cancel()
}

// stopBackfills refuses any further job and ends the running ones. It must be
// called before Stop waits for the workers: the flag and the worker count share
// backfillMux, so a job registered concurrently either sees the flag or is
// counted before the wait starts, never neither.
func (this *Runtime) stopBackfills() {
	this.backfillMux.Lock()
	this.backfillsStopped = true
	jobs := make([]*backfillJob, 0, len(this.backfills))
	for _, job := range this.backfills {
		jobs = append(jobs, job)
	}
	this.backfillMux.Unlock()
	for _, job := range jobs {
		job.cancel()
	}
}

// validateBackfillWindow refuses a window that cannot produce usable training
// data, and clamps one that only overshoots the present by a clock skew.
func validateBackfillWindow(from time.Time, to time.Time, now time.Time) (time.Time, time.Time, error) {
	if from.IsZero() || to.IsZero() {
		return from, to, &BackfillRangeError{Reason: "from and to are both required, as RFC3339 timestamps"}
	}
	if !from.Before(to) {
		return from, to, &BackfillRangeError{Reason: "from has to lie before to"}
	}
	if from.Before(minBackfillTime) {
		return from, to, &BackfillRangeError{Reason: "from lies before " + minBackfillTime.Format(time.RFC3339) + ", which is not a window of this platform"}
	}
	if to.After(now.Add(backfillFutureTolerance)) {
		return from, to, &BackfillRangeError{Reason: "to lies in the future; a backfill writes readings that were already taken"}
	}
	if to.After(now) {
		//within the tolerance: the caller's clock is simply ahead of this one
		to = now
	}
	if !from.Before(to) {
		return from, to, &BackfillRangeError{Reason: "the window is empty once it is cut off at the present"}
	}
	if to.Sub(from) > MaxBackfillSpan {
		return from, to, &BackfillRangeError{Reason: fmt.Sprintf("the window spans %v, more than the %v a backfill covers", to.Sub(from), MaxBackfillSpan)}
	}
	return from, to, nil
}

// checkBackfillVolume refuses a job before it starts rather than after it has
// been running for a day. The count is an upper bound, since it assumes every
// counted channel is backfillable and the job can only publish fewer. Only the
// skip reasons that follow from the definition alone are asked here, exactly as
// the job will ask them, so a channel the job refuses does not spend the
// reading budget; the remaining reasons need the loaded series or a device type
// read and cannot be asked before the job exists, but they only ever remove
// channels, so leaving them out keeps this an upper bound.
func checkBackfillVolume(channels []backfillChannel, from time.Time, to time.Time) error {
	total := int64(0)
	for _, channel := range channels {
		if backfillSkipsByDefinition(channel.channel) != "" {
			continue
		}
		total += backfillTicks(backfillStepSeconds(channel.channel), from, to)
		if total > maxBackfillEvents {
			return &BackfillRangeError{Reason: fmt.Sprintf(
				"this window and the channel intervals of this environment come to more than %d readings; shorten the window or widen the intervals",
				maxBackfillEvents)}
		}
	}
	return nil
}

// backfillStepSeconds is the grid one channel is reconstructed on: its publish
// interval, or the evaluation interval of a usable change trigger. The volume
// check counts on this grid too, since a channel publishing on change can, in
// the worst case, publish on every evaluation, and counting only heartbeats
// would wave through a job that publishes far more than it promised.
func backfillStepSeconds(channel domain.Channel) int64 {
	if cov, usable, _ := covOf(channel); usable {
		return cov.evalSeconds
	}
	return channel.IntervalSeconds
}

// backfillTicks is how many readings one channel produces over the window: the
// tick at from, plus one per whole interval that fits. Zero for a channel that
// does not tick on a schedule.
func backfillTicks(intervalSeconds int64, from time.Time, to time.Time) int64 {
	if intervalSeconds <= 0 || intervalSeconds > maxIntervalSeconds {
		return 0
	}
	//integer seconds throughout: a duration in float would round at the far end
	//of a year long window
	return (to.Unix()-from.Unix())/intervalSeconds + 1
}

// backfillChannel is one channel of the definition, with the parts of its
// surroundings the job needs.
type backfillChannel struct {
	zoneId  string
	assetId string
	// externalDeviceRef is the asset's platform device.
	externalDeviceRef string
	channel           domain.Channel
}

// backfillChannels walks the definition rather than the running generation, so
// that a channel the runtime dropped - a dataset whose file is gone, a source
// kind it does not execute - is still reported with a reason instead of
// silently missing from the result.
func backfillChannels(def domain.Environment) []backfillChannel {
	result := []backfillChannel{}
	var walk func(zones []domain.Zone, depth int)
	walk = func(zones []domain.Zone, depth int) {
		if depth > domain.MaxZoneDepth {
			return
		}
		for _, zone := range zones {
			for _, asset := range zone.Assets {
				for _, channel := range asset.Channels {
					result = append(result, backfillChannel{
						zoneId:            zone.Id,
						assetId:           asset.Id,
						externalDeviceRef: asset.ExternalRef,
						channel:           channel,
					})
				}
			}
			walk(zone.Zones, depth+1)
		}
	}
	walk(def.Zones, 1)
	return result
}

// backfillSkipsByDefinition is the half of skipReason that follows from the
// channel definition alone: direction, schedule and source kind. It is split out
// so that the volume check can ask exactly the question the job will ask, before
// a job exists - see checkBackfillVolume - rather than a second, slightly
// different one that would drift away from this one.
func backfillSkipsByDefinition(channel domain.Channel) string {
	source := channel.Source
	if channel.Direction != domain.Sensor || channel.IntervalSeconds <= 0 {
		return "the channel does not publish on a schedule, so there is no series to reconstruct"
	}
	if channel.IntervalSeconds > maxIntervalSeconds {
		return "the channel interval is out of range"
	}
	switch source.Kind {
	case domain.SourceScript:
		return "a script source is stateful: its value depends on the state its earlier runs left behind, which no longer exists for a past moment"
	case domain.SourceFormula:
		return "a formula is derived from other channels and the context, and follows from them rather than being a series of its own"
	case domain.SourceAggregate:
		return "an aggregate is derived from the channels of the sub-metered assets, and follows from them rather than being a series of its own"
	case domain.SourceSchedule:
		return "a schedule stands where its persisted anchor and, with a gate, the live context put it: neither of those exists for a past moment, so a reconstructed window would be a different programme than the one that ran"
	case domain.SourceProfile:
		if source.Profile == nil {
			return "the profile source carries no profile"
		}
	case domain.SourceDataset:
		if source.Dataset == nil {
			return "the dataset source carries no dataset"
		}
	default:
		return "this source kind is not backfilled"
	}
	return ""
}

// skipReason says why a channel cannot be backfilled, or "" when it can. The
// cheap reasons come first: the last one costs a device type read.
func (this *Runtime) skipReason(channel backfillChannel, points []dataset.Point) string {
	if reason := backfillSkipsByDefinition(channel.channel); reason != "" {
		return reason
	}
	if channel.channel.Source.Kind == domain.SourceDataset {
		if len(points) < 2 {
			return "the dataset of this channel is not loaded, so there is nothing to replay"
		}
		if points[len(points)-1].Unix <= points[0].Unix {
			//replayValue divides the elapsed time by this span
			return "the dataset of this channel covers no time span"
		}
	}
	if channel.externalDeviceRef == "" {
		return "the asset has no platform device, so a reading has nowhere to go"
	}
	if channel.channel.ExternalRef == "" {
		return "the channel has no platform service, so a reading has nowhere to go"
	}
	if _, err := this.publisher.TimeShapeOf(channel.externalDeviceRef, channel.channel.ExternalRef); err != nil {
		return err.Error()
	}
	return ""
}

// runBackfill is the job. Channels are done one after another rather than in
// parallel: every publish is synchronous, so parallelism here would only move
// the queue from this service into the platform's ingestion.
func (this *Runtime) runBackfill(ctx context.Context, job *backfillJob, gen *generation, channels []backfillChannel, from time.Time, to time.Time) {
	defer this.backfillWorkers.Done()
	defer close(job.done)
	//a bug in the reconstruction of one environment must not take the service
	//down with it, and the caller polling the status is the one who needs to
	//hear about it. This is the only thing that puts a job into failed.
	defer func() {
		problem := recover()
		if problem == nil {
			return
		}
		util.Logger.Error("the backfill panicked", "environment", gen.def.Id, "panic", fmt.Sprint(problem))
		finished := time.Now()
		job.update(func(current *BackfillStatus) {
			current.State = BackfillFailed
			current.Error = fmt.Sprint(problem)
			current.FinishedAt = &finished
			current.CurrentChannel = ""
			current.Position = nil
		})
	}()

	started := time.Now()
	published := int64(0)
	for _, channel := range channels {
		if ctx.Err() != nil {
			break
		}
		points := gen.series[channel.channel.Id]
		status := BackfillChannelStatus{
			ChannelId: channel.channel.Id,
			AssetId:   channel.assetId,
			Name:      channel.channel.Name,
		}
		if reason := this.skipReason(channel, points); reason != "" {
			status.SkipReason = reason
			job.update(func(current *BackfillStatus) {
				current.Channels = append(current.Channels, status)
				current.ChannelsDone++
			})
			continue
		}
		status.Backfillable = true
		job.update(func(current *BackfillStatus) {
			current.CurrentChannel = channel.channel.Id
		})
		this.runBackfillChannel(ctx, job, gen, channel, points, from, to, &status)
		published += status.Published
		job.update(func(current *BackfillStatus) {
			current.Channels = append(current.Channels, status)
			current.ChannelsDone++
			current.CurrentChannel = ""
		})
		util.Logger.Info("backfill channel finished",
			"environment", gen.def.Id, "channel", channel.channel.Id,
			"published", status.Published, "silent", status.Silent, "failed", status.Failed)
	}

	finished := time.Now()
	state := BackfillDone
	if ctx.Err() != nil {
		state = BackfillCancelled
	}
	job.update(func(current *BackfillStatus) {
		current.State = state
		current.FinishedAt = &finished
		current.Published = published
		current.CurrentChannel = ""
		current.Position = nil
	})
	elapsed := finished.Sub(started)
	rate := float64(0)
	if elapsed > 0 {
		rate = float64(published) / elapsed.Seconds()
	}
	util.Logger.Info("backfill finished", "environment", gen.def.Id, "state", string(state),
		"published", published, "duration", elapsed, "events_per_second", rate)
}

// runBackfillChannel replays one channel over the window. Nothing here touches
// the environment - not its state, not its replay anchors, not its last values
// - which is the point of the local counter and anchor below: the live
// simulation keeps running while the job does, and moving the persisted loop
// anchor backwards would make the live channel jump to a different point in
// its data.
func (this *Runtime) runBackfillChannel(ctx context.Context, job *backfillJob, gen *generation, channel backfillChannel, points []dataset.Point, from time.Time, to time.Time, status *BackfillChannelStatus) {
	interval := channel.channel.IntervalSeconds
	//with a change trigger the value is computed on the evaluation grid and the
	//publish interval becomes the heartbeat, exactly as it does live
	cov, hasCov, _ := covOf(channel.channel)
	step := interval
	if hasCov {
		step = cov.evalSeconds
	}
	steps := backfillTicks(step, from, to)

	//a looping replay plays relative to an anchor: the live anchor is the moment
	//the simulation started, which lies after this window, so replayValue would
	//report every instant as "not yet started". The window's own start is used
	//instead, since it is both usable and reproducible - the same window
	//replays the same data.
	anchor := from.Unix()

	//a cumulative profile publishes a meter reading, not a rate. The live path
	//keeps that counter in the asset state; the job keeps its own, starting at
	//zero, so the backfilled series is a ramp of its own. See docs/backfill.md.
	counter := float64(0)
	cumulative := channel.channel.Source.Kind == domain.SourceProfile && channel.channel.Source.Profile.Cumulative

	//the change trigger is reproduced the same way as the value: local
	//bookkeeping, from nothing, using the same pure covSends the live gate uses,
	//so a reconstructed series and a live one agree.
	//
	//Two pieces, mirroring the live gate: the comparison base, set only by a
	//finite published value, and the moment of the last publish, reset by every
	//publish. lastPublishedAt starts at the window start rather than zero
	//because the live heartbeat timer starts with the channel, so a channel
	//whose first samples are not numbers waits one heartbeat here too, instead
	//of every instant counting as overdue.
	var lastPublished *float64
	lastPublishedAt := from.Unix()

	//the injected faults are reproduced the same way, and resolved locally rather
	//than read off the generation: the job walks the definition, and both
	//resolutions have to answer the same question about the same step. The offsets
	//of a meter exchange are the job's own, so a reconstructed window never touches
	//what the live simulation counted from.
	faults, _ := newChannelFaults(gen.def.Seed, channel.channel, step)
	faultMemory := &faultRun{}
	exchanges := map[string]float64{}

	for i := int64(0); i < steps; i++ {
		if ctx.Err() != nil {
			return
		}
		//the instant is built from the window start by whole intervals rather
		//than by repeated addition, and in the local zone: profileValue reads
		//the hour and the weekday off the instant, and the live path hands it a
		//local time.Now(), so a window given in UTC would otherwise shift every
		//day profile by this server's zone offset
		at := from.Add(time.Duration(i*step) * time.Second).In(time.Local)

		value, ok := this.backfillValue(gen, channel, points, anchor, at, &counter, cumulative, step)
		if !ok {
			status.Silent++
			continue
		}
		//the fault sits between the computed value and the send, exactly as it does
		//live: the counter above has already advanced on the undisturbed value, and
		//a frozen or spiked reading is what the comparison below sees.
		if len(faults.list) > 0 {
			var send bool
			value, send = faultedReading(faults, faultMemory, exchanges, value, at)
			if !send {
				//suppressed, and counted as silent for the same reason a value that
				//was never produced is
				status.Silent++
				//the heartbeat of the live runner fires and resets its timer whether
				//or not anything went out, and the history run advances
				//lastAttemptUnix the same way; without this mirror a suppressed
				//heartbeat would leave the gap standing here and the two paths would
				//diverge by up to one heartbeat at the trailing edge of the outage
				if hasCov && at.Unix()-lastPublishedAt >= interval {
					lastPublishedAt = at.Unix()
				}
				continue
			}
		}
		if hasCov && !covBackfillSends(cov, lastPublished, lastPublishedAt, value, at, interval) {
			//suppressed, and counted as silent for the same reason a value that
			//was never produced is: nothing was sent at this instant. Published
			//plus silent plus failed stays the number of steps.
			status.Silent++
			continue
		}
		if err := this.publisher.PublishEventAt(channel.externalDeviceRef, channel.channel.ExternalRef, value, at); err != nil {
			status.Failed++
			status.LastError = err.Error()
			//WARN, not ERROR: a backfill that loses readings is worth looking at
			//and is not a page. The count and the message are in the status.
			if status.Failed == 1 {
				util.Logger.Warn("unable to publish a backfilled reading", attributes.ErrorKey, err,
					"environment", gen.def.Id, "channel", channel.channel.Id, "at", at)
			}
			//the attempt restarts the heartbeat gap whether or not the reading
			//went out, mirroring the live runner, which resets the timer right
			//after covPublish unconditionally; keeping the old moment would make
			//every following instant overdue and shift the reconstructed grid
			//off the live cadence. The comparison base stays as it was, exactly
			//as covPublish leaves the stored entry alone on a failure.
			if hasCov {
				lastPublishedAt = at.Unix()
			}
			continue
		}
		if hasCov {
			//every publish restarts the heartbeat gap, mirroring the live
			//timer reset after covPublish
			lastPublishedAt = at.Unix()
			if finite(value) {
				sent := value
				lastPublished = &sent
			}
			//a value that is not finite went out but must not become the
			//comparison base, exactly as covPublish refuses to store one: every
			//later comparison against it would be false, so a NaN sample would
			//suppress movement here while the live channel kept publishing it -
			//breaking the parity the bookkeeping exists for.
		}
		status.Published++
		if status.Published%backfillLogEvery == 0 {
			util.Logger.Info("backfill progress", "environment", gen.def.Id,
				"channel", channel.channel.Id, "published", status.Published, "at", at)
		}
		position := at
		job.update(func(current *BackfillStatus) {
			current.Position = &position
			current.Published++
		})
	}
}

// covBackfillSends is the live gate's decision, replayed on the grid: whatever
// covSends says goes out - the first finite reading, and every one that moved
// far enough - and one that says no goes out anyway once the heartbeat gap has
// run.
//
// The gap is measured with >= against integer seconds, so a heartbeat lands on
// the grid instant exactly one interval after the last publish whenever the
// interval is a multiple of the evaluation step - and at most one step later
// when it is not.
func covBackfillSends(cov covSettings, lastPublished *float64, lastPublishedAt int64, value float64, at time.Time, heartbeatSeconds int64) bool {
	if covSends(cov, lastPublished, value) {
		return true
	}
	return at.Unix()-lastPublishedAt >= heartbeatSeconds
}

// backfillValue is the reading of one channel at one instant, from the same
// pure functions the live path uses.
//
// stepSeconds is the span one computation stands for - the publish interval, or
// the evaluation interval of a change trigger - and is what the live path passes
// as binding.stepSeconds for exactly the same three uses.
func (this *Runtime) backfillValue(gen *generation, channel backfillChannel, points []dataset.Point, anchor int64, at time.Time, counter *float64, cumulative bool, stepSeconds int64) (float64, bool) {
	source := channel.channel.Source
	switch source.Kind {
	case domain.SourceProfile:
		//the same lookup the live tick and the history run make, against the same
		//index: the step lands at the same instant in all three
		profile := gen.timeline.effectiveProfile(domain.TimelineChannel, channel.channel.Id, *source.Profile, at)
		value := profileValue(profile, gen.def.Seed, channel.channel.Id, stepSeconds, at)
		if cumulative {
			//the same share of an hourly rate the live tick adds
			*counter += value * float64(stepSeconds) / 3600
			value = *counter
		}
		return value, true
	case domain.SourceDataset:
		replay := gen.timeline.effectiveDataset(domain.TimelineChannel, channel.channel.Id, *source.Dataset, at)
		return replayValue(replay, points, anchor, at, stepSeconds)
	}
	return 0, false
}
