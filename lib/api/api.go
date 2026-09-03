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
var environmentEndpoints = []func(config config.Config, environments repo.Environments, shares repo.Shares, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, permissions Permissions, router gin.IRouter){}

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

	// Snapshot reads the live state of one running environment, in the shape
	// SetState accepts. It reports repo.ErrNotRunning for an environment the
	// runtime does not hold.
	Snapshot(id string) (moses_runtime.StateSnapshot, error)

	// StartBackfill reconstructs one environment over a past window and returns
	// the job as it stands. It reports a *runtime.BackfillRangeError for a
	// window it will not serve, runtime.ErrBackfillRunning when one is already
	// running, and repo.ErrNotRunning for an environment it does not hold.
	StartBackfill(id string, from time.Time, to time.Time) (moses_runtime.BackfillStatus, error)

	// BackfillStatusOf follows a job. It reports runtime.ErrNoBackfill when
	// nothing is known, which is also the answer after a restart.
	BackfillStatusOf(id string) (moses_runtime.BackfillStatus, error)

	// StartHistory runs one environment from a past instant up to now and makes
	// the state it arrives at the live one. It reports a
	// *runtime.HistoryRangeError for a window it will not serve,
	// runtime.ErrHistoryRunning or runtime.ErrBackfillRunning when one of the two
	// is already running, and repo.ErrNotRunning for an environment it does not
	// hold.
	StartHistory(id string, from time.Time) (moses_runtime.HistoryStatus, error)

	// HistoryStatusOf follows a run. It reports runtime.ErrNoHistory when nothing
	// is known, which is also the answer after a restart.
	HistoryStatusOf(id string) (moses_runtime.HistoryStatus, error)

	// CancelHistory aborts a run and returns where it stood. It reports
	// runtime.ErrNoHistory when nothing is known.
	CancelHistory(id string) (moses_runtime.HistoryStatus, error)
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

// A store only instance has no live state to read, which is the same answer as
// an environment that is not running here.
func snapshotState(notifier RuntimeNotifier, id string) (moses_runtime.StateSnapshot, error) {
	if notifier == nil {
		return moses_runtime.StateSnapshot{}, ErrNoRuntime
	}
	return notifier.Snapshot(id)
}

// The history endpoints answer rather than panic on a store only deployment, the
// same way the backfill ones do.
func startHistory(notifier RuntimeNotifier, id string, from time.Time) (moses_runtime.HistoryStatus, error) {
	if notifier == nil {
		return moses_runtime.HistoryStatus{}, ErrNoRuntime
	}
	return notifier.StartHistory(id, from)
}

func historyStatusOf(notifier RuntimeNotifier, id string) (moses_runtime.HistoryStatus, error) {
	if notifier == nil {
		return moses_runtime.HistoryStatus{}, ErrNoRuntime
	}
	return notifier.HistoryStatusOf(id)
}

func cancelHistory(notifier RuntimeNotifier, id string) (moses_runtime.HistoryStatus, error) {
	if notifier == nil {
		return moses_runtime.HistoryStatus{}, ErrNoRuntime
	}
	return notifier.CancelHistory(id)
}

func Start(ctx context.Context, config config.Config, staterepo *state.StateRepo, environments repo.Environments, shares repo.Shares, datasets repo.Datasets, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, permissions Permissions) {
	server := &http.Server{
		Addr:              ":" + config.ServerPort,
		Handler:           NewRouter(config, staterepo, environments, shares, datasets, catalog, mirror, notifier, permissions),
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
func NewRouter(config config.Config, staterepo *state.StateRepo, environments repo.Environments, shares repo.Shares, datasets repo.Datasets, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, permissions Permissions) *gin.Engine {
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
		if permissions == nil || shares == nil {
			// the share endpoints then answer 500 rather than storing a set
			// nobody granted, and a new device inherits nothing
			util.Logger.Warn("no permissions client or share store configured, the devices of an environment cannot be shared")
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
			e(config, environments, shares, catalog, mirror, notifier, permissions, router)
		}
	}
	return router
}
