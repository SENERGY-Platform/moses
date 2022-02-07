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
	"github.com/ory/dockertest"
	"log"
	"net"
	"strings"
	"sync"
)

func New(startConfig config.Config, keyxcloakExportLocation string) (config config.Config, shutdown func(), err error) {
	config = startConfig

	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Println("Could not connect to docker: ", err)
		return config, func() {}, err
	}

	closerList := []func(){}
	close := func(list []func()) {
		for i := len(list)/2 - 1; i >= 0; i-- {
			opp := len(list) - 1 - i
			list[i], list[opp] = list[opp], list[i]
		}
		for _, c := range list {
			if c != nil {
				c()
			}
		}
	}

	mux := sync.Mutex{}
	var globalError error
	wait := sync.WaitGroup{}

	//zookeeper
	zkWait := sync.WaitGroup{}
	zkWait.Add(1)
	wait.Add(1)
	zookeeperUrl := ""
	go func() {
		defer wait.Done()
		defer zkWait.Done()
		closer, _, zkIp, err := Zookeeper(pool)
		mux.Lock()
		defer mux.Unlock()
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
		zookeeperUrl = zkIp + ":2181"
	}()

	//kafka
	kafkaWait := sync.WaitGroup{}
	kafkaWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer kafkaWait.Done()
		zkWait.Wait()
		if globalError != nil {
			return
		}
		var closer func()
		config.KafkaUrl, closer, err = Kafka(pool, zookeeperUrl)
		mux.Lock()
		defer mux.Unlock()
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
	}()

	var elasticIp string
	var mongoIp string

	//kafka
	elasticWait := sync.WaitGroup{}
	elasticWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer elasticWait.Done()
		if globalError != nil {
			return
		}
		closer, _, ip, err := Elasticsearch(pool)
		elasticIp = ip
		mux.Lock()
		defer mux.Unlock()
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
	}()

	mongoWait := sync.WaitGroup{}
	mongoWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer mongoWait.Done()
		if globalError != nil {
			return
		}
		closer, _, ip, err := MongoTestServer(pool)
		mongoIp = ip
		config.MongoUrl = "mongodb://" + mongoIp + ":27017"
		mux.Lock()
		defer mux.Unlock()
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
	}()

	var permissionUrl string
	permWait := sync.WaitGroup{}
	permWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer permWait.Done()
		kafkaWait.Wait()
		elasticWait.Wait()
		if globalError != nil {
			return
		}
		closer, _, permIp, err := PermSearch(pool, config.KafkaUrl, elasticIp)
		mux.Lock()
		defer mux.Unlock()
		permissionUrl = "http://" + permIp + ":8080"
		config.PermQueryUrl = permissionUrl
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
	}()

	//device-repo
	deviceRepoWait := sync.WaitGroup{}
	deviceRepoWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer deviceRepoWait.Done()
		mongoWait.Wait()
		zkWait.Wait()
		kafkaWait.Wait()
		permWait.Wait()
		if globalError != nil {
			return
		}
		closer, _, ip, err := DeviceRepo(pool, mongoIp, config.KafkaUrl, permissionUrl)
		mux.Lock()
		defer mux.Unlock()
		config.DeviceRepoUrl = "http://" + ip + ":8080"
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
	}()

	//device-manager
	deviceManagerWait := sync.WaitGroup{}
	deviceManagerWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer deviceManagerWait.Done()
		deviceRepoWait.Wait()
		mongoWait.Wait()
		if globalError != nil {
			return
		}
		closer, _, ip, err := DeviceManager(pool, config.KafkaUrl, config.DeviceRepoUrl, "-", permissionUrl)
		mux.Lock()
		defer mux.Unlock()
		config.DeviceManagerUrl = "http://" + ip + ":8080"
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
	}()

	//memcached
	cacheWait := sync.WaitGroup{}
	cacheWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer cacheWait.Done()
		if globalError != nil {
			return
		}
		closer, _, ip, err := Memcached(pool)
		mux.Lock()
		defer mux.Unlock()
		config.IotCacheUrls = ip + ":11211"
		config.TokenCacheUrls = ip + ":11211"
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
	}()

	keycloakWait := sync.WaitGroup{}
	keycloakWait.Add(1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer keycloakWait.Done()
		if globalError != nil {
			return
		}
		closer, _, ip, err := Keycloak(pool)
		mux.Lock()
		defer mux.Unlock()
		config.AuthEndpoint = "http://" + ip + ":8080"
		config.AuthClientSecret = "d61daec4-40d6-4d3e-98c9-f3b515696fc6"
		config.AuthClientId = "connector"
		closerList = append(closerList, closer)
		if err != nil {
			globalError = err
			return
		}
		err = ConfigKeycloak(config.AuthEndpoint, keyxcloakExportLocation, "connector")
		if err != nil {
			log.Println("ConfigKeycloak: ", err)
			globalError = err
			return
		}
	}()

	wait.Wait()
	if globalError != nil {
		close(closerList)
		return config, shutdown, globalError
	}

	ctx, cancel := context.WithCancel(context.Background())
	err = lib.New(config, ctx)
	if err != nil {
		log.Println(err)
		close(closerList)
		cancel()
		return config, func() { close(closerList) }, err
	}

	closerList = append(closerList, func() {
		cancel()
	})

	return config, func() { close(closerList) }, nil
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

func StringToList(str string) []string {
	temp := strings.Split(str, ",")
	result := []string{}
	for _, e := range temp {
		trimmed := strings.TrimSpace(e)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
