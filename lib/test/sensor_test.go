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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"github.com/SENERGY-Platform/platform-connector-lib/kafka"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	uuid "github.com/satori/go.uuid"
	"io/ioutil"
	"log"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestSensor(t *testing.T) {
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

	t.Run("try sensor", func(t *testing.T) {
		trySensorFromDevice(t, config, protocol, deviceType, device)
	})

}

func trySensorFromDevice(t *testing.T, config config.Config, protocol model.Protocol, deviceType model.DeviceType, device state.DeviceMsg) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Run("set sensor time", func(t *testing.T) {
		setSensorTime(t, config, device, 1)
	})
	service := model.Service{}
	for _, s := range deviceType.Services {
		if s.LocalId == "sepl_get" {
			service = s
			break
		}
	}
	mux := sync.Mutex{}
	events := []model.Envelope{}

	err := kafka.NewConsumer(ctx, kafka.ConsumerConfig{
		KafkaUrl:       config.KafkaUrl,
		GroupId:        "testing_" + uuid.NewV4().String(),
		Topic:          model.ServiceIdToTopic(service.Id),
		MinBytes:       int(config.KafkaConsumerMinBytes),
		MaxBytes:       int(config.KafkaConsumerMaxBytes),
		MaxWait:        100 * time.Millisecond,
		TopicConfigMap: config.KafkaTopicConfigs,
	}, func(topic string, msg []byte, time time.Time) error {
		mux.Lock()
		defer mux.Unlock()
		resp := model.Envelope{}
		err := json.Unmarshal(msg, &resp)
		if err != nil {
			t.Fatal(err)
			return err
		}
		events = append(events, resp)
		return nil
	}, func(err error) {
		t.Fatal(err)
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Second)

	mux.Lock()
	defer mux.Unlock()
	if len(events) == 0 {
		t.Fatal("unexpected event count", events)
	}
	event := events[0]
	if event.DeviceId != device.ExternalRef {
		t.Fatal("unexpected envelope", event)
	}
	if event.ServiceId != service.Id {
		t.Fatal("unexpected envelope", event)
	}

	var expected interface{}
	err = json.Unmarshal([]byte("{\"metrics\":{\"level\":0,\"title\":\"\",\"updateTime\":0}}"), &expected)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(event.Value, expected) {
		t.Fatal(event.Value, "\n\n!=\n\n", expected)
	}
}

func setSensorTime(t *testing.T, config config.Config, deviceMsg state.DeviceMsg, seconds int64) {
	//PUT /service UpdateServiceRequest
	service := state.Service{}
	for _, s := range deviceMsg.Services {
		if s.Name == "sepl_get" {
			service = s
			break
		}
	}

	client := http.Client{
		Timeout: 5 * time.Second,
	}
	b := new(bytes.Buffer)
	err := json.NewEncoder(b).Encode(state.UpdateServiceRequest{
		Id:             service.Id,
		Name:           service.Name,
		ExternalRef:    service.ExternalRef,
		Code:           service.Code,
		SensorInterval: seconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("PUT", "http://localhost:"+config.ServerPort+"/service", b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", string(helper.AdminJwt))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(responseBody))
		t.Fatal(err)
	}
}
