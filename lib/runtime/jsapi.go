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
	"github.com/SENERGY-Platform/moses/lib/util"
	"time"
)

// The javascript surface is the legacy one: a migrated channel carries its
// script verbatim, so moses.world, room, device and service keep their meaning,
// now against the environment, the asset's zone, the asset and the channel. The
// new names are aliases onto the same maps:
//
//	moses.world   == moses.environment
//	moses.room    == moses.zone
//	moses.device  == moses.asset
//	moses.service == moses.channel
//
// Everything below runs inside a script run with the environment mutex held,
// which is what makes the state maps safe without locking of their own.
func (this *Runtime) jsApi(env *environment, gen *generation, binding channelBinding, input interface{}, send func(value interface{})) map[string]interface{} {
	environmentApi := this.jsEnvironmentApi(env, gen)
	zoneApi := this.jsZoneApi(env, gen, binding.zoneId)
	assetApi := this.jsAssetApi(env, binding.asset.id)
	channelApi := map[string]interface{}{
		"input": input,
		"send":  send,
	}
	return map[string]interface{}{
		"world":   environmentApi,
		"room":    zoneApi,
		"device":  assetApi,
		"service": channelApi,

		//aliases, deliberately the same maps and not copies
		"environment": environmentApi,
		"zone":        zoneApi,
		"asset":       assetApi,
		"channel":     channelApi,
	}
}

func (this *Runtime) jsEnvironmentApi(env *environment, gen *generation) map[string]interface{} {
	return map[string]interface{}{
		"state": jsStateApi(env, func() map[string]interface{} { return env.contextStates() }),
		"getRoom": func(zoneId string) map[string]interface{} {
			if _, known := gen.zones[zoneId]; !known {
				util.Logger.Warn("no zone for id found", "environment", env.id, "id", zoneId)
				return map[string]interface{}{}
			}
			return this.jsZoneApi(env, gen, zoneId)
		},
	}
}

func (this *Runtime) jsZoneApi(env *environment, gen *generation, zoneId string) map[string]interface{} {
	return map[string]interface{}{
		//a zone value with a time constant is resolved here rather than on a
		//ticker: the mutex is held, so this is the one place it can be exact
		"state": jsStateApi(env, func() map[string]interface{} {
			env.advanceZone(zoneId, time.Now())
			return env.zoneStates(zoneId)
		}),
		"getDevice": func(assetId string) map[string]interface{} {
			asset, known := gen.assets[assetId]
			//the asset has to sit in this zone, exactly like the legacy
			//room.getDevice() only found the devices of that room
			if !known || asset.zoneId != zoneId {
				util.Logger.Warn("no asset for id found in this zone", "environment", env.id, "zone", zoneId, "id", assetId)
				return map[string]interface{}{}
			}
			return this.jsAssetApi(env, assetId)
		},
	}
}

func (this *Runtime) jsAssetApi(env *environment, assetId string) map[string]interface{} {
	return map[string]interface{}{
		"state": jsStateApi(env, func() map[string]interface{} { return env.assetStates(assetId) }),
	}
}

// jsStateApi is the get/set pair of one scope. states() resolves the map late,
// so a scope whose map does not exist yet is created on first use.
//
// get seeds a missing key with 0 and returns it, as the legacy api did: a
// migrated script relies on "state.get() + 1" working on the first tick. The
// seeding is a state change, so it marks the environment dirty; it happens once
// per key, because the next get finds the key.
func jsStateApi(env *environment, states func() map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"get": func(field string) interface{} {
			target := states()
			value, ok := target[field]
			if !ok {
				target[field] = 0
				env.dirty = true
				return 0
			}
			return value
		},
		"set": func(field string, value interface{}) {
			states()[field] = value
			env.dirty = true
		},
	}
}
