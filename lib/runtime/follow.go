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
	"math"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// followFetchTimeout bounds one follow refresh fetch, and with it how long a
// stuck wrapper can delay the next tick of the shared timer. It is far
// smaller than seriesLoadTimeout: a refresh asks for [last point, now], not a
// whole window.
const followFetchTimeout = 2 * time.Minute

// followStaleMultiplier is how many refresh cadences a following source may
// miss before it counts as stale: past that its newest value is no longer held
// (replay.go) and the loop says so once. Three is one missed refresh plus room
// for a late one.
const followStaleMultiplier = 3

// minFollowTick is the floor of the shared ticker. domain.MinFollowEvery is a
// minute and validation enforces it, so this only catches a document that
// bypassed the api - but a stored one-second follow_every must not turn into a
// fetch per second. A test lowers it to observe several ticks in one run.
var minFollowTick = time.Second

// followClock is where a follow loop reads the current instant and gets its
// shared ticker. A test replaces it to drive the cadence itself, since a loop
// running against the wall clock can only be asserted on with a tolerance.
type followClock struct {
	now  func() time.Time
	tick func(interval time.Duration) (<-chan time.Time, func())
}

// followClockOf is the wall clock unless a test injected one.
func (this *Runtime) followClockOf() followClock {
	if this.followClock != nil {
		return *this.followClock
	}
	return followClock{
		now: time.Now,
		tick: func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		},
	}
}

// followSource is one dataset source of an environment that keeps reading
// after the initial fetch, a channel or a context source. seriesId is the key
// its points are stored under in generation.series; label is what a warning
// names it by - the channel id, or the context key for a context source.
//
// A source that declared a fallback appears twice, once for each of its two
// series: they are fetched on the same cadence and the same window but from
// different rows, so each needs a refresh of its own.
type followSource struct {
	seriesId   string
	label      string
	isContext  bool
	isFallback bool
	owner      string
	dataset    *domain.DatasetSource
}

// field names this source the way the loader that first fetched it logs it:
// under "channel" for a channel and "key" for a context source (runtime.go
// loadZoneSeries, contextsource.go loadContextSeries).
func (this followSource) field() (string, string) {
	if this.isContext {
		return "key", this.label
	}
	return "channel", this.label
}

// attributes names this source in a log line: its loader's field, plus the mark
// of the substitute series, whose trouble is not the main series' trouble.
func (this followSource) attributes() []any {
	key, value := this.field()
	if this.isFallback {
		return []any{key, value, "series", "fallback"}
	}
	return []any{key, value}
}

// stored is this source's points in a generation, and store puts a refreshed
// series back. Both must be called with env.mux held, like every other touch of
// that map.
func (this followSource) stored(gen *generation) []dataset.Point {
	series := gen.series[this.seriesId]
	if this.isFallback {
		return series.fallback
	}
	return series.points
}

func (this followSource) store(gen *generation, points []dataset.Point) {
	series := gen.series[this.seriesId]
	if this.isFallback {
		series.fallback = points
	} else {
		series.points = points
	}
	gen.series[this.seriesId] = series
}

// warn reports one problem of this source, under the field its loader uses.
func (this followSource) warn(msg string, err error, envId string) {
	util.Logger.Warn(msg, append([]any{attributes.ErrorKey, err, "environment", envId}, this.attributes()...)...)
}

// resolvedFollower is a followSource with its two durations parsed, the
// instant its next refresh is due and what the loop knows about its upstream
// having stopped. Every field here is written and read by the follow loop
// goroutine alone and needs no lock.
type resolvedFollower struct {
	followSource
	every   time.Duration
	window  time.Duration
	nextDue time.Time

	// emptyRefreshes counts the refreshes in a row that appended nothing, and
	// staleWarned says the crossing of followStaleMultiplier has been reported,
	// so a source whose upstream is gone costs one line and not one per tick.
	emptyRefreshes int
	staleWarned    bool
	// probeWarned says a probe of a source with no stored points - its own
	// initial load, or its fallback's - has already reported a failure, so a
	// source that never fills does not log per tick.
	probeWarned bool
}

// missed counts one refresh that brought nothing and reports the source as
// stale at the cadence where the replay stops holding its newest value: from
// there on the channel is silent, and one line says why.
func (this *resolvedFollower) missed(envId string, newest time.Time) {
	this.emptyRefreshes++
	if this.staleWarned || this.emptyRefreshes < followStaleMultiplier {
		return
	}
	this.staleWarned = true
	util.Logger.Warn("a following dataset source has gone stale",
		append([]any{"environment", envId,
			"newest", newest.UTC().Format(time.RFC3339), "empty_refreshes", this.emptyRefreshes},
			this.attributes()...)...)
}

