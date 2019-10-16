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
	"encoding/json"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"github.com/SENERGY-Platform/platform-connector-lib/kafka"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	uuid "github.com/satori/go.uuid"
	"log"
	"sync"
	"testing"
	"time"
)

func TestCommand(t *testing.T) {
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

	log.Println("wait for protocol creation")
	time.Sleep(5 * time.Second)

	protocol := model.Protocol{}
	deviceType := model.DeviceType{}
	worldId, roomId := "", ""
	device := state.DeviceMsg{}

	t.Run("get protocol", func(t *testing.T) {
		protocol = getTestMosesProtocol(t, config)
	})

	t.Run("create device-type", func(t *testing.T) {
		deviceType = createTestDeviceType(t, config, protocol)
	})

	log.Println("wait for device-type creation")
	time.Sleep(5 * time.Second)

	t.Run("create world and room", func(t *testing.T) {
		worldId, roomId = createTestWorldAndRoom(t, config)
	})

	t.Run("create moses device", func(t *testing.T) {
		device = createMosesDevice(t, config, worldId, roomId, deviceType)
		log.Println("wait for device creation")
		time.Sleep(5 * time.Second)
		checkDevice(t, config, device)
	})

	t.Run("try command", func(t *testing.T) {
		tryCommandToDevice(t, config, protocol, deviceType, device)
	})

}

func tryCommandToDevice(t *testing.T, config config.Config, protocol model.Protocol, deviceType model.DeviceType, deviceMsg state.DeviceMsg) {
	service := model.Service{}
	for _, s := range deviceType.Services {
		if s.LocalId == "sepl_get" {
			service = s
			break
		}
	}
	err := kafka.InitTopic(config.ZookeeperUrl, model.ServiceIdToTopic(service.Id))
	if err != nil {
		t.Fatal(err)
	}
	mux := sync.Mutex{}
	responses := []model.ProtocolMsg{}
	consumer, err := kafka.NewConsumer(config.ZookeeperUrl, "testing_"+uuid.NewV4().String(), config.KafkaResponseTopic, func(topic string, msg []byte, time time.Time) error {
		mux.Lock()
		defer mux.Unlock()
		resp := model.ProtocolMsg{}
		err := json.Unmarshal(msg, &resp)
		if err != nil {
			t.Fatal(err)
			return err
		}
		responses = append(responses, resp)
		return nil
	}, func(err error, consumer *kafka.Consumer) {
		t.Fatal(err)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Stop()

	producer, err := kafka.PrepareProducer(config.ZookeeperUrl, config.SyncKafka, config.SyncKafkaIdempotent)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(model.ProtocolMsg{
		Request:  model.ProtocolRequest{},
		Response: model.ProtocolResponse{},
		TaskInfo: model.TaskInfo{},
		Metadata: model.Metadata{
			Device: model.Device{
				Id:           deviceMsg.ExternalRef,
				LocalId:      deviceMsg.Id,
				DeviceTypeId: deviceType.Id,
			},
			Service:  service,
			Protocol: protocol,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = producer.Produce(config.Protocol, string(request))
	if err != nil {
		t.Fatal(err)
	}
	log.Println("wait for command handling")
	time.Sleep(5 * time.Second)

	mux.Lock()
	defer mux.Unlock()
	if len(responses) != 1 {
		t.Fatal("unexpected response count", responses)
	}
	if responses[0].Response.Output[config.ProtocolSegmentName] != "{\"level\":0,\"title\":\"\",\"updateTime\":0}" {
		t.Fatal("unexpected response msg", responses[0].Response.Output)
	}
}
