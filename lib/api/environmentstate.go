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
	environmentEndpoints = append(environmentEndpoints, EnvironmentStateEndpoints)
}

// EnvironmentStateEndpoints serve the reading direction of the live state. The
// writing direction is PATCH /environments/{id}/state in environment.go, and the
// two are deliberately the same shape.
func EnvironmentStateEndpoints(config config.Config, environments repo.Environments, shares repo.Shares, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, permissions Permissions, router gin.IRouter) {
	for _, route := range []func(repo.Environments, RuntimeNotifier) (string, string, gin.HandlerFunc){
		getEnvironmentStateH,
	} {
		method, path, handler := route(environments, notifier)
		router.Handle(method, path, handler)
	}
}

// EnvironmentState is the live state of one environment as it is read.
//
// repo.StateChange is embedded rather than copied field by field: it is the body
// PATCH accepts, and embedding is what keeps the two from drifting apart. A
// client can take context, zones and assets out of this answer, change a value
// and send them straight back.
type EnvironmentState struct {
	repo.StateChange

	// Running says whether this instance simulates the environment at all. An
	// environment that is stored but not running here is not an error - it is
	// the normal case for a document that was just written, or one another
	// instance runs - and it carries no state, which is what an editor turns
	// into its empty state.
	Running bool `json:"running" example:"true"`

	// HistoryRunning says that the environment stands at a past instant while a
	// history run rebuilds it. It carries no state then and is not running in the
	// live sense, which is why it is a flag of its own rather than a state an
	// editor would show as current.
	HistoryRunning bool `json:"history_running,omitempty" example:"false"`

	// AsOf is when the values were read, RFC3339. It is not decoration: a zone
	// value with a time constant is on its way to a set point and is resolved to
	// exactly this instant, so the number means nothing without it.
	AsOf time.Time `json:"as_of" example:"2026-08-27T09:41:00Z"`
}

// @Summary Live state of one environment
// @Description What the running simulation currently holds: the shared context, the state of every zone and the state of every asset. This is the reading direction of PATCH on the same path and answers in the same shape, so a value can be read, changed and sent straight back.
// @Description
// @Description A zone value that the definition gives a time constant is on its way to its set point rather than at it; it is resolved to the instant in `as_of`, which is the same value a script would read at that moment.
// @Description
// @Description These are live values, not the definition: they are not what GET /environments/{id} returns, and they are not stored by reading them.
// @Description
// @Description An environment that is stored but not simulated here answers 200 with `running: false` and no states. That is not an error: another instance may run it, or it may just have been written. Only an environment that does not exist, or one the caller may not see, is a 404.
// @Description
// @Description While a history run rebuilds the environment from a past instant the answer is 200 with `running: false` and `history_running: true`. There is no live state to read then: the environment stands in the past, and reading it would resolve its values against the wall clock and corrupt the run.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 200 {object} EnvironmentState
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, or no access to it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/state [get]
func getEnvironmentStateH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments/:id/state", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		//ownership is decided on the stored document, exactly as the writing
		//direction does: the runtime knows no owners
		if _, ok = accessibleEnvironment(gc, environments, token, id); !ok {
			return
		}

		snapshot, err := snapshotState(notifier, id)
		switch {
		case err == nil:
			gc.JSON(http.StatusOK, EnvironmentState{
				StateChange: snapshot.State,
				Running:     true,
				AsOf:        snapshot.AsOf,
			})
		case errors.Is(err, moses_runtime.ErrHistoryRunning):
			//an answer rather than a 409, for the same reason: a read has one, and
			//naming the run is what lets an editor say why the values are gone
			gc.JSON(http.StatusOK, EnvironmentState{Running: false, HistoryRunning: true, AsOf: time.Now()})
		case errors.Is(err, repo.ErrNotRunning), errors.Is(err, ErrNoRuntime):
			//200 and not 404, unlike the writing direction: a PATCH that cannot
			//be applied has failed, while a read of an environment that produces
			//nothing has an answer - that it produces nothing. The caller already
			//passed the access check, so nothing is disclosed by saying so.
			gc.JSON(http.StatusOK, EnvironmentState{Running: false, AsOf: time.Now()})
		default:
			util.Logger.Error("unable to read the live state", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to read the live state")
		}
	}
}
