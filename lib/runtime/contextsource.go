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
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// contextSeriesId names a context source's series in the loaded-series map and
// its replay anchor in the state. Prefixed so it can never collide with a
// channel id, which shares both namespaces.
func contextSeriesId(key string) string {
	return "context:" + key
}

// runContextSource drives one context key over time. Without it a context key
// keeps its initial value forever, which is what made the context look inert:
// zones and formulas read it, but nothing moved it.
func (this *Runtime) runContextSource(ctx context.Context, env *environment, gen *generation, key string, source domain.Source) {
	defer env.runners.Done()
	interval := source.IntervalSeconds
	if interval <= 0 || interval > maxIntervalSeconds {
		//validation refuses this on the way in, so it bypassed the api
		util.Logger.Warn("context source interval is unusable, the key stays static",
			"environment", env.id, "key", key, "interval_seconds", interval)
		return
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			this.tickContextSource(env, gen, key, source)
		}
	}
}

func (this *Runtime) tickContextSource(env *environment, gen *generation, key string, source domain.Source) {
	env.mux.Lock()
	defer env.mux.Unlock()
	switch source.Kind {
	case domain.SourceProfile:
		if source.Profile == nil {
			return
		}
		value := profileValue(*source.Profile, gen.def.Seed, contextSeriesId(key), source.IntervalSeconds, time.Now())
		env.contextStates()[key] = value
		env.dirty = true
	case domain.SourceDataset:
		points := gen.series[contextSeriesId(key)]
		if len(points) < 2 {
			return //the loader already reported the missing dataset
		}
		anchor := this.anchorFor(env, contextSeriesId(key), source.Dataset)
		value, playable := replayValue(*source.Dataset, points, anchor, time.Now(), source.IntervalSeconds)
		if !playable {
			return
		}
		env.contextStates()[key] = value
		env.dirty = true
	}
}

// anchorFor returns the persisted replay anchor, creating it on first use -
// the same bookkeeping executeDataset does for channels, under the same map.
// Must be called with env.mux held.
func (this *Runtime) anchorFor(env *environment, id string, source *domain.DatasetSource) int64 {
	if source.Anchor == domain.AnchorOriginal {
		return 0
	}
	anchor, known := env.state.Anchors[id]
	if !known {
		anchor = time.Now().Unix()
		if env.state.Anchors == nil {
			env.state.Anchors = map[string]int64{}
		}
		env.state.Anchors[id] = anchor
		env.dirty = true
	}
	return anchor
}

// loadContextSeries fetches the datasets context sources replay, alongside the
// channel series and into the same map, under the collision-safe id.
func (this *Runtime) loadContextSeries(ctx context.Context, def domain.Environment, result map[string][]dataset.Point) {
	for key, source := range def.ContextSources {
		if source.Kind != domain.SourceDataset || source.Dataset == nil {
			continue
		}
		if source.Dataset.Origin != domain.OriginFile && source.Dataset.Origin != domain.OriginPlatform {
			continue
		}
		points, err := this.fetchSeries(ctx, def.Owner, source.Dataset)
		if err != nil {
			util.Logger.Warn("unable to load the dataset of a context source, the key stays static",
				attributes.ErrorKey, err, "environment", def.Id, "key", key, "dataset", source.Dataset.Ref)
			continue
		}
		result[contextSeriesId(key)] = points
	}
}
