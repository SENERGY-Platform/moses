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

package server

import (
	"context"
	"github.com/SENERGY-Platform/moses/lib"
	"github.com/SENERGY-Platform/moses/lib/config"
	"log"
	"net"
	"runtime/debug"
	"sync"
)

func New(ctx context.Context, wg *sync.WaitGroup, startConfig config.Config, keyxcloakExportLocation string) (config config.Config, err error) {
	config = startConfig

	_, zkIp, err := Zookeeper(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	zookeeperUrl := zkIp + ":2181"

	config.KafkaUrl, err = Kafka(ctx, wg, zookeeperUrl)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}

	_, mongoIp, err := MongoDB(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.MongoUrl = "mongodb://" + mongoIp + ":27017"

	_, permV2Ip, err := PermissionsV2(ctx, wg, config.MongoUrl, config.KafkaUrl)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.PermissionsV2Url = "http://" + permV2Ip + ":8080"

	_, ip, err := DeviceRepo(ctx, wg, config.KafkaUrl, config.MongoUrl, config.PermissionsV2Url)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.DeviceRepoUrl = "http://" + ip + ":8080"

	_, ip, err = DeviceManager(ctx, wg, config.KafkaUrl, config.DeviceRepoUrl, config.PermissionsV2Url)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.DeviceManagerUrl = "http://" + ip + ":8080"

	_, ip, err = Memcached(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.IotCacheUrls = ip + ":11211"
	config.TokenCacheUrls = ip + ":11211"

	config.AuthEndpoint, err = Keycloak(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.AuthClientSecret = "d61daec4-40d6-4d3e-98c9-f3b515696fc6"
	config.AuthClientId = "connector"

	err = lib.New(config, ctx)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}

	return config, nil
}

func getFreePort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
