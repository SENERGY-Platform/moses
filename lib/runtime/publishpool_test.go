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
	"math/rand"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
)

// poolJob is a reading for the pool's own tests: the service ref decides the
// warm up and carries the test's name, because the warm topics are process wide;
// the channel id decides the shard.
func poolJob(test string, channel string, step int, done func(sent bool, err error)) publishJob {
	return publishJob{
		channelId: channel,
		binding: channelBinding{
			asset:   assetRef{id: "asset-1", externalRef: "device-1"},
			channel: domain.Channel{Id: channel, ExternalRef: "urn:infai:ses:service:" + test + ":" + channel},
		},
		value: step,
		at:    historyFrom.Add(time.Duration(step) * time.Second),
		done:  done,
	}
}

// forgetWarmTopics puts the process wide set of produced-to topics back, for a
// test that asserts on what a first reading of a topic does. Runs of the same
// test would otherwise find its topics warm from the run before.
func forgetWarmTopics() {
	publishWarmMux.Lock()
	defer publishWarmMux.Unlock()
	publishWarm = map[string]bool{}
}

// TestThePublishPoolKeepsOneChannelInOrder is the property the sharding
// exists for: the comparison base of a change trigger and every series read out
// of a reconstruction assume that reading n of a channel went out before n+1.
// The latency varies per reading, so a channel spread over the workers would
// overtake itself here rather than only under load.
func TestThePublishPoolKeepsOneChannelInOrder(t *testing.T) {
	const channels = 40
	const perChannel = 25

	mux := sync.Mutex{}
	seen := map[string][]int{}
	pool := newPublishPool(context.Background(), 8, func(job publishJob) (bool, error) {
		time.Sleep(time.Duration(rand.Int63n(200)) * time.Microsecond)
		mux.Lock()
		defer mux.Unlock()
		seen[job.channelId] = append(seen[job.channelId], job.value.(int))
		return true, nil
	})
	defer pool.Close()

	acked := sync.WaitGroup{}
	//by step and then by channel, the way the run submits
	for step := 0; step < perChannel; step++ {
		for channel := 0; channel < channels; channel++ {
			acked.Add(1)
			pool.Submit(poolJob(t.Name(), fmt.Sprintf("ch-%d", channel), step, func(sent bool, err error) { acked.Done() }))
		}
	}
	pool.Drain()
	acked.Wait()

	mux.Lock()
	defer mux.Unlock()
	if len(seen) != channels {
		t.Fatalf("expected readings of %d channels, got %d", channels, len(seen))
	}
	for id, steps := range seen {
		if len(steps) != perChannel {
			t.Fatalf("%v: expected %d readings, got %d", id, perChannel, len(steps))
		}
		for i, step := range steps {
			if step != i {
				t.Fatalf("%v: reading %d of the channel is step %d, so the pool published it out of order: %v", id, i, step, steps)
			}
		}
	}
}

// TestAChannelIsPinnedToOneWorkerOfThePublishPool is the other half of the
// order: the assignment follows from the channel alone, so it cannot change
// mid-run and is the same on the next run of the same environment.
func TestAChannelIsPinnedToOneWorkerOfThePublishPool(t *testing.T) {
	pool := newPublishPool(context.Background(), 16, func(publishJob) (bool, error) { return true, nil })
	defer pool.Close()

	first := pool.shardOf("ch-7")
	for i := 0; i < 100; i++ {
		if got := pool.shardOf("ch-7"); got != first {
			t.Fatalf("the same channel went to worker %d and then to %d", first, got)
		}
	}
	if first < 0 || first >= 16 {
		t.Fatalf("worker %d is not one of the 16", first)
	}

	//and the hash really spreads: a pool whose workers are mostly idle would be
	//configured but not used
	used := map[int]bool{}
	for i := 0; i < 500; i++ {
		used[pool.shardOf(fmt.Sprintf("urn:infai:ses:channel:%d", i))] = true
	}
	if len(used) != 16 {
		t.Errorf("only %d of the 16 workers ever got a channel", len(used))
	}
}

// The worker count is clamped on both sides: no worker would accept readings
// nobody sends, and a count far above the channels of an environment only holds
// queues.
func TestTheWorkerCountIsClamped(t *testing.T) {
	for _, workers := range []int{-5, 0, 1} {
		pool := newPublishPool(context.Background(), workers, func(publishJob) (bool, error) { return true, nil })
		if len(pool.shards) != 1 {
			t.Errorf("a pool asked for %d workers has %d queues, expected 1", workers, len(pool.shards))
		}
		pool.Close()
	}

	for configured, want := range map[int]int{-1: defaultPublishWorkers, 0: defaultPublishWorkers, 1: 1,
		32: 32, maxPublishWorkers: maxPublishWorkers, maxPublishWorkers + 1: maxPublishWorkers, 100000: maxPublishWorkers} {
		cfg := config.Config{JsTimeout: time.Second, StateFlushInterval: time.Hour, PublishWorkers: configured}
		if got := newRuntime(cfg, newFakeEnvironments(), newFakeStates(), nil, &fakePublisher{}).publishWorkers; got != want {
			t.Errorf("a configured worker count of %d became %d, expected %d", configured, got, want)
		}
	}
}

