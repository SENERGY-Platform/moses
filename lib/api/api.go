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

package api

import (
	"context"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/jwt"
	"github.com/SENERGY-Platform/moses/lib/state"
	"log"
	"net/http"
	"reflect"
	"runtime"
	"time"

	"github.com/julienschmidt/httprouter"
)

var endpoints = []func(config config.Config, states *state.StateRepo, router *httprouter.Router){}

func Start(ctx context.Context, config config.Config, staterepo *state.StateRepo) {
	router := httprouter.New()
	for _, e := range endpoints {
		log.Println("add endpoints: " + runtime.FuncForPC(reflect.ValueOf(e).Pointer()).Name())
		e(config, staterepo, router)
	}
	logger := Logger(router, config.LogLevel)
	server := &http.Server{Addr: ":" + config.ServerPort, Handler: logger, WriteTimeout: 10 * time.Second, ReadTimeout: 2 * time.Second, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		log.Println("Listening on ", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("ERROR: api server error", err)
			log.Fatal(err)
		}
	}()
	go func() {
		<-ctx.Done()
		log.Println("DEBUG: api shutdown", server.Shutdown(context.Background()))
	}()
}

func isAdmin(jwt jwt.Jwt) bool {
	for _, role := range jwt.RealmAccess.Roles {
		if role == "admin" {
			return true
		}
	}
	return false
}
