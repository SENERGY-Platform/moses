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
	"github.com/ory/dockertest"
	"log"
	"net/http"
)

func Elasticsearch(pool *dockertest.Pool) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start elasticsearch")
	repo, err := pool.Run("docker.elastic.co/elasticsearch/elasticsearch", "7.6.1", []string{"discovery.type=single-node"})
	if err != nil {
		return func() {}, "", "", err
	}
	hostPort = repo.GetPort("9200/tcp")
	err = pool.Retry(func() error {
		log.Println("try elastic connection...")
		_, err := http.Get("http://" + repo.Container.NetworkSettings.IPAddress + ":9200/_cluster/health")
		if err != nil {
			log.Println(err)
		}
		return err
	})
	return func() { repo.Close() }, hostPort, repo.Container.NetworkSettings.IPAddress, err
}

func PermSearch(pool *dockertest.Pool, zk string, elasticIp string) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start permsearch")
	repo, err := pool.Run("ghcr.io/senergy-platform/permission-search", "dev", []string{
		"KAFKA_URL=" + zk,
		"ELASTIC_URL=" + "http://" + elasticIp + ":9200",
	})
	if err != nil {
		return func() {}, "", "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	go Dockerlog(pool, ctx, repo, "PERMISSION-SEARCH")
	hostPort = repo.GetPort("8080/tcp")
	err = pool.Retry(func() error {
		log.Println("try permsearch connection...")
		_, err := http.Get("http://" + repo.Container.NetworkSettings.IPAddress + ":8080/jwt/check/deviceinstance/foo/r/bool")
		if err != nil {
			log.Println(err)
		}
		return err
	})
	if err != nil {
		cancel()
		return func() { repo.Close() }, hostPort, repo.Container.NetworkSettings.IPAddress, err
	} else {
		return func() {
			repo.Close()
			cancel()
		}, hostPort, repo.Container.NetworkSettings.IPAddress, err
	}
}
