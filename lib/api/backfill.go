/*
 * Copyright 2026 InfAI (CC SES)
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
	"errors"
	"net/http"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/repo"
	moses_runtime "github.com/SENERGY-Platform/moses/lib/runtime"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/gin-gonic/gin"
)

func init() {
	environmentEndpoints = append(environmentEndpoints, BackfillEndpoints)
}

// BackfillEndpoints start and observe the reconstruction of an environment over
// a window that has already passed.
func BackfillEndpoints(config config.Config, environments repo.Environments, shares repo.Shares, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, permissions Permissions, router gin.IRouter) {
	for _, route := range []func(repo.Environments, RuntimeNotifier) (string, string, gin.HandlerFunc){
		postBackfillH,
		getBackfillH,
	} {
		method, path, handler := route(environments, notifier)
		router.Handle(method, path, handler)
	}
}

// BackfillRequest is the window to reconstruct.
type BackfillRequest struct {
	From time.Time `json:"from" example:"2026-07-01T00:00:00Z"`
	To   time.Time `json:"to" example:"2026-08-01T00:00:00Z"`
}

// @Summary Reconstruct one environment over a past window
// @Description Computes the environment over the given window and publishes every reading with the timestamp it would have had, so that a model can be trained on history the environment never actually lived through. Runs asynchronously; the response is the job as it stands at that moment, and GET on the same path follows it.
// @Description
// @Description Only a channel whose platform service declares the attribute `senergy/time_path` can be backfilled, because that attribute is what makes the platform read the event time out of the payload instead of stamping the arrival time. moses does not change device types to add it. Only profile and dataset sources are reconstructed: a script source depends on the state its earlier runs left behind, and a formula follows from other channels. Every channel that is skipped says why in the status.
// @Description
// @Description The window may not end in the future, may not span more than 366 days, and may not come to more than two million readings in total across the channels of the environment; a reading is published synchronously, so that count is the runtime of the job.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Param window body BackfillRequest true "the window to reconstruct"
// @Success 202 {object} moses_runtime.BackfillStatus "accepted, with the job as it stands"
// @Failure 400 {string} string "the body is unreadable, or the window is empty, in the future, too long or too dense"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, no access to it, or it is not running here"
// @Failure 409 {string} string "a backfill or a history run of this environment is already running"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/backfill [post]
func postBackfillH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPost, "/environments/:id/backfill", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		if _, ok = accessibleEnvironment(gc, environments, token, id); !ok {
			return
		}

		request := BackfillRequest{}
		if err := gc.ShouldBindJSON(&request); err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as a backfill window: %s", err.Error())
			return
		}
		if notifier == nil {
			gc.String(http.StatusNotFound, "the environment is not running here")
			return
		}

		status, err := notifier.StartBackfill(id, request.From, request.To)
		rangeError := &moses_runtime.BackfillRangeError{}
		switch {
		case err == nil:
			gc.JSON(http.StatusAccepted, status)
		case errors.As(err, &rangeError):
			gc.String(http.StatusBadRequest, "%s", rangeError.Error())
		case errors.Is(err, moses_runtime.ErrBackfillRunning), errors.Is(err, moses_runtime.ErrHistoryRunning):
			gc.String(http.StatusConflict, "%s", err.Error())
		case errors.Is(err, repo.ErrNotRunning), errors.Is(err, ErrNoRuntime):
			//404 and not 409, as the state endpoint does: from outside, an
			//environment this instance does not run is indistinguishable from
			//one that does not exist here
			gc.String(http.StatusNotFound, "the environment is not running here")
		default:
			util.Logger.Error("unable to start the backfill", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to start the backfill")
		}
	}
}

// @Summary The backfill of one environment
// @Description Where the job stands: running, done, failed or cancelled, how many channels are finished with, which instant it is at, and per channel either the number of readings published or the reason it was skipped.
// @Description
// @Description The registry is held in memory only. A restart forgets every job, and this then answers 404 rather than claiming a state it cannot know: a job is not resumable, because it would have to know which readings already reached the platform, and the platform keeps a second one rather than replacing it.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 200 {object} moses_runtime.BackfillStatus
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, no access to it, or nothing is known about a backfill of it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/backfill [get]
func getBackfillH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments/:id/backfill", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		if _, ok = accessibleEnvironment(gc, environments, token, id); !ok {
			return
		}
		if notifier == nil {
			gc.String(http.StatusNotFound, "nothing is known about a backfill of this environment")
			return
		}
		status, err := notifier.BackfillStatusOf(id)
		switch {
		case err == nil:
			gc.JSON(http.StatusOK, status)
		case errors.Is(err, moses_runtime.ErrNoBackfill), errors.Is(err, ErrNoRuntime):
			gc.String(http.StatusNotFound, "nothing is known about a backfill of this environment")
		default:
			util.Logger.Error("unable to read the backfill status", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to read the backfill status")
		}
	}
}