// delivered clears that bookkeeping, and says the source plays again if it had
// been reported stale.
func (this *resolvedFollower) delivered(envId string) {
	this.emptyRefreshes = 0
	if !this.staleWarned {
		return
	}
	this.staleWarned = false
	util.Logger.Info("a following dataset source is delivering again",
		append([]any{"environment", envId}, this.attributes()...)...)
}

// probeFailed reports one failed probe of a source with no stored points,
// throttled to one line until the probe succeeds - shared by a source's own
// initial load and its fallback's, so neither logs per tick while its upstream
// stays gone.
func (this *resolvedFollower) probeFailed(msg string, err error, envId string) {
	if this.probeWarned {
		return
	}
	this.probeWarned = true
	this.warn(msg, err, envId)
}

// probeRecovered clears that bookkeeping, and says the probe succeeds again if
// it had been reported failing.
func (this *resolvedFollower) probeRecovered(envId string) {
	if !this.probeWarned {
		return
	}
	this.probeWarned = false
	util.Logger.Info("a following dataset source is answering again",
		append([]any{"environment", envId}, this.attributes()...)...)
}

// followersOf collects every dataset source of a definition that declared
// follow - channels in document order, then context sources - each with its
// refresh interval and its window resolved. A source whose follow_every or
// window does not parse is dropped and logged: validation refuses both on the
// way in, so it bypassed the api.
//
// series is what the load this generation was built from produced. A substitute
// series follows only where the source's own series is in it: without one there
// is nothing for a substitute to stand in for, and following it anyway would
// have it recover a source that keeps failing to load, once per cadence and for
// as long as the environment runs.
//
// The map is read here rather than under env.mux because the caller runs it
// before the follow loop starts and holds the runtime's lifecycle lock, so
// nothing writes it yet.
func followersOf(def domain.Environment, series map[string]replaySeries) []resolvedFollower {
	var raw []followSource
	var walkZones func(zones []domain.Zone)
	walkZones = func(zones []domain.Zone) {
		for _, zone := range zones {
			walkZones(zone.Zones)
			for _, asset := range zone.Assets {
				for _, channel := range asset.Channels {
					source := channel.Source
					if source.Kind != domain.SourceDataset || source.Dataset == nil || !source.Dataset.Follow {
						continue
					}
					raw = append(raw, followSource{
						seriesId: channel.Id, label: channel.Id, owner: def.Owner, dataset: source.Dataset,
					})
					if _, loaded := series[channel.Id]; !loaded {
						continue
					}
					if substitute := source.Dataset.FallbackSource(); substitute != nil {
						raw = append(raw, followSource{
							seriesId: channel.Id, label: channel.Id, isFallback: true, owner: def.Owner, dataset: substitute,
						})
					}
				}
			}
		}
	}
	walkZones(def.Zones)
	for key, source := range def.ContextSources {
		if source.Kind != domain.SourceDataset || source.Dataset == nil || !source.Dataset.Follow {
			continue
		}
		raw = append(raw, followSource{
			seriesId: contextSeriesId(key), label: key, isContext: true, owner: def.Owner, dataset: source.Dataset,
		})
		if _, loaded := series[contextSeriesId(key)]; !loaded {
			continue
		}
		if substitute := source.Dataset.FallbackSource(); substitute != nil {
			raw = append(raw, followSource{
				seriesId: contextSeriesId(key), label: key, isContext: true, isFallback: true, owner: def.Owner, dataset: substitute,
			})
		}
	}

	result := make([]resolvedFollower, 0, len(raw))
	for _, f := range raw {
		if f.dataset.Origin != domain.OriginPlatform && f.dataset.Origin != domain.OriginExport {
			//only these two have something to poll again, and both fetch paths
			//below address the wrapper: an uploaded dataset would be sent as a
			//device id. Validation refuses it, so this bypassed the api.
			f.warn("only a platform or export dataset can follow, this one is never refreshed",
				fmt.Errorf("origin %q", f.dataset.Origin), def.Id)
			continue
		}
		every, err := domain.ParseFollowEvery(f.dataset.FollowEvery)
		if err != nil {
			f.warn("a following dataset source has an unusable follow_every, it is never refreshed", err, def.Id)
			continue
		}
		window, err := domain.ParseWindow(f.dataset.Window)
		if err != nil {
			f.warn("a following dataset source has an unusable window, it is never refreshed", err, def.Id)
			continue
		}
		result = append(result, resolvedFollower{followSource: f, every: every, window: window})
	}
	return result
}

