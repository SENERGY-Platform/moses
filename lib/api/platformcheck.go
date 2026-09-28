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

package api

import (
	"context"
	"time"

	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/domain"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"
)

// platformCheckTimeout bounds the reads of the platform check. It runs before
// anything is written, so giving up leaves nothing behind.
const platformCheckTimeout = 30 * time.Second

// checkPlatform reads what env names on the platform and returns the problems
// of those references; an error means the platform could not be read. previous
// is the stored document on a PUT and nil otherwise.
func checkPlatform(gc *gin.Context, catalog DeviceCatalog, token sc_jwt.Token, previous *domain.Environment, env *domain.Environment) ([]domain.Problem, error) {
	if catalog == nil {
		return nil, nil
	}
	references, nodes := platformReferences(previous, *env)
	//Validate refuses such a document anyway, and it must not become that many reads
	if nodes > domain.MaxNodes || len(references) == 0 {
		return nil, nil
	}
	//the request context and not a detached one: nothing is written yet
	ctx, cancel := context.WithTimeout(gc.Request.Context(), platformCheckTimeout)
	defer cancel()
	return catalog.CheckReferences(ctx, token.Jwt(), references)
}

// platformReferences lists what every asset names on the platform. A device the
// stored document already carried with the same device type is not read again:
// the device-repository trails the device-manager, so a device the previous save
// created may not be readable yet.
func platformReferences(previous *domain.Environment, env domain.Environment) ([]devices.AssetReference, int) {
	type attachment struct{ device, deviceType string }
	stored := map[attachment]bool{}
	forEachAsset(previous, func(asset *domain.Asset) {
		if asset.ExternalRef != "" {
			stored[attachment{asset.ExternalRef, asset.ExternalTypeId}] = true
		}
	})
	references := []devices.AssetReference{}
	//Validate refuses an oversized id, and read it would fail the whole query
	bounded := func(id string) string {
		if len(id) > domain.MaxExternalIdLength {
			return ""
		}
		return id
	}
	nodes := domain.WalkAssets(env, func(path string, asset domain.Asset) {
		reference := devices.AssetReference{Path: path, DeviceTypeId: bounded(asset.ExternalTypeId)}
		if asset.ExternalRef != "" && !stored[attachment{asset.ExternalRef, asset.ExternalTypeId}] {
			reference.DeviceId = bounded(asset.ExternalRef)
		}
		for i, channel := range asset.Channels {
			reference.Channels = append(reference.Channels, devices.ChannelReference{
				Path:      domain.ChannelPath(path, i),
				ServiceId: bounded(channel.ExternalRef),
				Publishes: channel.Direction == domain.Sensor,
			})
		}
		references = append(references, reference)
	})
	return references, nodes
}
