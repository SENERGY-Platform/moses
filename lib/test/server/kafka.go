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
	"fmt"
	"log"
	"math"
	"net"
	"strconv"
	"sync"

	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
	kafka_test "github.com/testcontainers/testcontainers-go/modules/kafka"
	"github.com/testcontainers/testcontainers-go/wait"
)

func Kafka(ctx context.Context, wg *sync.WaitGroup) (kafkaUrl string, err error) {
	kafkaport, err := GetFreePort()
	if err != nil {
		return kafkaUrl, err
	}
	provider, err := testcontainers.NewDockerProvider(testcontainers.DefaultNetwork("bridge"))
	if err != nil {
		return kafkaUrl, err
	}
	hostIp, err := provider.GetGatewayIP(ctx)
	if err != nil {
		return kafkaUrl, err
	}
	kafkaUrl = hostIp + ":" + strconv.Itoa(kafkaport)
	log.Println("host ip: ", hostIp)
	log.Println("host port: ", kafkaport)
	log.Println("kafkaUrl url: ", kafkaUrl)

	c, err := kafka_test.Run(ctx, "confluentinc/confluent-local:7.5.0",
		testcontainers.WithExposedPorts(strconv.Itoa(kafkaport)+":9093/tcp"),
		testcontainers.WithEnv(
			map[string]string{
				"KAFKA_LISTENERS":                                "PLAINTEXT://0.0.0.0:9093,BROKER://0.0.0.0:9092,CONTROLLER://0.0.0.0:9094",
				"KAFKA_REST_BOOTSTRAP_SERVERS":                   "PLAINTEXT://0.0.0.0:9093,BROKER://0.0.0.0:9092,CONTROLLER://0.0.0.0:9094",
				"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP":           "BROKER:PLAINTEXT,PLAINTEXT:PLAINTEXT,CONTROLLER:PLAINTEXT",
				"KAFKA_INTER_BROKER_LISTENER_NAME":               "BROKER",
				"KAFKA_BROKER_ID":                                "1",
				"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":         "1",
				"KAFKA_OFFSETS_TOPIC_NUM_PARTITIONS":             "1",
				"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
				"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":            "1",
				"KAFKA_LOG_FLUSH_INTERVAL_MESSAGES":              strconv.Itoa(math.MaxInt),
				"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS":         "0",
				"KAFKA_NODE_ID":                                  "1",
				"KAFKA_PROCESS_ROLES":                            "broker,controller",
				"KAFKA_CONTROLLER_LISTENER_NAMES":                "CONTROLLER",
			},
		),
		testcontainers.WithLifecycleHooks(testcontainers.ContainerLifecycleHooks{
			PostStarts: []testcontainers.ContainerHook{
				func(ctx context.Context, c testcontainers.Container) error {
					if err := copyStarterScript(ctx, c, kafkaUrl); err != nil {
						log.Println("ERROR: copy starter script: ", err)
						return fmt.Errorf("copy starter script: %w", err)
					}

					err = wait.ForLog(".*Transitioning from RECOVERY to RUNNING.*").AsRegexp().WaitUntilReady(ctx, c)
					if err != nil {
						log.Println("ERROR: wait for log: ", err)
						return err
					}
					return nil
				},
			},
		}),
	)
	if err != nil {
		return kafkaUrl, err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		log.Println("DEBUG: remove container kafka", c.Terminate(context.Background()))
	}()

	return kafkaUrl, err
}

func GetFreePort() (int, error) {
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

func copyStarterScript(ctx context.Context, c testcontainers.Container, kafkaUrl string) error {
	const publicPort = nat.Port("9093/tcp")
	const starterScript = "/usr/sbin/testcontainers_start.sh"
	// starterScript {
	const starterScriptContent = `#!/bin/bash
source /etc/confluent/docker/bash-config
export KAFKA_ADVERTISED_LISTENERS=%s,BROKER://%s:9092
echo Starting Kafka KRaft mode
sed -i '/KAFKA_ZOOKEEPER_CONNECT/d' /etc/confluent/docker/configure
echo 'kafka-storage format --ignore-formatted -t "$(kafka-storage random-uuid)" -c /etc/kafka/kafka.properties' >> /etc/confluent/docker/configure
echo '' > /etc/confluent/docker/ensure
/etc/confluent/docker/configure
/etc/confluent/docker/launch`

	if err := wait.ForMappedPort(publicPort).
		WaitUntilReady(ctx, c); err != nil {
		return fmt.Errorf("wait for mapped port: %w", err)
	}

	inspect, err := c.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}

	hostname := inspect.Config.Hostname

	scriptContent := fmt.Sprintf(starterScriptContent, "PLAINTEXT://"+kafkaUrl, hostname)

	if err := c.CopyToContainer(ctx, []byte(scriptContent), starterScript, 0o755); err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	return nil
}