// runFollowLoop refreshes the following dataset sources of one environment on
// a single timer. The timer ticks at the shortest follow_every among them and
// every source is refreshed only when its own cadence is due, so an hourly
// source is not dragged along by a minutely one. A cadence that is not a
// multiple of the shared tick lands on whichever tick is nearest its due
// instant, so it alternates between the tick before and the one after and
// averages out at what it declares.
//
// It ends with the environment: ctx is envCtx, the same context runChannel and
// runContextSource run on, cancelled by Stop and by Reload's stopRunners.
func (this *Runtime) runFollowLoop(ctx context.Context, env *environment, gen *generation, followers []resolvedFollower) {
	defer env.runners.Done()
	interval := followers[0].every
	for _, f := range followers[1:] {
		if f.every < interval {
			interval = f.every
		}
	}
	if interval < minFollowTick {
		interval = minFollowTick
	}
	clock := this.followClockOf()
	//the initial fetch of every source just happened, in the load this
	//generation was built from, so the first refresh of each is one cadence away
	started := clock.now()
	for i := range followers {
		followers[i].nextDue = started.Add(followers[i].every)
	}
	//a series that is already past its hold bound when the loop starts is
	//silent from the first tick, so the warning belongs here and not three
	//empty refreshes later
	env.mux.Lock()
	for i := range followers {
		points := followers[i].stored(gen)
		if len(points) == 0 {
			continue
		}
		newest := time.Unix(points[len(points)-1].Unix, 0)
		if started.Sub(newest) > followers[i].every*followStaleMultiplier {
			followers[i].emptyRefreshes = followStaleMultiplier - 1
			followers[i].missed(gen.def.Id, newest)
		}
	}
	env.mux.Unlock()
	ticks, stopTicking := clock.tick(interval)
	defer stopTicking()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			this.refreshFollowers(ctx, env, gen, followers, clock.now(), interval)
		}
	}
}

// followDue decides whether a follower is due at now and where its next due
// instant goes. The due times stay on the follower's own grid, with half a tick
// of tolerance: advanced from the receipt instant instead, they land a hair
// after the next tick whenever a tick was late, which skips about every second
// refresh of a follower whose cadence is the tick interval. A loop that fell
// more than one cadence behind starts over from now rather than working off a
// queue of catch-up refreshes.
func followDue(nextDue time.Time, now time.Time, every time.Duration, interval time.Duration) (bool, time.Time) {
	if now.Before(nextDue.Add(-interval / 2)) {
		return false, nextDue
	}
	if now.Sub(nextDue) > every {
		return true, now.Add(every)
	}
	return true, nextDue.Add(every)
}

// refreshFollowers refreshes the sources that are due at now, skipped whole
// while a history run owns the environment: a run replays its own frozen
// series on a virtual clock, and a live refresh must not reach into it.
//
// now is the instant of this tick, taken once by the caller, so the cadences
// of several sources are decided against one moment.
func (this *Runtime) refreshFollowers(ctx context.Context, env *environment, gen *generation, followers []resolvedFollower, now time.Time, interval time.Duration) {
	env.mux.Lock()
	closed := env.underHistory || env.removed
	env.mux.Unlock()
	if closed {
		return
	}
	for i := range followers {
		due, next := followDue(followers[i].nextDue, now, followers[i].every, interval)
		if !due {
			continue
		}
		followers[i].nextDue = next
		if this.refreshFollower(ctx, env, gen, &followers[i]) {
			//a source recovered from a failed initial load, so this generation
			//is about to be replaced by one that binds its channel: refreshing
			//the remaining sources into it would only spend fetches
			this.triggerFollowReload(env, gen.def.Id)
			return
		}
	}
}