// TestTheFirstReadingOfAServiceTopicIsProducedAlone is the warm up. The
// connector library keeps the topics it created in a map it neither locks for
// the write nor for the read every later publish makes, so a worker holding a
// first reading of a topic sends it under a lock that excludes every other
// publish, and everything else stays parallel.
func TestTheFirstReadingOfAServiceTopicIsProducedAlone(t *testing.T) {
	const channels = 6
	const perChannel = 8
	forgetWarmTopics()

	mux := sync.Mutex{}
	inFlight, peakFirst, peakLater := 0, 0, 0
	first := map[string]bool{}
	pool := newPublishPool(context.Background(), 4, func(job publishJob) (bool, error) {
		mux.Lock()
		inFlight++
		isFirst := !first[job.binding.channel.ExternalRef]
		first[job.binding.channel.ExternalRef] = true
		if isFirst && inFlight > peakFirst {
			peakFirst = inFlight
		}
		if !isFirst && inFlight > peakLater {
			peakLater = inFlight
		}
		mux.Unlock()

		time.Sleep(time.Duration(rand.Int63n(400)) * time.Microsecond)

		mux.Lock()
		inFlight--
		mux.Unlock()
		return true, nil
	})
	defer pool.Close()

	for step := 0; step < perChannel; step++ {
		for channel := 0; channel < channels; channel++ {
			pool.Submit(poolJob(t.Name(), fmt.Sprintf("ch-%d", channel), step, func(sent bool, err error) {}))
		}
	}
	pool.Drain()

	mux.Lock()
	defer mux.Unlock()
	if peakFirst != 1 {
		t.Errorf("the first reading of a topic ran next to %d others, expected it to be alone", peakFirst-1)
	}
	//and the warm up really is only the first one, or the pool would be a
	//sequential publisher with extra steps
	if peakLater < 2 {
		t.Errorf("no two readings ever ran at once, so the pool published sequentially")
	}
	if len(first) != channels {
		t.Errorf("expected %d topics, got %d", channels, len(first))
	}
}

// TestAnAbortedPublishPoolBooksTheRestAsAborted: an abort stops the sending, not
// the accounting. Every reading the pool took has to be answered, or the
// counters of a run would not add up to the steps it took.
func TestAnAbortedPublishPoolBooksTheRestAsAborted(t *testing.T) {
	const queued = 20

	before := goruntime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sending := make(chan struct{})
	release := make(chan struct{})
	pool := newPublishPool(ctx, 1, func(job publishJob) (bool, error) {
		if job.value.(int) == 0 {
			//the first reading of the topic, which is produced under the warm up
			//lock; it must not block, or every later one would wait for it
			return true, nil
		}
		sending <- struct{}{}
		<-release
		return true, nil
	})

	answers := make([]error, queued+2)
	answered := make([]int, queued+2)
	sent := make([]bool, queued+2)
	mux := sync.Mutex{}
	answer := func(index int) func(bool, error) {
		return func(ok bool, err error) {
			mux.Lock()
			defer mux.Unlock()
			answered[index]++
			answers[index] = err
			sent[index] = ok
		}
	}

	//the topic is warmed first, so the readings below are staged rather than
	//held up by the warm up lock
	pool.Submit(poolJob(t.Name(), "ch-1", 0, answer(0)))
	pool.Drain()

	for i := 1; i <= queued+1; i++ {
		pool.Submit(poolJob(t.Name(), "ch-1", i, answer(i)))
	}

	//the first of them is in the send, the twenty behind it are staged
	<-sending
	cancel()
	close(release)
	pool.Drain()
	pool.Close()

	mux.Lock()
	defer mux.Unlock()
	for i, count := range answered {
		if count != 1 {
			t.Fatalf("reading %d was answered %d times, expected exactly once", i, count)
		}
	}
	if !sent[1] || answers[1] != nil {
		t.Errorf("the reading that was already in the send did not finish: sent %v, error %v", sent[1], answers[1])
	}
	for i := 2; i <= queued+1; i++ {
		if sent[i] {
			t.Errorf("reading %d was reported as sent although the pool was aborted", i)
		}
		if !errors.Is(answers[i], ErrPublishAborted) {
			t.Errorf("reading %d was failed with %v, expected the abort to be named", i, answers[i])
		}
	}

	//Close waits for the workers, so this needs no settling
	if after := goruntime.NumGoroutine(); after > before {
		t.Errorf("the closed pool left %d goroutines behind", after-before)
	}
}

