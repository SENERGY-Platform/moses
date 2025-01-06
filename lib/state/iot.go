/*
 * Copyright 2019 InfAI (CC SES)
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

package state

import (
	deviceRepo "github.com/SENERGY-Platform/device-repository/lib/client"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/jwt"
	permClient "github.com/SENERGY-Platform/permissions-v2/pkg/client"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"github.com/google/uuid"
	"log"
	"net/url"
	"runtime/debug"
)

func (this *StateRepo) GetIotDeviceType(jwt jwt.Jwt, id string) (dt model.DeviceType, err error) {
	err = jwt.Impersonate.GetJSON(this.Config.DeviceManagerUrl+"/device-types/"+url.PathEscape(id), &dt)
	if err != nil {
		log.Println("ERROR: unable to get device type", err)
	}
	return
}

func (this *StateRepo) GetIotDeviceTypes(jwt jwt.Jwt) (result []model.DeviceType, err error) {
	err = jwt.Impersonate.GetJSON(this.Config.DeviceManagerUrl+"/device-types", &result)
	if err != nil {
		log.Println("ERROR: unable to query service", err)
	}
	return
}

func (this *StateRepo) GetIotDeviceTypesIds(jwt jwt.Jwt) (result []string, err error) {
	steps := 1000
	limit := 0
	offset := 0
	temp := []string{}
	c := permClient.New(this.Config.PermissionsV2Url)
	for len(temp) == limit {
		limit = steps
		temp, err, _ = c.AdminListResourceIds(permClient.InternalAdminToken, "device-types", permClient.ListOptions{
			Limit:  int64(limit),
			Offset: int64(offset),
		})
		if err != nil {
			return result, err
		}
		result = append(result, temp...)
		offset = offset + limit
	}
	return
}

func (this *StateRepo) GetMosesDeviceTypesIds(jwt jwt.Jwt) (result []string, err error) {
	steps := 1000
	limit := 0
	offset := 0
	temp := []models.DeviceType{}
	c := deviceRepo.NewClient(this.Config.DeviceRepoUrl, nil)
	for len(temp) == limit {
		limit = steps
		temp, _, err, _ = c.ListDeviceTypesV3(permClient.InternalAdminToken, deviceRepo.DeviceTypeListOptions{
			Limit:       int64(limit),
			Offset:      int64(offset),
			ProtocolIds: []string{this.MosesProtocolId},
			SortBy:      "name.asc",
		})
		if err != nil {
			return result, err
		}
		for _, element := range temp {
			result = append(result, element.Id)
		}
		offset = offset + limit
	}
	return
}

func (this *StateRepo) GenerateExternalDevice(jwt jwt.Jwt, request CreateDeviceByTypeRequest) (device model.Device, err error) {
	deviceInp := model.Device{Name: request.Name, DeviceTypeId: request.DeviceTypeId, LocalId: uuid.NewString()}
	err = jwt.Impersonate.PostJSON(this.Config.DeviceManagerUrl+"/devices", deviceInp, &device)
	if err != nil {
		log.Println("ERROR: unable to create device in iot repository: ", err, device)
	}
	return
}

func (this *StateRepo) DeleteExternalDevice(jwt jwt.Jwt, id string) (err error) {
	if id != "" {
		_, err = jwt.Impersonate.Delete(this.Config.DeviceManagerUrl + "/devices/" + url.PathEscape(id))
	}
	return
}

func (this *StateRepo) GetProtocolList(handler string) (result []models.Protocol, err error) {
	token, err := this.Connector.Security().Access()
	if err != nil {
		debug.PrintStack()
		return result, err
	}
	result, err, _ = deviceRepo.NewClient(this.Config.DeviceRepoUrl, nil).ListProtocols(string(token), 1000, 0, "name.asc")
	return result, err
}

func (this *StateRepo) EnsureProtocol(handler string, segments []model.ProtocolSegment) (protocolId string, err error) {
	protocols, err := this.GetProtocolList(handler)
	if err != nil {
		debug.PrintStack()
		return protocolId, err
	}
	if len(protocols) == 1 {
		return protocols[0].Id, err
	}
	if len(protocols) > 1 {
		log.Println("WARNING: found multiple existing moses protocols")
		return protocols[0].Id, err
	}
	protocol, err := this.CreateProtocol(handler, segments)
	if err != nil {
		return protocolId, err
	}
	protocolId = protocol.Id
	return protocolId, err
}

func (this *StateRepo) CreateProtocol(handler string, segments []model.ProtocolSegment) (protocol model.Protocol, err error) {
	token, err := this.Connector.Security().Access()
	if err != nil {
		return protocol, err
	}
	err = token.PostJSON(this.Config.DeviceManagerUrl+"/protocols", model.Protocol{
		Name:             handler,
		Handler:          handler,
		ProtocolSegments: segments,
	}, &protocol)
	if err != nil {
		log.Println("ERROR:", err)
		log.Println("DEBUG: token=", token)
		debug.PrintStack()
	}
	return
}