// refreshFollower brings one source up to date and reports whether the
// environment has to be reloaded for it, which only a source that recovered
// from a failed initial load needs.
//
// On success the grown series is swapped into gen.series under env.mux - the
// same lock executeDataset and tickContextSource already hold while reading
// it, so a running channel picks the grown series up on its next tick without
// a cache of its own to invalidate. A fetch error leaves the old series
// exactly as it was, to be retried at the next tick.
func (this *Runtime) refreshFollower(ctx context.Context, env *environment, gen *generation, f *resolvedFollower) bool {
	env.mux.Lock()
	stored := f.stored(gen)
	pending := env.reloadPending
	env.mux.Unlock()
	if len(stored) == 0 {
		//a reload an earlier probe triggered is still running and loads this
		//source itself; probing again would only spend a fetch
		if pending {
			return false
		}
		if f.isFallback {
			this.loadMissingFallback(ctx, env, gen, f)
			return false
		}
		return this.loadMissingFollower(ctx, env, gen, f)
	}

	now := time.Now()
	newest := time.Unix(stored[len(stored)-1].Unix, 0)
	start := newest
	if start.After(now) {
		//a newest point after now - a wrongly stamped import, or this clock
		//behind the platform's - would make [last point, now] an inverted
		//window the wrapper cannot answer. One cadence back is a window it
		//can, and appendNewer drops everything that is not newer anyway.
		start = now.Add(-f.every)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, followFetchTimeout)
	fresh, err := this.fetchRemoteSince(fetchCtx, f.owner, f.dataset, start, now)
	cancel()
	if err != nil {
		if followAbandoned(ctx, env, err) {
			return false
		}
		f.warn("unable to refresh a following dataset source, it keeps its last fetched series", err, gen.def.Id)
		f.missed(gen.def.Id, newest)
		return false
	}
	//no new measurement since the last refresh is the ordinary answer of a
	//source that publishes less often than it is followed, not a problem worth
	//a line per tick - but enough of them in a row is one, see missed
	if len(fresh) == 0 {
		f.missed(gen.def.Id, newest)
		return false
	}

	env.mux.Lock()
	if env.underHistory || env.removed {
		//a history run took the environment over while this fetch was in
		//flight; its own frozen series is what the run reads, so the fetch is
		//simply dropped rather than raced into gen.series
		env.mux.Unlock()
		return false
	}
	current := f.stored(gen)
	grown := appendNewer(current, fresh)
	appended := len(grown) != len(current)
	if appended {
		f.store(gen, trimToWindow(grown, now.Add(-f.window)))
	}
	env.mux.Unlock()
	if !appended {
		f.missed(gen.def.Id, newest) //the source answered its known tail again and nothing else
		return false
	}
	f.delivered(gen.def.Id)
	return false
}

// triggerFollowReload rebuilds the environment for a source that recovered
// from a failed initial load. It runs on a goroutine of its own because Reload
// waits for env.runners, which counts the follow loop calling this.
//
// reloadPending keeps the probe of a later tick from spawning a second reload
// while this one runs, and followReloads is what Stop waits on, so no reload
// outlives it - env.runners cannot serve for that, Reload waits on it itself.
func (this *Runtime) triggerFollowReload(env *environment, envId string) {
	env.mux.Lock()
	if env.reloadPending || env.removed {
		env.mux.Unlock()
		return
	}
	env.reloadPending = true
	env.mux.Unlock()
	this.followReloads.Add(1)
	go func() {
		defer this.followReloads.Done()
		defer func() {
			env.mux.Lock()
			env.reloadPending = false
			env.mux.Unlock()
		}()
		this.Reload(envId)
	}()
}

// loadMissingFollower probes whether a source with no stored points can be
// loaded now: its initial load failed, so there is no tail to ask for, and for
// a channel there is no binding either, since a generation only binds a dataset
// channel whose series is loaded. It reports whether a reload is needed.
//
// The fetched points are deliberately dropped rather than stored into this
// generation. Storing them would leave a source whose reload then fails - a
// database blip in Reload - with points but no binding forever, because every
// later tick would take the tail path and never probe again. The reload loads
// the series itself, and until it succeeds the empty series keeps the probe
// coming back.
func (this *Runtime) loadMissingFollower(ctx context.Context, env *environment, gen *generation, f *resolvedFollower) bool {
	fetchCtx, cancel := context.WithTimeout(ctx, followFetchTimeout)
	fresh, err := this.fetchRemoteSeries(fetchCtx, f.owner, f.dataset)
	cancel()
	if err != nil {
		if followAbandoned(ctx, env, err) {
			return false
		}
		f.probeFailed("unable to load a following dataset source whose initial load failed, it is retried at the next tick", err, gen.def.Id)
		return false
	}
	//fetchRemoteSeries refuses fewer than two points, so a probe that answers
	//them means the reload finds a series that is playable and, for a channel,
	//bindable. A context source needs no binding but the same reload: nothing
	//else puts the points into a generation any more.
	if len(fresh) < 2 {
		f.probeFailed("a following dataset source whose initial load failed answers fewer than two points, it is probed again at each tick",
			fmt.Errorf("%d points in the replay window", len(fresh)), gen.def.Id)
		return false
	}
	f.probeRecovered(gen.def.Id)
	env.mux.Lock()
	defer env.mux.Unlock()
	return !env.underHistory && !env.removed && len(f.stored(gen)) == 0
}

