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
	environmentEndpoints = append(environmentEndpoints, HistoryEndpoints)
}

// HistoryEndpoints start, follow and abort the run that gives one environment a
// past.
func HistoryEndpoints(config config.Config, environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, router gin.IRouter) {
	for _, route := range []func(repo.Environments, RuntimeNotifier) (string, string, gin.HandlerFunc){
		postHistoryH,
		getHistoryH,
		deleteHistoryH,
	} {
		method, path, handler := route(environments, notifier)
		router.Handle(method, path, handler)
	}
}

// HistoryRequest is where the run starts. There is no end: a run always ends at
// the present, because its result is the live state.
type HistoryRequest struct {
	From time.Time `json:"from" example:"2026-07-01T00:00:00Z"`
}

// @Summary Run one environment from a past instant up to now
// @Description Simulates the environment from `from` to the present on a virtual clock and publishes every reading under the instant it was computed for. Unlike a backfill the run carries state, and the state it arrives at **becomes** the live state: a meter reading continues into the live one as one ramp rather than starting a second one next to it. Runs asynchronously; the response is the run as it stands, `GET` follows it and `DELETE` aborts it.
// @Description
// @Description **The live simulation of this environment is suspended for the duration of the run and its current state is discarded.** While it runs, `PATCH /environments/{id}/state`, the state snapshot and a second run are answered with 409, incoming commands are dropped, and an edit to the definition takes effect when the run ends. State `done` means the live simulation is running again on the state the run arrived at.
// @Description
// @Description Only a channel whose platform service declares the attribute `senergy/time_path` can publish with a historical timestamp; every other channel is computed but sends nothing, so the environment as a whole still arrives at the right state. Every source kind is simulated, script and schedule included, which is what a backfill cannot do. Every channel that publishes nothing says why in `channels` of the status.
// @Description
// @Description There is no end: the run ends at the present, and once it has simulated the window it keeps going until it has caught up with the time it spent doing so, so that no gap is left at the handover. `to` in the status is therefore where the run actually ended.
// @Description
// @Description The window may not start in the future, has to be at least a minute and at most 366 days long, and may not come to more than twenty million simulation steps across the channels and context sources of the environment.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Param window body HistoryRequest true "the instant to start from"
// @Success 202 {object} moses_runtime.HistoryStatus "accepted, with the run as it stands"
// @Failure 400 {string} string "the body is unreadable, or the window is in the future, too long or too dense"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, no access to it, or it is not running here"
// @Failure 409 {string} string "a history run or a backfill of this environment is already running"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/history [post]
func postHistoryH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPost, "/environments/:id/history", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		if _, ok = accessibleEnvironment(gc, environments, token, id); !ok {
			return
		}

		request := HistoryRequest{}
		if err := gc.ShouldBindJSON(&request); err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as a history window: %s", err.Error())
			return
		}

		status, err := startHistory(notifier, id, request.From)
		rangeError := &moses_runtime.HistoryRangeError{}
		switch {
		case err == nil:
			gc.JSON(http.StatusAccepted, status)
		case errors.As(err, &rangeError):
			gc.String(http.StatusBadRequest, "%s", rangeError.Error())
		case errors.Is(err, moses_runtime.ErrHistoryRunning), errors.Is(err, moses_runtime.ErrBackfillRunning):
			gc.String(http.StatusConflict, "%s", err.Error())
		case errors.Is(err, repo.ErrNotRunning), errors.Is(err, ErrNoRuntime):
			//404 and not 409, as the state endpoint does: from outside, an
			//environment this instance does not run is indistinguishable from one
			//that does not exist here
			gc.String(http.StatusNotFound, "the environment is not running here")
		default:
			util.Logger.Error("unable to start the history run", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to start the history run")
		}
	}
}

// @Summary The history run of one environment
// @Description Where the run stands: running, done, failed or cancelled, which virtual instant it has reached, how many publish steps went out or were refused, and per channel what became of it — including the reason a channel published nothing at all. `done` means the live simulation is running again on the state the run arrived at; `failed` and `cancelled` mean it is running again on the partial state the run had reached, which is a consistent state of an earlier instant and not a rollback.
// @Description
// @Description The registry is held in memory only. A restart forgets every run, and this then answers 404 rather than claiming a state it cannot know.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 200 {object} moses_runtime.HistoryStatus
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, no access to it, or nothing is known about a history run of it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/history [get]
func getHistoryH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments/:id/history", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		if _, ok = accessibleEnvironment(gc, environments, token, id); !ok {
			return
		}
		status, err := historyStatusOf(notifier, id)
		switch {
		case err == nil:
			gc.JSON(http.StatusOK, status)
		case errors.Is(err, moses_runtime.ErrNoHistory), errors.Is(err, ErrNoRuntime):
			gc.String(http.StatusNotFound, "nothing is known about a history run of this environment")
		default:
			util.Logger.Error("unable to read the history run", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to read the history run")
		}
	}
}

// @Summary Abort the history run of one environment
// @Description Ends a running simulation of the past. It does not wait and it does not undo: the run stops at its next step and hands the environment back to the live simulation, which then continues from the partial state — a consistent state of an earlier instant. The response is the run as it stood when the abort was accepted.
// @Description
// @Description A run over a year of minute data suspends the live simulation for a long time, which is why it has a switch a backfill does not need.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 202 {object} moses_runtime.HistoryStatus "the abort was accepted"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, no access to it, or nothing is known about a history run of it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/history [delete]
func deleteHistoryH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodDelete, "/environments/:id/history", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		if _, ok = accessibleEnvironment(gc, environments, token, id); !ok {
			return
		}
		status, err := cancelHistory(notifier, id)
		switch {
		case err == nil:
			gc.JSON(http.StatusAccepted, status)
		case errors.Is(err, moses_runtime.ErrNoHistory), errors.Is(err, ErrNoRuntime):
			gc.String(http.StatusNotFound, "nothing is known about a history run of this environment")
		default:
			util.Logger.Error("unable to abort the history run", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to abort the history run")
		}
	}
}
