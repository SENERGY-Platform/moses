/*
 * Copyright 2018 InfAI (CC SES)
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

package main

import (
	"github.com/ory/dockertest"
	"github.com/ory/dockertest/docker"
	"log"
	"moses/connector"
	"moses/marshaller"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type PersistenceMock struct{}

func (this PersistenceMock) PersistTemplate(templ RoutineTemplate) error {
	return nil
}

func (this PersistenceMock) GetTemplate(id string) (templ RoutineTemplate, err error) {
	return templ, nil
}

func (this PersistenceMock) GetTemplates() (templ []RoutineTemplate, err error) {
	return templ, nil
}

func (this PersistenceMock) DeleteWorld(id string) error {
	return nil
}

func (this PersistenceMock) DeleteGraph(id string) error {
	return nil
}

func (this PersistenceMock) DeleteTemplate(id string) error {
	return nil
}

func (this PersistenceMock) PersistWorld(world World) (err error) {
	return
}

func (this PersistenceMock) PersistGraph(graph Graph) (err error) {
	return
}

func (this PersistenceMock) LoadWorlds() (result map[string]*World, err error) {
	return
}

func (this PersistenceMock) LoadGraphs() (result map[string]*Graph, err error) {
	return
}

type ProtocolMock struct{}

var test_send_values = []SendMock{}
var test_receiver func(deviceId string, serviceId string, cmdMsg interface{}, responder func(respMsg interface{}))

type SendMock struct {
	Device  string
	Service string
	Value   interface{}
}

func (this *ProtocolMock) Send(deviceId string, serviceId string, marshaller marshaller.Marshaller, value interface{}) (err error) {
	test_send_values = append(test_send_values, SendMock{Device: deviceId, Service: serviceId, Value: value})
	return
}

func (this *ProtocolMock) SetReceiver(receiver func(deviceId string, serviceId string, cmdMsg interface{}, responder func(respMsg interface{}))) {
	test_receiver = receiver
}

func (this *ProtocolMock) Start() (err error) {
	return
}

var mockserver *httptest.Server
var integratedServer *httptest.Server

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

var integratedConfig Config
var integratedstaterepo StateRepo

func TestMain(m *testing.M) {
	//with mocks
	staterepo := &StateRepo{Persistence: PersistenceMock{}, Config: Config{JsTimeout: 2 * time.Second}, Protocol: &ProtocolMock{}}
	err := staterepo.Load()
	if err != nil {
		log.Fatal("unable to load state repo: ", err)
	}
	log.Println("start state routines")
	staterepo.Start()
	routes := getRoutes(Config{DevApi: "true"}, staterepo)
	logger := Logger(routes, "CALL")
	mockserver = httptest.NewServer(logger)
	defer mockserver.Close()

	//indication tests
	log.Println("start integrated system for tests")

	integratedConfig, err = LoadConfig()
	if err != nil {
		log.Fatal("unable to load integratedConfig: ", err)
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}

	log.Println("start mongodb")
	mongo, err := pool.Run("mongo", "latest", []string{})
	if err != nil {
		log.Fatalf("Could not start dockerrabbitmq: %s", err)
	}
	defer mongo.Close()
	integratedConfig.MongoUrl = "mongodb://localhost:" + mongo.GetPort("27017/tcp")

	kafkaport, err := GetFreePort()
	if err != nil {
		log.Fatalf("Could not find new port: %s", err)
	}
	zkport, err := GetFreePort()
	if err != nil {
		log.Fatalf("Could not find new port: %s", err)
	}
	log.Println("start kafka")
	dockerkafka, err := pool.RunWithOptions(&dockertest.RunOptions{Repository: "spotify/kafka", Tag: "latest", Env: []string{
		"ADVERTISED_PORT=" + strconv.Itoa(kafkaport),
		"ADVERTISED_HOST=localhost",
	}, PortBindings: map[docker.Port][]docker.PortBinding{
		"9092/tcp": {{HostIP: "", HostPort: strconv.Itoa(kafkaport)}},
		"2181/tcp": {{HostIP: "", HostPort: strconv.Itoa(zkport)}},
	}})
	defer dockerkafka.Close()
	integratedConfig.ZookeeperUrl = "localhost:" + strconv.Itoa(zkport)

	log.Println("start rabbitmq")
	dockerrabbitmq, err := pool.Run("rabbitmq", "3-management", []string{})
	if err != nil {
		log.Fatalf("Could not start dockerrabbitmq: %s", err)
	}
	defer dockerrabbitmq.Close()

	log.Println("start elasticsearch")
	dockerelastic, err := pool.Run("elasticsearch", "latest", []string{})
	if err != nil {
		log.Fatalf("Could not start dockerelastic: %s", err)
	}
	defer dockerelastic.Close()

	log.Println("start iot-ontology")
	dockerrdf, err := pool.Run("fgseitsrancher.wifa.intern.uni-leipzig.de:5000/iot-ontology", "unstable", []string{
		"DBA_PASSWORD=myDbaPassword",
		"DEFAULT_GRAPH=iot",
	})
	if err != nil {
		log.Fatalf("Could not start rdf: %s", err)
	}
	defer dockerrdf.Close()

	time.Sleep(5 * time.Second)

	log.Println("start permsearch")
	dockersearch, err := pool.Run("fgseitsrancher.wifa.intern.uni-leipzig.de:5000/permissionsearch", "unstable", []string{
		"AMQP_URL=" + "amqp://guest:guest@" + dockerrabbitmq.Container.NetworkSettings.IPAddress + ":5672/",
		"ELASTIC_URL=" + "http://" + dockerelastic.Container.NetworkSettings.IPAddress + ":9200",
	})
	defer dockersearch.Close()
	/*
		dockervtsearch, err := pool.Run("fgseitsrancher.wifa.intern.uni-leipzig.de:5000/valuetypesearch", "unstable", []string{
			"AMQP_URL=" + "amqp://guest:guest@" + dockerrabbitmq.Container.NetworkSettings.IPAddress + ":5672/",
			"ELASTIC_URL=" + "http://" + dockerelastic.Container.NetworkSettings.IPAddress + ":9200",
		})
		if err != nil {
			log.Fatalf("Could not start search: %s", err)
		}
		defer dockervtsearch.Close()
	*/
	time.Sleep(5 * time.Second)

	log.Println("start iot-repo")
	dockeriot, err := pool.Run("fgseitsrancher.wifa.intern.uni-leipzig.de:5000/iot-device-repository", "unstable", []string{
		"SPARQL_ENDPOINT=" + "http://" + dockerrdf.Container.NetworkSettings.IPAddress + ":8890/sparql",
		"AMQP_URL=" + "amqp://guest:guest@" + dockerrabbitmq.Container.NetworkSettings.IPAddress + ":5672/",
		"PERMISSIONS_URL=" + "http://" + dockersearch.Container.NetworkSettings.IPAddress + ":8080",
	})
	if err != nil {
		log.Fatalf("Could not start iot repo: %s", err)
	}
	defer dockeriot.Close()
	integratedConfig.IotUrl = "http://localhost:" + dockeriot.GetPort("8080/tcp")

	time.Sleep(5 * time.Second)

	log.Println("init protocol handler")
	protocol, err := connector.NewMosesProtocolConnector(connector.Config{
		ZookeeperUrl:       integratedConfig.ZookeeperUrl,
		KafkaEventTopic:    integratedConfig.KafkaEventTopic,
		ProtocolTopic:      integratedConfig.KafkaProtocolTopic,
		KafkaResponseTopic: integratedConfig.KafkaResponseTopic,
	})
	if err != nil {
		log.Fatal("unable to initialize protocol: ", err)
	}
	log.Println("connect to database")
	persistence, err := NewMongoPersistence(integratedConfig)
	if err != nil {
		log.Fatal("unable to connect to database: ", err)
	}

	integratedstaterepo = StateRepo{Persistence: persistence, Config: integratedConfig, Protocol: protocol}
	err = integratedstaterepo.Load()
	if err != nil {
		log.Fatal("unable to load state repo: ", err)
	}
	log.Println("start integrated state routines")
	staterepo.Start()
	integratedroutes := getRoutes(Config{DevApi: "true"}, &integratedstaterepo)
	integratedlogger := Logger(integratedroutes, "CALL")
	integratedServer = httptest.NewServer(integratedlogger)
	defer integratedServer.Close()

	//run
	m.Run()
}

func TestStartup(t *testing.T) {

}
