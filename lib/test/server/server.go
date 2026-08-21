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
	"log"
	"net"
	"runtime/debug"
	"strconv"
	"sync"

	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/SENERGY-Platform/moses/lib"
	"github.com/SENERGY-Platform/moses/lib/config"
)

func New(ctx context.Context, wg *sync.WaitGroup, startConfig config.Config, keyxcloakExportLocation string) (config config.Config, err error) {
	config = startConfig

	// the config used by the moses process under test lives on the host and
	// must use localhost + mapped ports; the containers talk to each other
	// via container ips / the docker gateway ip (not reachable from the host
	// on Docker Desktop/WSL2)
	hostKafkaUrl, containerKafkaUrl, err := Kafka(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.KafkaUrl = hostKafkaUrl

	mongoHostPort, mongoIp, err := MongoDB(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.MongoUrl = sb_config_types.Secret("mongodb://localhost:" + mongoHostPort)
	containerMongoUrl := "mongodb://" + mongoIp + ":27017"

	permV2HostPort, permV2Ip, err := PermissionsV2(ctx, wg, containerMongoUrl, containerKafkaUrl)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.PermissionsV2Url = "http://localhost:" + permV2HostPort
	containerPermV2Url := "http://" + permV2Ip + ":8080"

	repoHostPort, _, err := DeviceRepo(ctx, wg, containerKafkaUrl, containerMongoUrl, containerPermV2Url)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.DeviceRepoUrl = "http://localhost:" + repoHostPort
	config.DeviceManagerUrl = config.DeviceRepoUrl

	memcachedHostPort, _, err := Memcached(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.IotCacheUrls = "localhost:" + memcachedHostPort
	config.TokenCacheUrls = config.IotCacheUrls

	config.AuthEndpoint, err = Keycloak(ctx, wg)
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.AuthClientSecret = sb_config_types.Secret("d61daec4-40d6-4d3e-98c9-f3b515696fc6")
	config.AuthClientId = "connector"

	apiPort, err := getFreePort()
	if err != nil {
		log.Println("ERROR:", err)
		debug.PrintStack()
		return config, err
	}
	config.ServerPort = strconv.Itoa(apiPort)

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
