/*
 * Copyright 2020 InfAI (CC SES)
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
	"sync"
)

func DeviceManagerWithDependencies(basectx context.Context, wg *sync.WaitGroup) (managerUrl string, repoUrl string, permv2Url string, err error) {
	_, managerUrl, repoUrl, permv2Url, err = DeviceManagerWithDependenciesAndKafka(basectx, wg)
	return
}

func DeviceManagerWithDependenciesAndKafka(basectx context.Context, wg *sync.WaitGroup) (kafkaUrl string, managerUrl string, repoUrl string, permv2Url string, err error) {
	ctx, cancel := context.WithCancel(basectx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	kafkaUrl, err = Kafka(ctx, wg)
	if err != nil {
		return kafkaUrl, managerUrl, repoUrl, permv2Url, err
	}

	_, mongoIp, err := MongoDB(ctx, wg)
	if err != nil {
		return kafkaUrl, managerUrl, repoUrl, permv2Url, err
	}
	mongoUrl := "mongodb://" + mongoIp + ":27017"

	_, permV2Ip, err := PermissionsV2(ctx, wg, mongoUrl, kafkaUrl)
	if err != nil {
		return kafkaUrl, managerUrl, repoUrl, permv2Url, err
	}
	permv2Url = "http://" + permV2Ip + ":8080"

	_, repoIp, err := DeviceRepo(ctx, wg, kafkaUrl, mongoUrl, permv2Url)
	if err != nil {
		return kafkaUrl, managerUrl, repoUrl, permv2Url, err
	}
	repoUrl = "http://" + repoIp + ":8080"
	managerUrl = repoUrl

	return kafkaUrl, managerUrl, repoUrl, permv2Url, err
}