// A bug in the send path must not take the service down from a worker goroutine,
// where no recover of the run can reach it - and must not be swallowed either,
// or the job would report itself done while its readings were lost.
func TestAPublishThatPanicsFailsTheReadingAndReachesTheRun(t *testing.T) {
	pool := newPublishPool(context.Background(), 1, func(job publishJob) (bool, error) {
		panic("the connector fell over")
	})
	defer pool.Close()

	sent, err := true, error(nil)
	pool.Submit(poolJob(t.Name(), "ch-1", 0, func(ok bool, problem error) { sent, err = ok, problem }))

	//Drain waits for the ack before it raises, so reading the two below is
	//ordered behind whoever wrote them
	raised := func() (problem any) {
		defer func() { problem = recover() }()
		pool.Drain()
		return nil
	}()
	if sent {
		t.Error("a reading whose send panicked was reported as sent")
	}
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("expected the panic to be named as the failure of the reading, got %v", err)
	}
	if raised == nil {
		t.Fatal("the panic stayed on the worker, where no recover of the run can reach it")
	}
	if !strings.Contains(fmt.Sprint(raised), "the connector fell over") {
		t.Errorf("expected the original panic to be carried over, got %v", raised)
	}
}

// TestAPublishedReadingIsCopiedAwayFromTheState is the copy at the submit. A
// value a script read out of the state is the very Go map that sits in the
// state, and a worker marshals it outside the environment mutex.
func TestAPublishedReadingIsCopiedAwayFromTheState(t *testing.T) {
	state := map[string]interface{}{
		"n":      1.0,
		"nested": map[string]interface{}{"deep": 1.0},
		"list":   []interface{}{1.0, map[string]interface{}{"deep": 1.0}},
	}
	copied, ok := copyPublished(state).(map[string]interface{})
	if !ok {
		t.Fatalf("the copy is a %T", copyPublished(state))
	}
	if !reflect.DeepEqual(copied, state) {
		t.Fatalf("the copy differs from the value: %#v against %#v", copied, state)
	}

	state["n"] = 2.0
	state["nested"].(map[string]interface{})["deep"] = 2.0
	state["list"].([]interface{})[0] = 2.0
	state["list"].([]interface{})[1].(map[string]interface{})["deep"] = 2.0

	if copied["n"] != 1.0 {
		t.Errorf("the copy followed the state to %v", copied["n"])
	}
	if got := copied["nested"].(map[string]interface{})["deep"]; got != 1.0 {
		t.Errorf("a nested map is shared with the state: %v", got)
	}
	if got := copied["list"].([]interface{})[0]; got != 1.0 {
		t.Errorf("a slice is shared with the state: %v", got)
	}
	if got := copied["list"].([]interface{})[1].(map[string]interface{})["deep"]; got != 1.0 {
		t.Errorf("a map inside a slice is shared with the state: %v", got)
	}

	//a scalar is handed on as it is, and nil stays nil
	if copyPublished(42.5) != 42.5 || copyPublished(nil) != nil || copyPublished("text") != "text" {
		t.Error("a scalar reading was not handed on unchanged")
	}
	//a value nested past the bound is handed on rather than taking the stack down
	deep := map[string]interface{}{}
	deep["self"] = deep
	copyPublished(deep)
}

// TestAScriptThatSendsAMapFromTheStateDoesNotRaceTheNextStep is the same thing
// through the run: without the copy the worker marshals the map while the next
// step writes into it, which is a concurrent map access - a fatal error, not a
// failed test - and the race detector reports it here.
func TestAScriptThatSendsAMapFromTheStateDoesNotRaceTheNextStep(t *testing.T) {
	const id = "env-hist-state-map"
	code := "var box = moses.asset.state.get('box');" +
		"if (typeof box === 'object' && box !== null) { box.n = box.n + 1; } else { box = {n: 1}; }" +
		"moses.asset.state.set('box', box);" +
		"moses.channel.send(box);"
	def := testEnvironment(id, scriptChannel("ch-1", domain.Sensor, 1, serviceRefOf(id), code))
	publisher := &fakePublisher{latency: func() time.Duration { return time.Millisecond }}
	rt, env, gen := historyFixture(t, def, nil, publisher)

	runEngine(t, rt, env, gen, historyFrom, historyFrom.Add(30*time.Second))

	events := publisher.backfilled(serviceRefOf(id))
	if len(events) != 31 {
		t.Fatalf("expected 31 readings, got %d", len(events))
	}
	//every reading is the counter of its own instant: a shared map would make
	//them all read the last one
	for i, event := range events {
		box, ok := event.value.(map[string]interface{})
		if !ok {
			t.Fatalf("reading %d is a %T, expected the map the script sent", i, event.value)
		}
		number, numeric := asFloat(box["n"])
		if !numeric {
			t.Fatalf("reading %d carries n=%#v", i, box["n"])
		}
		if int(number) != i+1 {
			t.Fatalf("reading %d carries n=%v, expected %d - the readings share one map", i, number, i+1)
		}
	}
}

