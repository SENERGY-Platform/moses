/*
 * Copyright 2018 SENERGY Team
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

package moses

import (
	"log"
	"net/http/httptest"
	"os"
	"testing"
)

type PersistenceMock struct{}

func (this PersistenceMock) PersistWorld(world World) (err error) {
	return
}

func (this PersistenceMock) PersistGraph(graph Graph) (err error) {
	return
}

func (this PersistenceMock) LoadWorlds() (result map[string]World, err error) {
	return
}

func (this PersistenceMock) LoadGraphs() (result map[string]Graph, err error) {
	return
}

var testserver *httptest.Server

func TestMain(m *testing.M) {
	persistencemock := PersistenceMock{}
	staterepo := &StateRepo{Persistence: persistencemock, Config: Config{}}
	err := staterepo.Load()
	if err != nil {
		log.Fatal("unable to load state repo: ", err)
	}
	log.Println("start state routines")
	err = staterepo.Start()
	if err != nil {
		log.Fatal("unable to start state repo: ", err)
	}
	routes := getRoutes(Config{}, staterepo)
	testserver = httptest.NewServer(routes)
	defer testserver.Close()
	os.Exit(m.Run())
}

func TestStartup(t *testing.T) {

}
