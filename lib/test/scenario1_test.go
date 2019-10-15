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

package test

import (
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"log"
	"testing"
	"time"
)

func TestScenario1(t *testing.T) {
	defaultConfig, err := config.LoadConfigLocation("../../config.json")
	if err != nil {
		t.Fatal(err)
	}

	log.Println("startup")
	config, stop, err := server.New(defaultConfig, "./server/keycloak-export.json")
	defer stop()
	if err != nil {
		t.Fatal(err)
	}

	log.Println("wait")
	time.Sleep(5 * time.Second)

	protocol := model.Protocol{}
	deviceType := model.DeviceType{}
	worldId, roomId := "", ""

	t.Run("get protocol", func(t *testing.T) {
		protocol = getTestMosesProtocol(t, config)
	})

	t.Run("create device-type", func(t *testing.T) {
		deviceType = createTestDeviceType(t, config, protocol)
	})

	t.Run("create world and room", func(t *testing.T) {
		worldId, roomId = createTesWorldAndRoom(t, config)
	})

	t.Run("create moses device", func(t *testing.T) {
		createMosesDevice(t, config, worldId, roomId, deviceType)
	})
}

func createTestDeviceType(t *testing.T, config config.Config, protocol model.Protocol) (result model.DeviceType) {
	err := helper.AdminJwt.PostJSON(config.DeviceManagerUrl+"/device-types", model.DeviceType{
		Name: "foo",
		Services: []model.Service{
			{
				Name:        "sepl_get",
				LocalId:     "sepl_get",
				Description: "sepl_get",
				ProtocolId:  protocol.Id,
				Outputs: []model.Content{
					{
						ProtocolSegmentId: protocol.ProtocolSegments[0].Id,
						Serialization:     "json",
						ContentVariable: model.ContentVariable{
							Name: "metrics",
							Type: model.Structure,
							SubContentVariables: []model.ContentVariable{
								{
									Name: "updateTime",
									Type: model.Integer,
								},
								{
									Name: "level",
									Type: model.Integer,
								},
								{
									Name: "title",
									Type: model.String,
								},
							},
						},
					},
				},
			},
			{
				Name:        "exact",
				LocalId:     "exact",
				Description: "exact",
				ProtocolId:  protocol.Id,
				Inputs: []model.Content{
					{
						ProtocolSegmentId: protocol.ProtocolSegments[0].Id,
						Serialization:     "json",
						ContentVariable: model.ContentVariable{
							Name: "metrics",
							Type: model.Structure,
							SubContentVariables: []model.ContentVariable{
								{
									Name: "level",
									Type: model.Integer,
								},
							},
						},
					},
				},
			},
		},
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Id == "" {
		t.Fatal("unexpected result", result)
	}
	return
}

func createTesWorldAndRoom(t *testing.T, config config.Config) (worldId string, roomId string) {
	t.Skip()
	return worldId, roomId
}

func createMosesDevice(t *testing.T, config config.Config, worldId string, roomId string, deviceType model.DeviceType) {
	t.Skip()
	return
}
