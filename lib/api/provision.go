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
	"fmt"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/util"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
)

// provisionDevices registers a platform device for every asset that names a
// device type but carries no device yet, and writes the id back.
//
// This happens when an environment is stored, not when an editor adds an
// asset. An editor that creates the device up front leaves one behind for
// every edit that is abandoned, discarded or refused by validation - the
// device would exist while the asset that justified it never does. Storing is
// the first moment the asset is real.
//
// It runs after validation and before the write, so a document that is
// rejected creates nothing. A failure here fails the request: an asset stored
// without its device would publish nowhere, silently.
//
// Re-storing the same document creates nothing, because the reference is set
// by then - which is what makes a retry after a failed write safe.
//
// It returns the devices it created, because they are the ones that still have
// to inherit what the environment as a whole already granted.
func provisionDevices(ctx context.Context, catalog DeviceCatalog, token sc_jwt.Token, env *domain.Environment) ([]managedDevice, error) {
	created := []managedDevice{}
	if catalog == nil {
		return created, nil
	}
	for i := range env.Zones {
		if err := provisionZone(ctx, catalog, token, &env.Zones[i], &created); err != nil {
			return created, err
		}
	}
	if len(created) > 0 {
		util.Logger.Info("created platform devices for new assets", "environment", env.Id, "devices", len(created))
	}
	return created, nil
}

func provisionZone(ctx context.Context, catalog DeviceCatalog, token sc_jwt.Token, zone *domain.Zone, created *[]managedDevice) error {
	for i := range zone.Zones {
		if err := provisionZone(ctx, catalog, token, &zone.Zones[i], created); err != nil {
			return err
		}
	}
	for i := range zone.Assets {
		asset := &zone.Assets[i]
		if asset.ExternalRef != "" || asset.ExternalTypeId == "" {
			continue
		}
		device, err := catalog.CreateDevice(ctx, token.Jwt(), asset.ExternalTypeId, asset.Name)
		if err != nil {
			util.Logger.Error("unable to create the platform device of an asset", attributes.ErrorKey, err,
				"asset", asset.Id, "device_type", asset.ExternalTypeId)
			return fmt.Errorf("unable to create the platform device for %q: %w", asset.Name, err)
		}
		asset.ExternalRef = device.Id
		//this device exists because of this asset, so it goes when the asset goes.
		//The only place the flag is ever set to true.
		asset.ExternalManaged = true
		*created = append(*created, managedDevice{assetId: asset.Id, deviceId: device.Id})
	}
	return nil
}

// managedDevice is one platform device moses created, together with the asset
// that justified it. The asset id is carried for the log line only: what is
// deleted is addressed by the device id.
type managedDevice struct {
	assetId  string
	deviceId string
}

// forEachAsset visits every asset of an environment, in nested zones as well -
// an asset in a sub-zone is not a special case anywhere else and must not become
// one here.
func forEachAsset(env *domain.Environment, visit func(asset *domain.Asset)) {
	if env == nil {
		return
	}
	var walk func(zone *domain.Zone)
	walk = func(zone *domain.Zone) {
		for i := range zone.Zones {
			walk(&zone.Zones[i])
		}
		for i := range zone.Assets {
			visit(&zone.Assets[i])
		}
	}
	for i := range env.Zones {
		walk(&env.Zones[i])
	}
}

// reconcileManagedFlags decides which devices of the document about to be stored
// moses is allowed to delete later. The client sends the whole document, so the
// flag it carries is worth nothing: an editor working from a stale copy echoes
// yesterday's value, and a handcrafted request could set it on a device the user
// picked - which would make moses delete somebody's real device on the next edit.
//
// A device stays managed only where the same asset still carries the same device
// it was provisioned with. Anything else counts as the user's:
//   - an asset that is new to this document, whatever it claims,
//   - an asset whose external_ref changed, because what is there now was picked,
//   - an asset without an external_ref, which provisionZone sets itself.
//
// existing is nil when the document is new; then nothing can be inherited.
func reconcileManagedFlags(existing *domain.Environment, env *domain.Environment) {
	previous := map[string]*domain.Asset{}
	ambiguous := map[string]bool{}
	forEachAsset(existing, func(asset *domain.Asset) {
		if asset.Id == "" {
			return
		}
		if _, taken := previous[asset.Id]; taken {
			//validation makes ids unique document wide, but a document stored
			//before that rule, or migrated from the legacy model, need not obey
			//it. Which of the two an asset continues is then unknowable, and
			//guessing decides whether a device is deleted.
			ambiguous[asset.Id] = true
			return
		}
		previous[asset.Id] = asset
	})
	forEachAsset(env, func(asset *domain.Asset) {
		before, known := previous[asset.Id]
		asset.ExternalManaged = known &&
			!ambiguous[asset.Id] &&
			asset.ExternalRef != "" &&
			before.ExternalRef == asset.ExternalRef &&
			before.ExternalManaged
	})
}

// managedDevicesOf lists the devices of an environment that moses created,
// deduplicated: two assets pointing at the same device would otherwise be
// deleted twice, and the second delete would fail for the wrong reason.
func managedDevicesOf(env *domain.Environment) []managedDevice {
	result := []managedDevice{}
	seen := map[string]bool{}
	forEachAsset(env, func(asset *domain.Asset) {
		if !asset.ExternalManaged || asset.ExternalRef == "" || seen[asset.ExternalRef] {
			return
		}
		seen[asset.ExternalRef] = true
		result = append(result, managedDevice{assetId: asset.Id, deviceId: asset.ExternalRef})
	})
	return result
}

// orphanedDevices are the devices moses created for the stored document and that
// the new one no longer publishes through.
//
// What counts is the device reference, not the asset: an asset whose managed
// device was exchanged for a picked one releases the old device just as much as a
// deleted asset does. And a device still referenced anywhere in the new document
// is kept, even by an asset that does not own it - deleting it would break a
// channel that still publishes.
func orphanedDevices(existing *domain.Environment, env *domain.Environment) []managedDevice {
	stillReferenced := map[string]bool{}
	forEachAsset(env, func(asset *domain.Asset) {
		if asset.ExternalRef != "" {
			stillReferenced[asset.ExternalRef] = true
		}
	})
	result := []managedDevice{}
	for _, device := range managedDevicesOf(existing) {
		if !stillReferenced[device.deviceId] {
			result = append(result, device)
		}
	}
	return result
}

// deleteDevices removes the platform devices of assets that are gone. Best
// effort, and deliberately so: it runs after the document was written, where
// failing the request would claim a change that did happen. What a failure leaves
// behind is a device without an asset - the state moses was in for every removal
// before this existed, and recoverable by hand.
//
// Never called with anything but managedDevicesOf or orphanedDevices output: a
// device the user picked must not reach this function.
func deleteDevices(ctx context.Context, catalog DeviceCatalog, token sc_jwt.Token, environmentId string, obsolete []managedDevice) {
	if catalog == nil || len(obsolete) == 0 {
		return
	}
	deleted := 0
	for _, device := range obsolete {
		//a device that is already gone is not a failure: the catalog reports the
		//device-manager's 404 as success, so a retry after a partial cleanup and
		//a device somebody removed by hand both land here
		if err := catalog.DeleteDevice(ctx, token.Jwt(), device.deviceId); err != nil {
			util.Logger.Warn("unable to delete the platform device of a removed asset", attributes.ErrorKey, err,
				"environment", environmentId, "asset", device.assetId, "device", device.deviceId)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		util.Logger.Info("deleted the platform devices of removed assets", "environment", environmentId, "devices", deleted)
	}
}