// historyPoolDocument is a site with enough channels to fill the pool, in the
// three shapes the run drives: a plain one, a split one whose source runs more
// often than it publishes, and one publishing on change.
func historyPoolDocument(id string, plain int) domain.Environment {
	channels := []domain.Channel{}
	for i := 0; i < plain; i++ {
		channels = append(channels,
			profileChannel(fmt.Sprintf("ch-plain-%d", i), fmt.Sprintf("%s-plain-%d", serviceRefOf(id), i),
				60, flatProfile(float64(100+i), 0)))
	}

	split := scriptChannel("ch-split", domain.Sensor, 60, serviceRefOf(id)+"-split",
		"var n = moses.asset.state.get('n') + 1; moses.asset.state.set('n', n); moses.channel.send(n);")
	split.Source.IntervalSeconds = 20

	cov := profileChannel("ch-cov", serviceRefOf(id)+"-cov", 600, flatProfile(230, 0))
	cov.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 60}

	return testEnvironment(id, append(channels, split, cov)...)
}

// historyBookedSteps is how many steps of one channel the run books, which is
// the grid it publishes on: the evaluation grid with a change trigger, the
// publish interval without one, and nothing for a channel with no publish grid.
// Deliberately not historyTicksOf, which counts the due events of the volume
// check, where a split channel is counted on its source grid as well.
func historyBookedSteps(binding channelBinding, from time.Time, to time.Time) int64 {
	switch {
	case binding.cov != nil:
		return backfillTicks(binding.cov.evalSeconds, from, to)
	case binding.sourceInterval > 0:
		if !channelPublishes(binding.channel) {
			return 0
		}
		return backfillTicks(binding.channel.IntervalSeconds, from, to)
	default:
		return backfillTicks(binding.channel.IntervalSeconds, from, to)
	}
}

// assertHistoryStepsAddUp is the invariant of the bookkeeping: every channel
// books exactly one of published, silent and failed per step of the grid it
// publishes on, and the totals of the run are the sums of those.
func assertHistoryStepsAddUp(t *testing.T, gen *generation, result HistoryResult, from time.Time, to time.Time) {
	t.Helper()
	published, failed := int64(0), int64(0)
	for _, binding := range gen.sensors {
		status := resultFor(t, result, binding.channel.Id)
		steps := historyBookedSteps(binding, from, to)
		if status.Published+status.Silent+status.Failed != steps {
			t.Errorf("%v: %d published + %d silent + %d failed is not the %d steps of its publish grid",
				binding.channel.Id, status.Published, status.Silent, status.Failed, steps)
		}
		published += status.Published
		failed += status.Failed
	}
	if result.Published != published || result.Failed != failed {
		t.Errorf("the run reports %d published and %d failed, its channels %d and %d",
			result.Published, result.Failed, published, failed)
	}
}

// TestAHistoryRunPublishesEveryChannelInOrder is the pool in the run: more
// channels than workers, so channels share one, and a latency that varies per
// reading. Every channel still arrives in the order of the virtual clock.
func TestAHistoryRunPublishesEveryChannelInOrder(t *testing.T) {
	const id = "env-hist-pool-order"
	const plain = 24
	def := historyPoolDocument(id, plain)
	publisher := &fakePublisher{latency: func() time.Duration {
		return time.Duration(rand.Int63n(300)) * time.Microsecond
	}}
	rt, env, gen := historyFixture(t, def, nil, publisher)

	from, to := historyFrom, historyFrom.Add(30*time.Minute)
	result := runEngine(t, rt, env, gen, from, to)

	for i := 0; i < plain; i++ {
		ref := fmt.Sprintf("%s-plain-%d", serviceRefOf(id), i)
		events := publisher.backfilled(ref)
		//a minute grid over half an hour, both ends included
		if len(events) != 31 {
			t.Fatalf("%v: expected 31 readings, got %d", ref, len(events))
		}
		for j, event := range events {
			want := from.Add(time.Duration(j) * time.Minute)
			if !event.at.Equal(want) {
				t.Fatalf("%v: reading %d is stamped %v, expected %v - the channel arrived out of order", ref, j, event.at, want)
			}
			//and the readings of one channel are that channel's, so no job went
			//to the wrong service
			if event.value != float64(100+i) {
				t.Fatalf("%v: reading %d is %v, expected the channel's own %v", ref, j, event.value, float64(100+i))
			}
		}
	}

	//the split channel publishes what its source last computed, and its counter
	//only rises: an overtaking reading would show up as a value out of sequence
	last := float64(0)
	for i, event := range publisher.backfilled(serviceRefOf(id) + "-split") {
		value, ok := event.value.(float64)
		if !ok {
			t.Fatalf("the split channel published %T", event.value)
		}
		if i > 0 && value <= last {
			t.Fatalf("the split channel published %v after %v", value, last)
		}
		last = value
	}

	assertHistoryStepsAddUp(t, gen, result, from, to)

	//and the run really published in parallel: without the pool the peak is one,
	//so this is what tells the two paths apart
	if peak := publisher.peakConcurrency(); peak < 2 {
		t.Errorf("the run never had two readings in flight at once, so it published synchronously")
	}
}

