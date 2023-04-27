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
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"log"
	"sync"
)

func DeviceRepo(ctx context.Context, wg *sync.WaitGroup, kafkaUrl string, mongoUrl string, permsearch string) (hostPort string, ipAddress string, err error) {
	log.Println("start device-repository")
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "ghcr.io/senergy-platform/device-repository:dev",
			Env: map[string]string{
				"KAFKA_URL":       kafkaUrl,
				"PERMISSIONS_URL": permsearch,
				"MONGO_URL":       mongoUrl,
			},
			ExposedPorts:    []string{"8080/tcp"},
			WaitingFor:      wait.ForListeningPort("8080/tcp"),
			AlwaysPullImage: true,
		},
		Started: true,
	})
	if err != nil {
		return "", "", err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		log.Println("DEBUG: remove container device-repository", c.Terminate(context.Background()))
	}()

	ipAddress, err = c.ContainerIP(ctx)
	if err != nil {
		return "", "", err
	}
	temp, err := c.MappedPort(ctx, "8080/tcp")
	if err != nil {
		return "", "", err
	}
	hostPort = temp.Port()

	return hostPort, ipAddress, err
}
