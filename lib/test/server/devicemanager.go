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
	"github.com/ory/dockertest"
	"log"
	"net/http"
)

func DeviceManager(pool *dockertest.Pool, zk string, deviceRepoUrl string, semanticRepoUrl string, permsearchUrl string) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start device repo")
	repo, err := pool.Run("ghcr.io/senergy-platform/device-manager", "dev", []string{
		"KAFKA_URL=" + zk,
		"PERMISSIONS_URL=" + permsearchUrl,
		"DEVICE_REPO_URL=" + deviceRepoUrl,
		"SEMANTIC_REPO_URL=" + semanticRepoUrl,
	})
	if err != nil {
		return func() {}, "", "", err
	}
	hostPort = repo.GetPort("8080/tcp")
	err = pool.Retry(func() error {
		log.Println("try manager connection...")
		_, err := http.Get("http://" + repo.Container.NetworkSettings.IPAddress + ":8080/")
		if err != nil {
			log.Println(err)
		}
		return err
	})
	return func() { repo.Close() }, hostPort, repo.Container.NetworkSettings.IPAddress, err
}
