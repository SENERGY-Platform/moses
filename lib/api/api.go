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
	"errors"
	"net/http"
	"reflect"
	"runtime"
	"time"

	gin_mw "github.com/SENERGY-Platform/gin-middleware"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/devices"
	"github.com/SENERGY-Platform/moses/lib/repo"
	moses_runtime "github.com/SENERGY-Platform/moses/lib/runtime"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/gin-gonic/gin"
)

var endpoints = []func(config config.Config, states *state.StateRepo, router gin.IRouter){}

// environmentEndpoints need the environment store rather than the legacy state
// repo, hence a separate registration.
var environmentEndpoints = []func(config config.Config, environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, router gin.IRouter){}

// datasetEndpoints serve uploaded timeseries files; they need only their store.
var datasetEndpoints = []func(config config.Config, datasets repo.Datasets, router gin.IRouter){}

// catalogEndpoints serve the device types and devices an asset is built from.
var catalogEndpoints = []func(config config.Config, catalog DeviceCatalog, router gin.IRouter){}

// DeviceCatalog is what the api needs from the platform's device registry: the
// types an asset can be built from, and the device it publishes through. An
// interface so the handlers can be tested without a device-repository.
type DeviceCatalog interface {
	DeviceTypes(token string) ([]devices.DeviceType, error)
	CreateDevice(ctx context.Context, token string, deviceTypeId string, name string) (devices.Device, error)
	DeleteDevice(ctx context.Context, token string, id string) error
}

// RuntimeNotifier is how a change to a stored environment reaches the running
// simulation. An interface so the api can be served without a runtime at all.
//
// Both methods concern exactly one environment - passing the whole store would
// invite the global restart the legacy runtime did on every edit.
type RuntimeNotifier interface {
	// Reload picks up the current definition of one environment.
	Reload(id string)
	// Remove stops one environment, after its definition was deleted.
	Remove(id string)
	// SetState merges values into the live state of one running environment. It
	// reports ErrNotRunning for an environment the runtime does not hold, and a
	// *repo.UnknownIdsError for zone or asset ids the definition does not have.
	SetState(id string, change repo.StateChange) error

	// StartBackfill reconstructs one environment over a past window and returns
	// the job as it stands. It reports a *runtime.BackfillRangeError for a
	// window it will not serve, runtime.ErrBackfillRunning when one is already
	// running, and repo.ErrNotRunning for an environment it does not hold.
	StartBackfill(id string, from time.Time, to time.Time) (moses_runtime.BackfillStatus, error)

	// BackfillStatusOf follows a job. It reports runtime.ErrNoBackfill when
	// nothing is known, which is also the answer after a restart.
	BackfillStatusOf(id string) (moses_runtime.BackfillStatus, error)
}

// A nil notifier means the api runs as a store only, valid in a test, so this
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

// ErrNoRuntime is what a state change gets when the api runs as a store only.
// Storing the change would be worse than refusing it: the runtime is what would
// apply it, and a caller told "ok" would wait for an effect that never comes.
var ErrNoRuntime = errors.New("this instance serves the store only and runs no simulation")

func setState(notifier RuntimeNotifier, id string, change repo.StateChange) error {
	if notifier == nil {
		return ErrNoRuntime
	}
	return notifier.SetState(id, change)
}

func Start(ctx context.Context, config config.Config, staterepo *state.StateRepo, environments repo.Environments, datasets repo.Datasets, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) {
	server := &http.Server{
		Addr:              ":" + config.ServerPort,
		Handler:           NewRouter(config, staterepo, environments, datasets, catalog, mirror, notifier),
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

// NewRouter builds the gin engine. Access logging, panic recovery and error to
// status mapping come from gin-middleware rather than being hand written.
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
func NewRouter(config config.Config, staterepo *state.StateRepo, environments repo.Environments, datasets repo.Datasets, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) *gin.Engine {
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
		if mirror == nil {
			// the environments are then not mirrored as graphs, and the
			// applications that read those graphs do not see them
			util.Logger.Warn("no graph mirror configured, environments will not appear as graphs")
		}
		if catalog == nil {
			util.Logger.Warn("no device catalog configured, an editor cannot offer device types")
		} else {
			for _, e := range catalogEndpoints {
				e(config, catalog, router)
			}
		}
		if datasets == nil {
			util.Logger.Warn("no dataset store configured, skipping the dataset api")
		} else {
			for _, e := range datasetEndpoints {
				e(config, datasets, router)
			}
		}
		for _, e := range environmentEndpoints {
			util.Logger.Debug("add endpoints", "group", runtime.FuncForPC(reflect.ValueOf(e).Pointer()).Name())
			e(config, environments, catalog, mirror, notifier, router)
		}
	}
	return router
}