// TestTheHistoryCountersAddUpWhenPublishesFail: the counters are booked when the
// ack arrives, and a refusal arrives the same way a success does.
func TestTheHistoryCountersAddUpWhenPublishesFail(t *testing.T) {
	const id = "env-hist-pool-failures"
	def := historyPoolDocument(id, 8)
	//every third minute of the window is refused, across every channel
	publisher := &fakePublisher{
		latency: func() time.Duration { return time.Duration(rand.Int63n(200)) * time.Microsecond },
		failAt: func(at time.Time) error {
			if at.Unix()%180 == 0 {
				return errors.New("the platform refused this reading")
			}
			return nil
		},
	}
	rt, env, gen := historyFixture(t, def, nil, publisher)

	from, to := historyFrom, historyFrom.Add(time.Hour)
	result := runEngine(t, rt, env, gen, from, to)

	assertHistoryStepsAddUp(t, gen, result, from, to)
	if result.Failed == 0 {
		t.Fatal("the fixture is meant to have readings refused")
	}
	if result.Published == 0 {
		t.Fatal("the fixture is meant to have readings go out as well")
	}
	if !strings.Contains(result.LastError, "refused") {
		t.Errorf("expected the refusal to be named, got %q", result.LastError)
	}
}

// TestAChangeTriggerDecidesAgainstTheAnswerOfItsPreviousPublish is what the
// deferred settle has to keep exact. The channel's value never moves, so only
// the first reading and the heartbeats go out - and while the platform refuses,
// the comparison base stays empty, so every evaluation tries again instead of
// falling silent for a whole heartbeat. A base advanced on a refused reading
// would publish once at 0 and then nothing until 600.
func TestAChangeTriggerDecidesAgainstTheAnswerOfItsPreviousPublish(t *testing.T) {
	const id = "env-hist-cov-refused"
	channel := profileChannel("ch-1", serviceRefOf(id), 600, flatProfile(230, 0))
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 60}
	from := historyFrom
	//the first three evaluations are refused, the rest are taken
	publisher := &fakePublisher{
		latency: func() time.Duration { return 200 * time.Microsecond },
		failAt: func(at time.Time) error {
			if at.Unix()-from.Unix() < 180 {
				return errors.New("the platform refused this reading")
			}
			return nil
		},
	}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	to := from.Add(time.Hour)
	result := runEngine(t, rt, env, gen, from, to)

	//180 is the first reading that was taken, and the heartbeat runs from there
	want := []int64{180, 780, 1380, 1980, 2580, 3180}
	events := publisher.backfilled(serviceRefOf(id))
	if len(events) != len(want) {
		t.Fatalf("expected readings at %v, got %d of them: %v", want, len(events), publishedSeconds(events, from))
	}
	for i, offset := range want {
		if events[i].at.Unix()-from.Unix() != offset {
			t.Fatalf("the readings sit at %v, expected %v", publishedSeconds(events, from), want)
		}
	}

	status := resultFor(t, result, "ch-1")
	steps := backfillTicks(60, from, to)
	if status.Failed != 3 {
		t.Errorf("expected the three refused evaluations to be counted, got %d", status.Failed)
	}
	if status.Published != int64(len(want)) {
		t.Errorf("expected %d published steps, got %d", len(want), status.Published)
	}
	if status.Published+status.Silent+status.Failed != steps {
		t.Errorf("%d published + %d silent + %d failed is not the %d steps of the grid",
			status.Published, status.Silent, status.Failed, steps)
	}

	//and the base the run hands over is the last reading that really went out
	env.mux.Lock()
	defer env.mux.Unlock()
	booked, known := env.state.LastPublished["ch-1"]
	if !known || booked.Value != 230 || booked.AtUnix != from.Unix()+3180 {
		t.Errorf("expected the base of the last publish at 3180, got %#v (known %v)", booked, known)
	}
}

func publishedSeconds(events []publishedEvent, from time.Time) []int64 {
	result := []int64{}
	for _, event := range events {
		result = append(result, event.at.Unix()-from.Unix())
	}
	return result
}

