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

package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
)

// AssetReference is what one asset of a document names on the platform, with
// the path Validate reports that asset's problems under.
type AssetReference struct {
	Path         string
	DeviceTypeId string
	// DeviceId is the attached device to read; empty where the asset has none
	// yet, or where it need not be read again.
	DeviceId string
	Channels []ChannelReference
}

// ChannelReference is the platform service one channel publishes to.
type ChannelReference struct {
	Path      string
	ServiceId string
	// Publishes marks a sensor: its service has to take a reading moses sends.
	Publishes bool
}

// maxIdsPerQuery bounds the ids of one list query, since they travel in the url.
const maxIdsPerQuery = 50

// maxRepositoryAnswerBytes bounds what one answer of the device-repository may hold.
const maxRepositoryAnswerBytes = 32 << 20

// attachedDevicePermission is what the caller has to hold on a device they attach:
// moses publishes into it with its own account, so reading it is not enough.
const attachedDevicePermission = models.Write

// CheckReferences reads, and only reads, the device types and devices the
// assets name and reports each reference provisioning or the runtime could not
// use as a problem at its path. An error means the platform could not be read,
// which says nothing about the document.
//
// Every distinct id is read once, in list queries by id, so a document with many
// assets of few types costs a few requests and not one per asset.
func (this *Catalog) CheckReferences(ctx context.Context, token string, assets []AssetReference) ([]domain.Problem, error) {
	typeIds := distinctIds(assets, func(asset AssetReference) string {
		//an empty one is reported by Validate, and there is nothing to read for it
		if strings.TrimSpace(asset.DeviceTypeId) == "" {
			return ""
		}
		return asset.DeviceTypeId
	})
	deviceIds := distinctIds(assets, func(asset AssetReference) string { return asset.DeviceId })

	protocolId := ""
	types := map[string]models.DeviceType{}
	if len(typeIds) > 0 {
		var err error
		if protocolId, err = this.protocolIdWithin(ctx, token); err != nil {
			return nil, err
		}
		if err = readByIds(ctx, this, token, "/v3/device-types", typeIds, nil, func(deviceType models.DeviceType) {
			types[deviceType.Id] = deviceType
		}); err != nil {
			return nil, err
		}
	}
	attached := map[string]models.Device{}
	if len(deviceIds) > 0 {
		permission := url.Values{"p": {string(attachedDevicePermission)}}
		if err := readByIds(ctx, this, token, "/devices", deviceIds, permission, func(device models.Device) {
			attached[device.Id] = device
		}); err != nil {
			return nil, err
		}
	}

	problems := []domain.Problem{}
	fail := func(path string, format string, args ...interface{}) {
		problems = append(problems, domain.Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	for _, asset := range assets {
		checkAttachedDevice(asset, attached, fail)
		checkDeviceType(asset, types, protocolId, this.protocol, fail)
	}
	return problems, nil
}

func checkAttachedDevice(asset AssetReference, attached map[string]models.Device, fail func(string, string, ...interface{})) {
	if asset.DeviceId == "" {
		return
	}
	device, found := attached[asset.DeviceId]
	switch {
	case !found:
		fail(asset.Path+".external_ref", "the device %q does not exist, or the caller may not write it: moses publishes into an attached device, so attaching one takes the right to write it", asset.DeviceId)
	case strings.TrimSpace(asset.DeviceTypeId) != "" && device.DeviceTypeId != asset.DeviceTypeId:
		//the runtime publishes through the services of the device's own type
		fail(asset.Path+".external_ref", "the device %q is of the device type %q, not of %q named in external_type_id, so the services of its channels are not the device's",
			asset.DeviceId, device.DeviceTypeId, asset.DeviceTypeId)
	}
}

func checkDeviceType(asset AssetReference, types map[string]models.DeviceType, protocolId string, protocol string, fail func(string, string, ...interface{})) {
	if strings.TrimSpace(asset.DeviceTypeId) == "" {
		return
	}
	deviceType, found := types[asset.DeviceTypeId]
	if !found {
		fail(asset.Path+".external_type_id", "the device type %q does not exist, or the caller may not read it", asset.DeviceTypeId)
		return
	}
	services := map[string]models.Service{}
	ours := false
	for _, service := range deviceType.Services {
		services[service.Id] = service
		ours = ours || service.ProtocolId == protocolId
	}
	if !ours {
		fail(asset.Path+".external_type_id", "the device type %q has no service of the protocol %q, so nothing moses sends reaches a device of it", asset.DeviceTypeId, protocol)
	}
	for _, channel := range asset.Channels {
		if channel.ServiceId == "" {
			continue
		}
		service, found := services[channel.ServiceId]
		switch {
		case !found:
			fail(channel.Path+".external_ref", "the device type %q has no service %q", asset.DeviceTypeId, channel.ServiceId)
		case service.ProtocolId != protocolId:
			fail(channel.Path+".external_ref", "the service %q belongs to another protocol than %q, so moses cannot publish through it", channel.ServiceId, protocol)
		case channel.Publishes:
			//the publisher refuses every reading for such a service, see connectorPublisher.body
			if _, err := ResolveTimeShape(service); err != nil && !errors.Is(err, ErrNoTimePath) {
				fail(channel.Path+".external_ref", "the service %q cannot take a reading from moses: %v", channel.ServiceId, err)
			}
		}
	}
}

func distinctIds(assets []AssetReference, idOf func(AssetReference) string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, asset := range assets {
		id := idOf(asset)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
}

// protocolIdWithin is ProtocolId on a context, sharing its cache.
func (this *Catalog) protocolIdWithin(ctx context.Context, token string) (string, error) {
	if cached := this.cachedProtocolId(); cached != "" {
		return cached, nil
	}
	protocols := []models.Protocol{}
	query := url.Values{"limit": {"1000"}, "offset": {"0"}, "sort": {"name.asc"}}
	if err := this.getRepository(ctx, token, "/protocols?"+query.Encode(), &protocols); err != nil {
		return "", err
	}
	return this.rememberProtocol(protocols)
}

// readByIds lists what path holds for ids, a bounded chunk per query. What the
// repository leaves out of an answer does not exist, or the caller may not see it.
func readByIds[T any](ctx context.Context, catalog *Catalog, token string, path string, ids []string, extra url.Values, visit func(T)) error {
	for start := 0; start < len(ids); start += maxIdsPerQuery {
		end := min(start+maxIdsPerQuery, len(ids))
		query := url.Values{"ids": {strings.Join(ids[start:end], ",")}}
		for key, values := range extra {
			query[key] = values
		}
		page := []T{}
		if err := catalog.getRepository(ctx, token, path+"?"+query.Encode(), &page); err != nil {
			return err
		}
		for _, element := range page {
			visit(element)
		}
	}
	return nil
}

// getRepository reads one answer of the device-repository with the caller's
// token. Anything but a 200 is an error: a list by ids has no 404.
func (this *Catalog) getRepository(ctx context.Context, token string, pathAndQuery string, result interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, this.repoUrl+pathAndQuery, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", token)
	response, err := this.clients.Reads().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("the device-repository answered %d: %s", response.StatusCode, message)
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, maxRepositoryAnswerBytes)).Decode(result); err != nil {
		return fmt.Errorf("the device-repository answered unreadably: %w", err)
	}
	return nil
}