// loadMissingFallback fetches the substitute series of a source whose own series
// is loaded but whose fallback is not, because its initial load failed. Unlike a
// missing main series this needs no reload: nothing binds a substitute, the
// channel is already running, and the only thing missing is the coverage of its
// gaps - so the points go straight into the entry the main series holds.
//
// The failure is reported once rather than per cadence. A neighbouring export
// that stays unreadable costs the coverage and nothing else, and a line per
// cadence for a channel that plays is noise that buries the lines that matter.
func (this *Runtime) loadMissingFallback(ctx context.Context, env *environment, gen *generation, f *resolvedFollower) {
	fetchCtx, cancel := context.WithTimeout(ctx, followFetchTimeout)
	fresh, err := this.fetchRemoteSeries(fetchCtx, f.owner, f.dataset)
	cancel()
	if err != nil {
		if followAbandoned(ctx, env, err) {
			return
		}
		//fetchRemoteSeries refuses fewer than two points, so a substitute
		//whose rows are simply not there arrives here as an error too
		f.probeFailed("unable to load the fallback series of a following dataset source, the gaps of that source stay open until it answers again", err, gen.def.Id)
		return
	}
	f.probeRecovered(gen.def.Id)
	env.mux.Lock()
	defer env.mux.Unlock()
	if env.underHistory || env.removed {
		//a history run took the environment over while this fetch was in
		//flight; its own frozen series is what the run reads
		return
	}
	if _, loaded := gen.series[f.seriesId]; !loaded {
		//the source's own series left this generation, so there is nothing for
		//a substitute to stand in for
		return
	}
	f.store(gen, fresh)
}

// followAbandoned says a failed fetch is nobody's problem any more, so it is
// not worth a warning: the environment context was cancelled - a stop, a
// reload, a history run taking over - or the environment is gone. Our own
// followFetchTimeout is deliberately not covered: a wrapper that does not
// answer within it is worth hearing about.
func followAbandoned(ctx context.Context, env *environment, err error) bool {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return true
	}
	env.mux.Lock()
	defer env.mux.Unlock()
	return env.removed
}

// appendNewer merges a refreshed fetch into a stored series: only points
// strictly after the last stored instant are appended, deduplicated on the
// instant, and an empty stored series takes every point of the fetch.
//
// The result is always a freshly allocated slice, so a reader holding the old
// header never sees a partial append. Nothing may ever append into a stored
// slice instead: the trim below hands out sub-slices whose spare capacity
// belongs to the array such a reader still holds.
func appendNewer(stored []dataset.Point, fresh []dataset.Point) []dataset.Point {
	lastUnix := int64(math.MinInt64)
	if len(stored) > 0 {
		lastUnix = stored[len(stored)-1].Unix
	}
	added := make([]dataset.Point, 0, len(fresh))
	for _, point := range fresh {
		if point.Unix > lastUnix {
			added = append(added, point)
			lastUnix = point.Unix
		}
	}
	if len(added) == 0 {
		return stored
	}
	merged := make([]dataset.Point, len(stored), len(stored)+len(added))
	copy(merged, stored)
	return append(merged, added...)
}

// trimToWindow drops the points before cutoff, which is now minus the source's
// window, so that a following series stays bounded by what its window declares
// rather than growing for as long as the environment runs. The last two points
// are always kept: the replay divides by the span of the series and a
// generation binds nothing shorter than two points.
//
// The result shares the array of points and is never written into, see
// appendNewer.
func trimToWindow(points []dataset.Point, cutoff time.Time) []dataset.Point {
	if len(points) <= 2 {
		return points
	}
	limit := cutoff.Unix()
	cut := 0
	for cut < len(points) && points[cut].Unix < limit {
		cut++
	}
	if keep := len(points) - 2; cut > keep {
		cut = keep
	}
	if cut == 0 {
		return points
	}
	return points[cut:]
}