// TestACancelledHistoryRunAccountsForTheReadingsItAccepted: an abort leaves
// readings in the queues, and a run that returned before they were answered
// would report a status that is still moving. What went out is published, what
// was accepted and never sent is silent - nothing was attempted, so nothing was
// refused.
func TestACancelledHistoryRunAccountsForTheReadingsItAccepted(t *testing.T) {
	const id = "env-hist-pool-abort"
	document := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	//slow enough that the loop fills the queue of the shard long before the third
	//reading is acked, so the abort really has readings to drop
	attempts := 0
	publisher := &fakePublisher{
		latency: func() time.Duration { return 5 * time.Millisecond },
		failAt: func(at time.Time) error {
			attempts++
			if attempts == 3 {
				cancel()
			}
			return nil
		},
	}
	rt, env, gen := historyFixture(t, document, nil, publisher)

	before := goruntime.NumGoroutine()
	result, err := rt.runHistory(ctx, env, gen, historyFrom, historyFrom.Add(time.Hour), keepTheWindow, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the run to report the cancellation, got %v", err)
	}

	status := resultFor(t, result, "ch-1")
	if sent := int64(len(publisher.backfilled(serviceRefOf(id)))); status.Published != sent {
		t.Errorf("the run counted %d readings as published, the platform received %d", status.Published, sent)
	}
	if status.Failed != 0 {
		t.Errorf("an abort is not a refusal, but %d readings were counted as failed", status.Failed)
	}
	if status.Silent == 0 {
		t.Fatal("expected the readings the pool had accepted to be booked as silent")
	}
	if !strings.Contains(status.LastError, "aborted") || !strings.Contains(result.LastError, "aborted") {
		t.Errorf("expected the abort to be named as the reason, got %q / %q", status.LastError, result.LastError)
	}
	//an aborted run stops mid-window, so the steps it did not reach are not
	//counted at all - but what it did reach is accounted for exactly once
	if steps := backfillTicks(1, historyFrom, historyFrom.Add(time.Hour)); status.Published+status.Silent+status.Failed > steps {
		t.Errorf("the run booked %d steps, more than the %d of its grid",
			status.Published+status.Silent+status.Failed, steps)
	}

	//runHistory closes its pool before it returns, so nothing of it outlives the
	//run: a worker still holding a reading would publish into an environment the
	//live simulation has taken back
	if !waitFor(5*time.Second, func() bool { return goruntime.NumGoroutine() <= before }) {
		t.Errorf("the aborted run left %d goroutines behind", goruntime.NumGoroutine()-before)
	}
}

// A refusal that follows an abort must not overwrite it, and an abort must not
// overwrite a refusal: the platform's message is what a caller needs.
func TestAnAbortDoesNotOverwriteThePlatformsMessage(t *testing.T) {
	const id = "env-hist-abort-error"
	document := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0
	publisher := &fakePublisher{
		latency: func() time.Duration { return 5 * time.Millisecond },
		failAt: func(at time.Time) error {
			attempts++
			if attempts == 2 {
				cancel()
			}
			return errors.New("the platform refused this reading")
		},
	}
	rt, env, gen := historyFixture(t, document, nil, publisher)

	result, err := rt.runHistory(ctx, env, gen, historyFrom, historyFrom.Add(time.Hour), keepTheWindow, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the run to report the cancellation, got %v", err)
	}
	status := resultFor(t, result, "ch-1")
	if !strings.Contains(status.LastError, "refused") {
		t.Errorf("the abort overwrote the platform's message: %q", status.LastError)
	}
	if !strings.Contains(result.LastError, "refused") {
		t.Errorf("the abort overwrote the platform's message in the totals: %q", result.LastError)
	}
	if status.Failed == 0 {
		t.Error("expected the refused readings to be counted as failed")
	}
}

// TestABackfillPublishesEveryChannelOfAJobInOrder: twelve channels on two
// workers, so several share one, driven the way the job drives them - one
// channel at a time. The order inside a channel is exact, and the peak of one
// says why the job gains no throughput from the pool: one channel is one shard.
func TestABackfillPublishesEveryChannelOfAJobInOrder(t *testing.T) {
	const id = "env-bf-pool-order"
	const channels = 12
	list := []domain.Channel{}
	for i := 0; i < channels; i++ {
		list = append(list, profileChannel(fmt.Sprintf("ch-%d", i), fmt.Sprintf("%s-%d", serviceRefOf(id), i),
			60, flatProfile(float64(100+i), 0)))
	}
	document := testEnvironment(id, list...)
	publisher := &fakePublisher{latency: func() time.Duration {
		return time.Duration(rand.Int63n(300)) * time.Microsecond
	}}
	cfg := testConfig(time.Hour)
	cfg.PublishWorkers = 2
	rt := newRuntime(cfg, newFakeEnvironments(document), newFakeStates(), nil, publisher)
	gen := newGeneration(document, nil)

	from := historyFrom
	to := from.Add(time.Hour)
	pool := newPublishPool(context.Background(), rt.publishWorkers, rt.backfillPublisher())
	defer pool.Close()

	//several channels share a worker, or the test would not cover the sharding
	shards := map[int]int{}
	for i := 0; i < channels; i++ {
		shards[pool.shardOf(fmt.Sprintf("ch-%d", i))]++
	}
	if len(shards) != 2 {
		t.Fatalf("expected the twelve channels to sit on two workers, got %d", len(shards))
	}

	job := &backfillJob{done: make(chan struct{}), status: BackfillStatus{EnvironmentId: id}}
	statuses := []BackfillChannelStatus{}
	for _, channel := range backfillChannels(document) {
		status := BackfillChannelStatus{ChannelId: channel.channel.Id}
		rt.runBackfillChannel(context.Background(), pool, job, gen, channel, nil, from, to, &status)
		statuses = append(statuses, status)
	}

	steps := backfillTicks(60, from, to)
	for i := 0; i < channels; i++ {
		ref := fmt.Sprintf("%s-%d", serviceRefOf(id), i)
		events := publisher.backfilled(ref)
		if int64(len(events)) != steps {
			t.Fatalf("%v: expected the %d steps of the grid, got %d", ref, steps, len(events))
		}
		for j, event := range events {
			if want := from.Add(time.Duration(j) * time.Minute); !event.at.Equal(want) {
				t.Fatalf("%v: reading %d is stamped %v, expected %v - the job published out of order", ref, j, event.at, want)
			}
			if event.value != float64(100+i) {
				t.Fatalf("%v: reading %d is %v, expected the channel's own %v", ref, j, event.value, float64(100+i))
			}
		}
	}
	//and the counters are final when a channel returns, which is what the job
	//adds up afterwards
	for _, status := range statuses {
		if status.Published+status.Silent+status.Failed != steps {
			t.Errorf("%v: %d published + %d silent + %d failed is not the %d steps of the grid",
				status.ChannelId, status.Published, status.Silent, status.Failed, steps)
		}
	}
	if peak := publisher.peakConcurrency(); peak != 1 {
		t.Errorf("the job had %d readings in flight at once; it works one channel at a time, and one channel is one shard", peak)
	}
}

