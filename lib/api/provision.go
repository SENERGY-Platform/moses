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
func provisionDevices(ctx context.Context, catalog DeviceCatalog, token sc_jwt.Token, env *domain.Environment) error {
	if catalog == nil {
		return nil
	}
	created := 0
	for i := range env.Zones {
		if err := provisionZone(ctx, catalog, token, &env.Zones[i], &created); err != nil {
			return err
		}
	}
	if created > 0 {
		util.Logger.Info("created platform devices for new assets", "environment", env.Id, "devices", created)
	}
	return nil
}

func provisionZone(ctx context.Context, catalog DeviceCatalog, token sc_jwt.Token, zone *domain.Zone, created *int) error {
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
		*created++
	}
	return nil
}
