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

import (
	"bytes"
	"log"
	"net/http"
)

func Logger(handler http.Handler, logLevel string) *LoggerMiddleWare {
	return &LoggerMiddleWare{handler: handler, logLevel: logLevel}
}

type LoggerMiddleWare struct {
	handler  http.Handler
	logLevel string //DEBUG | CALL | NONE
}

func (this *LoggerMiddleWare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	this.log(r)
	if this.handler != nil {
		this.handler.ServeHTTP(w, r)
	} else {
		http.Error(w, "Forbidden", 403)
	}
}

func (this *LoggerMiddleWare) log(request *http.Request) {
	if this.logLevel != "NONE" {
		method := request.Method
		path := request.URL

		if this.logLevel == "CALL" {
			log.Printf("[%v] %v \n", method, path)
		}

		if this.logLevel == "DEBUG" {
			buf := new(bytes.Buffer)
			buf.ReadFrom(request.Body)
			body := buf.String()

			client := request.RemoteAddr

			log.Printf("%v [%v] %v\n%v\n", client, method, path, body)
		}

	}
}