// A cancelled backfill reports no failed readings: the ones it never sent were
// not refused, and the state of the job is what says why they are missing.
func TestACancelledBackfillReportsNoFailedReadings(t *testing.T) {
	const id = "env-bf-cancel-silent"
	document := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	publisher := &fakePublisher{latency: func() time.Duration { return 5 * time.Millisecond }}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(document), newFakeStates(), publisher)

	from := historyFrom
	if _, err := rt.StartBackfill(id, from, from.Add(time.Hour)); err != nil {
		t.Fatalf("unable to start the backfill: %v", err)
	}
	if !waitFor(5*time.Second, func() bool { return len(publisher.backfilled(serviceRefOf(id))) > 2 }) {
		t.Fatal("the job did not publish anything")
	}
	//the unexported one, because a backfill has no endpoint of its own to abort
	//it: a deletion of the environment and the shutdown go through here
	rt.cancelBackfill(id)
	status := waitForBackfill(t, rt, id)

	if status.State != BackfillCancelled {
		t.Errorf("expected the job to be cancelled, it is %v", status.State)
	}
	for _, channel := range status.Channels {
		if channel.Failed != 0 {
			t.Errorf("%v: the cancelled job counted %d readings as failed", channel.ChannelId, channel.Failed)
		}
	}
}

// TestABlockedHistoryRunDoesNotStopTheFlushOfAnotherEnvironment is why the
// submit may not wait. The flusher is one goroutine writing every environment in
// turn and taking each environment's mutex, so a run that holds its own mutex
// while it waits for the platform stops the state of every other environment
// from reaching the database - and a pod that dies in that window loses it.
func TestABlockedHistoryRunDoesNotStopTheFlushOfAnotherEnvironment(t *testing.T) {
	const blocked = "env-hist-blocked"
	const neighbour = "env-hist-neighbour"
	blockedDoc := testEnvironment(blocked, profileChannel("ch-1", serviceRefOf(blocked), 1, flatProfile(230, 0)))
	//a cumulative meter, so every live tick of the neighbour changes its state
	//and gives the flusher something to write
	neighbourDoc := testEnvironment(neighbour,
		profileChannel("ch-1", serviceRefOf(neighbour), 1, domain.ProfileSource{Base: 3600, Cumulative: true}))

	//every timestamped publish blocks, so the run of the first environment gets
	//stuck with its first readings
	publisher := &fakePublisher{gate: make(chan struct{})}
	states := newFakeStates()
	rt := startRuntime(t, testConfig(50*time.Millisecond),
		newFakeEnvironments(blockedDoc, neighbourDoc), states, publisher)
	//released before the runtime is stopped, which waits for the run
	t.Cleanup(func() { close(publisher.gate) })

	if _, err := rt.StartHistory(blocked, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}

	//three writes, so a flusher that got through one round by chance is not
	//mistaken for one that keeps running
	if !waitFor(15*time.Second, func() bool { return states.savesOf(neighbour) >= 3 }) {
		t.Errorf("the neighbouring environment was written %d times while the run was stuck",
			states.savesOf(neighbour))
	}
	//and the run really is stuck, or the assertion above would prove nothing
	status, err := rt.HistoryStatusOf(blocked)
	if err != nil {
		t.Fatalf("the run is not known: %v", err)
	}
	if status.State != HistoryRunning {
		t.Errorf("the run is %v, so it was not blocked by the publisher", status.State)
	}
}

