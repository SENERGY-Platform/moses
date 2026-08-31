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
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

type fakeFetcher struct {
	mux    sync.Mutex
	points []dataset.Point
	calls  []map[string]string
	// budgets is the context each call was handed, in call order. A real fetch
	// of a long window needs minutes, so what bounds it is worth an assertion.
	budgets []ctxBudget
}

func (this *fakeFetcher) Fetch(ctx context.Context, token string, deviceId string, serviceId string, column string, start time.Time, end time.Time) ([]dataset.Point, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.calls = append(this.calls, map[string]string{
		"token": token, "device": deviceId, "service": serviceId, "column": column,
		"window": end.Sub(start).String(),
	})
	this.budgets = append(this.budgets, budgetOf(ctx))
	//the real client passes the context to the request; a fake that ignored it
	//would let a broken budget look healthy
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return this.points, nil
}

// budgetAt returns the context of the nth fetch, counted from zero.
func (this *fakeFetcher) budgetAt(index int) ctxBudget {
	this.mux.Lock()
	defer this.mux.Unlock()
	if index >= len(this.budgets) {
		return ctxBudget{}
	}
	return this.budgets[index]
}

func (this *fakeFetcher) callCount() int {
	this.mux.Lock()
	defer this.mux.Unlock()
	return len(this.calls)
}

func platformChannel(envId string) domain.Channel {
	return domain.Channel{
		Id: "ch-real", Name: "real", Direction: domain.Sensor,
		ExternalRef: serviceRefOf(envId), IntervalSeconds: 1,
		Source: domain.Source{Kind: domain.SourceDataset, Dataset: &domain.DatasetSource{
			Origin: domain.OriginPlatform, Ref: "urn:device:real", ServiceRef: "urn:service:real",
			Column: "energy.value", Window: "7d",
			Resample: domain.ResampleHold, Anchor: domain.AnchorLoop,
		}},
	}
}

func TestAPlatformChannelReplaysTheFetchedWindow(t *testing.T) {
	now := time.Now().Unix()
	fetcher := &fakeFetcher{points: []dataset.Point{
		{Unix: now - 7200, Value: 11}, {Unix: now - 3600, Value: 22}, {Unix: now, Value: 33},
	}}
	env := testEnvironment("env-real", platformChannel("env-real"))
	env.Owner = "owner-42"
	publisher := &fakePublisher{}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, publisher)
	rt.fetcher = fetcher
	rt.ownerToken = func(userId string) (string, error) { return "Bearer token-for-" + userId, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)

	if !waitFor(5*time.Second, func() bool { return publisher.count() >= 2 }) {
		t.Fatalf("the platform replay did not publish, count %d", publisher.count())
	}
	for _, event := range publisher.all() {
		v := event.value.(float64)
		if v != 11 && v != 22 && v != 33 {
			t.Fatalf("expected only fetched values, got %v", v)
		}
	}
	//the fetch ran with the owner's token, the declared column and the window
	if len(fetcher.calls) != 1 {
		t.Fatalf("expected one fetch per start, got %d", len(fetcher.calls))
	}
	call := fetcher.calls[0]
	if call["token"] != "Bearer token-for-owner-42" {
		t.Errorf("the fetch has to use the owner's token, got %q", call["token"])
	}
	if call["device"] != "urn:device:real" || call["service"] != "urn:service:real" || call["column"] != "energy.value" {
		t.Errorf("wrong query: %v", call)
	}
	if call["window"] != "168h0m0s" {
		t.Errorf("expected a 7d window, got %q", call["window"])
	}
}

func TestAPlatformChannelWithoutAWrapperIsSkippedNotFatal(t *testing.T) {
	env := testEnvironment("env-nowrap",
		platformChannel("env-nowrap"),
		scriptChannel("ch-b", domain.Sensor, 1, serviceRefOf("env-nowrap")+"-b", `moses.service.send("alive");`))
	publisher := &fakePublisher{}
	//fetcher stays nil: no timescale_wrapper_url configured
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(env), newFakeStates(), nil, publisher)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Stop)
	if !waitFor(4*time.Second, func() bool { return publisher.count() > 0 }) {
		t.Fatal("the sibling channel has to keep running")
	}
}
