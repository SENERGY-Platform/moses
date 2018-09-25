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

package main

import "log"

func main() {
	log.Println("load config")
	config, err := LoadConfig()
	if err != nil {
		log.Fatal("unable to load config: ", err)
	}

	log.Println("connect to database")
	persistence, err := NewMongoPersistence(config)
	if err != nil {
		log.Fatal("unable to connect to database: ", err)
	}

	log.Println("load states")
	staterepo := &StateRepo{Persistence: persistence, Config: config}
	err = staterepo.Load()
	if err != nil {
		log.Fatal("unable to load state repo: ", err)
	}

	log.Println("start state routines")
	err = staterepo.Start()
	if err != nil {
		log.Fatal("unable to start state repo: ", err)
	}

	log.Println("start api on port: ", config.ServerPort)
	StartApi(config, staterepo)

}