// TestACovScriptThatSendsTwiceInOneRunSettlesInBetween: a script may call send
// several times in one evaluation, and the second value is decided against what
// the first one left. Without the settle in between the run hangs rather than
// getting it wrong, which is why the deadline is the assertion.
func TestACovScriptThatSendsTwiceInOneRunSettlesInBetween(t *testing.T) {
	const id = "env-hist-cov-two-sends"
	channel := scriptChannel("ch-1", domain.Sensor, 600, serviceRefOf(id),
		"moses.channel.send(1); moses.channel.send(100);")
	channel.PublishOnChange = &domain.ChangeTrigger{Absolute: 5, EvaluateIntervalSeconds: 60}
	publisher := &fakePublisher{latency: func() time.Duration { return 200 * time.Microsecond }}
	rt, env, gen := historyFixture(t, testEnvironment(id, channel), nil, publisher)

	from, to := historyFrom, historyFrom.Add(10*time.Minute)
	type outcome struct {
		result HistoryResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := rt.runHistory(context.Background(), env, gen, from, to, keepTheWindow, nil)
		done <- outcome{result: result, err: err}
	}()

	got := outcome{}
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not finish: without the settle before the second send of one script run, the channel's single ack slot overflows and the drain waits forever")
	}
	if got.err != nil {
		t.Fatalf("the run failed: %v", got.err)
	}

	//both values go out at every instant: the first because 100 is far from 1,
	//the second because 1 is far from 100
	steps := backfillTicks(60, from, to)
	//read through asFloat rather than a type assertion: otto exports an integral
	//javascript number as int64, and the gate reads it the same way
	values := []float64{}
	for _, event := range publisher.backfilled(serviceRefOf(id)) {
		number, numeric := asFloat(event.value)
		if !numeric {
			t.Fatalf("the script published a %T", event.value)
		}
		values = append(values, number)
	}
	if int64(len(values)) != steps*2 {
		t.Fatalf("expected two readings for each of the %d steps, got %d: %v", steps, len(values), values)
	}
	for i, value := range values {
		want := 1.0
		if i%2 == 1 {
			want = 100.0
		}
		if value != want {
			t.Fatalf("reading %d is %v, expected %v: %v", i, value, want, values)
		}
	}

	//one step is booked once, however many attempts it carried
	status := resultFor(t, got.result, "ch-1")
	if status.Published != steps || status.Silent != 0 || status.Failed != 0 {
		t.Errorf("expected %d published, 0 silent and 0 failed, got %d/%d/%d",
			steps, status.Published, status.Silent, status.Failed)
	}

	//and the base the run hands over is the last value that went out
	env.mux.Lock()
	defer env.mux.Unlock()
	booked, known := env.state.LastPublished["ch-1"]
	if !known || booked.Value != 100 {
		t.Errorf("expected the second value of the last evaluation as the base, got %#v (known %v)", booked, known)
	}
}

// A first publish that failed never reached the produce, so it never created the
// topic either: the next reading of that topic has to be a first publish again.
func TestAFailedFirstPublishLeavesTheTopicCold(t *testing.T) {
	forgetWarmTopics()
	failing := atomic.Bool{}
	failing.Store(true)
	pool := newPublishPool(context.Background(), 2, func(job publishJob) (bool, error) {
		if failing.Load() {
			return false, errors.New("unable to get an access token")
		}
		return true, nil
	})
	defer pool.Close()

	topic := model.ServiceIdToTopic("urn:infai:ses:service:" + t.Name() + ":ch-1")
	warm := func() bool {
		publishWarmMux.RLock()
		defer publishWarmMux.RUnlock()
		return publishWarm[topic]
	}

	pool.Submit(poolJob(t.Name(), "ch-1", 0, func(bool, error) {}))
	pool.Drain()
	if warm() {
		t.Error("a publish that failed marked its topic as created, so the next ones would produce to it in parallel")
	}

	failing.Store(false)
	pool.Submit(poolJob(t.Name(), "ch-1", 1, func(bool, error) {}))
	pool.Drain()
	if !warm() {
		t.Error("a publish that went out did not mark its topic as created")
	}
}

// TestARunThatBrokeDoesNotPublishWhatItHadStaged: a panic on the publish path
// leaves the pool with a backlog, and Close waits for that backlog rather than
// dropping it - so a run that broke aborts the pool first. Sending it would put
// readings of a half computed window on the platform and book no comparison base
// for them, which the live simulation then compares against.
func TestARunThatBrokeDoesNotPublishWhatItHadStaged(t *testing.T) {
	const id = "env-hist-broken"
	document := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 1, flatProfile(230, 0)))
	attempts := atomic.Int64{}
	publisher := &fakePublisher{
		latency: func() time.Duration { return time.Millisecond },
		failAt: func(at time.Time) error {
			if attempts.Add(1) == 3 {
				panic("a reconstruction bug")
			}
			return nil
		},
	}
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(document), newFakeStates(), publisher)

	if _, err := rt.StartHistory(id, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("unable to start the history run: %v", err)
	}
	status := waitForHistory(t, rt, id)
	if status.State != HistoryFailed {
		t.Fatalf("expected the run to be failed, it is %v (%v)", status.State, status.Error)
	}
	if !strings.Contains(status.Error, "a reconstruction bug") {
		t.Errorf("expected the panic to be named, got %q", status.Error)
	}
	//an hour on a one second grid is 3601 steps, and the loop had staged its
	//per-worker share of them when the panic arrived. Only the readings that
	//were already in the send may still have gone out.
	if sent := len(publisher.backfilled(serviceRefOf(id))); sent > 12 {
		t.Errorf("the broken run published %d readings, so Close sent what it had staged", sent)
	}
}
