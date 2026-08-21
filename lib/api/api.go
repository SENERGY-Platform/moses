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
	"net/http"
	"reflect"
	"runtime"
	"time"

	gin_mw "github.com/SENERGY-Platform/gin-middleware"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/gin-gonic/gin"
)

var endpoints = []func(config config.Config, states *state.StateRepo, router gin.IRouter){}

// environmentEndpoints serve the environment model and need the environment
// store rather than the legacy state repo, which is why they register separately.
var environmentEndpoints = []func(config config.Config, environments repo.Environments, notifier RuntimeNotifier, router gin.IRouter){}

// RuntimeNotifier is how a change to a stored environment reaches the running
// simulation. It is an interface and not the runtime itself so that this package
// keeps knowing nothing about how an environment is executed - and so that the
// api can be served without a runtime at all, which is what the tests do.
//
// Both methods concern exactly one environment: this is the point of the new
// runtime, and passing the whole store instead would invite the global restart
// the legacy runtime did on every edit.
type RuntimeNotifier interface {
	// Reload picks up the current definition of one environment.
	Reload(id string)
	// Remove stops one environment, after its definition was deleted.
	Remove(id string)
}

// notifyReload and notifyRemove tolerate a missing runtime: a nil notifier means
// the api runs as a store only, which is a valid configuration for a test and
// must not panic in a handler.
func notifyReload(notifier RuntimeNotifier, id string) {
	if notifier == nil {
		return
	}
	notifier.Reload(id)
}

func notifyRemove(notifier RuntimeNotifier, id string) {
	if notifier == nil {
		return
	}
	notifier.Remove(id)
}

func Start(ctx context.Context, config config.Config, staterepo *state.StateRepo, environments repo.Environments, notifier RuntimeNotifier) {
	server := &http.Server{
		Addr:              ":" + config.ServerPort,
		Handler:           NewRouter(config, staterepo, environments, notifier),
		WriteTimeout:      10 * time.Second,
		ReadTimeout:       2 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
	}
	go func() {
		util.Logger.Info("api listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// the process cannot do its job without the api, so someone has to act
			util.Logger.Error("api server stopped", attributes.ErrorKey, err)
		}
	}()
	go func() {
		<-ctx.Done()
		if err := server.Shutdown(context.Background()); err != nil {
			util.Logger.Warn("api shutdown returned an error", attributes.ErrorKey, err)
		}
	}()
}

// NewRouter builds the gin engine. Access logging, panic recovery and the error
// to status mapping come from gin-middleware instead of being hand written, so
// this service logs like the other services in the platform — and so a panic in
// a handler no longer kills the request without a trace.
//
// @title MOSES API
// @version 1.0.0
// @description Simulates environments — sites, buildings and apartments — and
// @description their devices, and publishes their data as if it came from real
// @description hardware. An environment is one document: GET returns what PUT
// @description accepts, so a whole site can be created in a single call.
// @description
// @description The legacy world/room/device endpoints are being replaced by the
// @description environment api and are not documented here.
// @BasePath /
// @securityDefinitions.apikey Bearer
// @in header
// @name Authorization
// @description A keycloak issued JWT. Verified at the gateway, not here.
func NewRouter(config config.Config, staterepo *state.StateRepo, environments repo.Environments, notifier RuntimeNotifier) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(
		gin_mw.StructLoggerHandlerWithDefaultGenerators(
			util.Logger.With(attributes.LogRecordTypeKey, attributes.HttpAccessLogRecordTypeVal),
			attributes.Provider,
			nil,
			nil,
		),
		gin_mw.ErrorHandler(GetStatusCode, ", "),
		gin_mw.StructRecoveryHandler(util.Logger, gin_mw.DefaultRecoveryFunc),
	)

	for _, e := range endpoints {
		util.Logger.Debug("add endpoints", "group", runtime.FuncForPC(reflect.ValueOf(e).Pointer()).Name())
		e(config, staterepo, router)
	}

	if environments == nil {
		// the environment api is served only once a store is wired in; the
		// other endpoints stay available either way
		util.Logger.Warn("no environment store configured, skipping the environment api")
	} else {
		if notifier == nil {
			// the environment api then edits stored documents without the
			// running simulation picking the change up
			util.Logger.Warn("no environment runtime configured, changes to an environment will not reach a running simulation")
		}
		for _, e := range environmentEndpoints {
			util.Logger.Debug("add endpoints", "group", runtime.FuncForPC(reflect.ValueOf(e).Pointer()).Name())
			e(config, environments, notifier, router)
		}
	}
	return router
}
