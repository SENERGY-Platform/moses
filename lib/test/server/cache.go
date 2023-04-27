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
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"log"
	"sync"
)

func Memcached(ctx context.Context, wg *sync.WaitGroup) (hostPort string, ipAddress string, err error) {
	log.Println("start memcached")
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:           "memcached:1.5.12-alpine",
			Tmpfs:           map[string]string{},
			WaitingFor:      wait.ForListeningPort("11211/tcp"),
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
		log.Println("DEBUG: remove container memcached", c.Terminate(context.Background()))
	}()

	ipAddress, err = c.ContainerIP(ctx)
	if err != nil {
		return "", "", err
	}
	temp, err := c.MappedPort(ctx, "11211/tcp")
	if err != nil {
		return "", "", err
	}
	hostPort = temp.Port()

	return hostPort, ipAddress, err
}
