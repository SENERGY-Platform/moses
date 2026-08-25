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

// Package devices serves the device types a simulated asset can be built from,
// and creates the platform devices those assets publish through.
//
// It exists so an editor can offer a choice instead of asking for identifiers:
// a device type carries the names, the service ids, the direction and the
// characteristic of every measuring point, which is everything the channels of
// an asset need.
package devices

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	deviceRepo "github.com/SENERGY-Platform/device-repository/lib/client"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/google/uuid"
)

// Service is one measuring point or manipulated variable of a device type.
type Service struct {
	Id        string           `json:"id"`
	Name      string           `json:"name"`
	Direction domain.Direction `json:"direction"`
	// CharacteristicId and ValuePath come from the output's content variable:
	// the characteristic gives the value its meaning and unit, the path names
	// the variable inside the message.
	CharacteristicId string `json:"characteristic_id"`
	ValuePath        string `json:"value_path"`
}

// DeviceType is a device type usable for simulation, reduced to what building
// an asset from it needs.
type DeviceType struct {
	Id       string    `json:"id"`
	Name     string    `json:"name"`
	Services []Service `json:"services"`
}

// Device is a platform device created for a simulated asset.
type Device struct {
	Id           string `json:"id"`
	LocalId      string `json:"local_id"`
	Name         string `json:"name"`
	DeviceTypeId string `json:"device_type_id"`
}

// registry is the narrow slice of the device-repository this package uses.
// Narrow because it is a boundary: a fake of two methods is testable, a fake of
// the full client is not, and the http paths below are exactly where a contract
// with another service silently drifts.
type registry interface {
	ListDeviceTypesV3(token string, options deviceRepo.DeviceTypeListOptions) ([]models.DeviceType, int64, error, int)
	ListProtocols(token string, limit int64, offset int64, sort string) ([]models.Protocol, error, int)
}

type Catalog struct {
	repo       registry
	managerUrl string
	protocol   string
	// protocolId is resolved once and cached: it never changes while the
	// service runs, and every device type query needs it.
	protocolId string
}

func NewCatalog(deviceRepoUrl string, deviceManagerUrl string, protocol string) *Catalog {
	return &Catalog{
		repo:       deviceRepo.NewClient(deviceRepoUrl, nil),
		managerUrl: deviceManagerUrl,
		protocol:   protocol,
	}
}

// listLimit bounds one page; the platform holds tens of device types, not
// thousands, so the loop below terminates quickly.
const listLimit = 200

// DeviceTypes returns the device types that publish through this service's
// protocol, which is what makes them simulatable at all.
func (this *Catalog) DeviceTypes(token string) ([]DeviceType, error) {
	protocolId, err := this.ProtocolId(token)
	if err != nil {
		return nil, err
	}
	result := []DeviceType{}
	for offset := int64(0); ; offset += listLimit {
		page, _, err, _ := this.repo.ListDeviceTypesV3(token, deviceRepo.DeviceTypeListOptions{
			Limit:       listLimit,
			Offset:      offset,
			ProtocolIds: []string{protocolId},
			SortBy:      "name.asc",
		})
		if err != nil {
			return nil, err
		}
		for _, deviceType := range page {
			result = append(result, convertDeviceType(deviceType, protocolId))
		}
		if int64(len(page)) < listLimit {
			return result, nil
		}
	}
}

// ProtocolId resolves the id of this service's protocol by its handler name.
func (this *Catalog) ProtocolId(token string) (string, error) {
	if this.protocolId != "" {
		return this.protocolId, nil
	}
	protocols, err, _ := this.repo.ListProtocols(token, 1000, 0, "name.asc")
	if err != nil {
		return "", err
	}
	for _, protocol := range protocols {
		if protocol.Handler == this.protocol {
			this.protocolId = protocol.Id
			return protocol.Id, nil
		}
	}
	return "", fmt.Errorf("no protocol with the handler %q exists, so no device type can be simulated", this.protocol)
}

func convertDeviceType(in models.DeviceType, protocolId string) DeviceType {
	result := DeviceType{Id: in.Id, Name: in.Name, Services: []Service{}}
	for _, service := range in.Services {
		//a service of another protocol belongs to the same device but is not
		//ours to drive
		if service.ProtocolId != protocolId {
			continue
		}
		characteristic, path := valueOf(service)
		result.Services = append(result.Services, Service{
			Id:               service.Id,
			Name:             service.Name,
			Direction:        directionOf(service.Interaction),
			CharacteristicId: characteristic,
			ValuePath:        path,
		})
	}
	return result
}

// directionOf maps the platform's interaction onto the simulation's direction.
// event+request counts as a sensor: it is measured and can additionally be
// asked, and what a simulation has to produce on a schedule is the measurement.
func directionOf(interaction models.Interaction) domain.Direction {
	if interaction == models.REQUEST {
		return domain.Actuator
	}
	return domain.Sensor
}

// valueOf finds the leaf that carries the measured value: the first content
// variable with a characteristic, depth first. Its path is dotted without the
// root, which is how the platform's own query api addresses it.
func valueOf(service models.Service) (characteristicId string, path string) {
	for _, output := range service.Outputs {
		if id, found := findCharacteristic(output.ContentVariable, "", &path); found {
			return id, path
		}
	}
	return "", ""
}

func findCharacteristic(variable models.ContentVariable, prefix string, path *string) (string, bool) {
	current := variable.Name
	if prefix != "" {
		current = prefix + "." + variable.Name
	}
	if variable.CharacteristicId != "" {
		//the root is not part of the path the query api expects
		if prefix == "" {
			*path = ""
		} else {
			*path = trimRoot(current)
		}
		return variable.CharacteristicId, true
	}
	for _, sub := range variable.SubContentVariables {
		if id, found := findCharacteristic(sub, current, path); found {
			return id, true
		}
	}
	return "", false
}

func trimRoot(path string) string {
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			return path[i+1:]
		}
	}
	return path
}

// CreateDevice registers a platform device for a simulated asset. The local id
// is generated: it identifies the device towards the protocol and has no
// meaning of its own here.
//
// The caller's own token is used, not a service account, so the device belongs
// to whoever built the asset and the device-manager decides what they may do.
func (this *Catalog) CreateDevice(ctx context.Context, token string, deviceTypeId string, name string) (Device, error) {
	result := Device{}
	body, err := json.Marshal(models.Device{
		Name:         name,
		DeviceTypeId: deviceTypeId,
		LocalId:      uuid.NewString(),
	})
	if err != nil {
		return result, err
	}
	response, err := this.send(ctx, token, http.MethodPost, this.managerUrl+"/devices", body)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	created := models.Device{}
	if err = json.NewDecoder(response.Body).Decode(&created); err != nil {
		return result, fmt.Errorf("the device-manager answered unreadably: %w", err)
	}
	return Device{Id: created.Id, LocalId: created.LocalId, Name: created.Name, DeviceTypeId: created.DeviceTypeId}, nil
}

// DeleteDevice removes a platform device again, so removing a simulated asset
// does not leave one behind.
func (this *Catalog) DeleteDevice(ctx context.Context, token string, id string) error {
	response, err := this.send(ctx, token, http.MethodDelete, this.managerUrl+"/devices/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

// send talks to the device-manager with the caller's token and turns anything
// but a 2xx into an error carrying the answer, bounded: the body is not ours
// and an error page can be large.
func (this *Catalog) send(ctx context.Context, token string, method string, url string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		response.Body.Close()
		return nil, fmt.Errorf("the device-manager answered %d: %s", response.StatusCode, message)
	}
	return response, nil
}
