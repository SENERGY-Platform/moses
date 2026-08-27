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
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"
)

func init() {
	environmentEndpoints = append(environmentEndpoints, EnvironmentEndpoints)
}

// EnvironmentEndpoints serves the environment model. GET returns exactly what
// PUT accepts, so an export can be edited and put back.
//
// Each route is its own function returning method, path and handler, because
// swaggo reads annotations above a function declaration only.
func EnvironmentEndpoints(config config.Config, environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, router gin.IRouter) {
	for _, route := range []func(repo.Environments, DeviceCatalog, GraphMirror, RuntimeNotifier) (string, string, gin.HandlerFunc){
		listEnvironmentsH,
		getEnvironmentH,
		putEnvironmentH,
		postEnvironmentH,
		deleteEnvironmentH,
		patchEnvironmentStateH,
		getSwaggerDocH,
	} {
		method, path, handler := route(environments, catalog, mirror, notifier)
		router.Handle(method, path, handler)
	}
}

// @Summary List environments
// @Description Every environment owned by the caller, ordered by name. Empty list, never null.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Success 200 {array} domain.Environment
// @Failure 400 {string} string "the token is missing or unreadable"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /environments [get]
func listEnvironmentsH(environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		//an admin sees every environment, matching mayAccess, which already lets
		//one open any of them: a list that hid what the detail route serves
		//would only make them unfindable
		result, err := listFor(gc, environments, token)
		if err != nil {
			util.Logger.Error("unable to list environments", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to list environments")
			return
		}
		gc.JSON(http.StatusOK, result)
	}
}

// @Summary Export one environment
// @Description Returns exactly what PUT accepts, so an export can be edited and put back.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 200 {object} domain.Environment
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, or no access to it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id} [get]
func getEnvironmentH(environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env, err := environments.Get(gc.Request.Context(), gc.Param("id"))
		if errors.Is(err, repo.ErrNotFound) {
			gc.String(http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}
		if !mayAccess(token, env) {
			// 404 not 403: existence is not information for a caller
			// without access
			gc.String(http.StatusNotFound, "not found")
			return
		}
		gc.JSON(http.StatusOK, env)
	}
}

// @Summary Create or replace one environment
// @Description Idempotent. The id in the path wins over the one in the body, so a document can be copied to a new id without editing it. Ownership comes from the token on create and never transfers on update. An asset without an external_ref gets a platform device created for it, and a device created that way is deleted again when the asset that carried it is gone from the document; a device attached to an asset by the caller is never deleted. external_managed says which is which and is decided by the server, so sending it has no effect. The environment is also mirrored as a graph in the device-repository, rebuilt from the document on every save, so a change made to that graph by hand does not survive; external_graph_ref names the graph and is decided by the server, so sending it has no effect either.
// @Description
// @Description Send back the `version` of the document you read and the write is refused with 409 if anybody stored a change in between; the response of every successful write carries the new version. Sending 0, or leaving the field out, writes unchecked — which is what a client that knows nothing of the field does, and what makes losing a concurrent edit, and the devices only the other document still references, possible.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Param environment body domain.Environment true "the environment"
// @Success 200 {object} domain.Environment
// @Failure 400 {object} domain.ValidationError "every problem, with the path of the offending field"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "the environment belongs to somebody else"
// @Failure 409 {string} string "the document was changed since it was read; the message names both versions"
// @Failure 500 {string} string "error message"
// @Router /environments/{id} [put]
func putEnvironmentH(environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPut, "/environments/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env := domain.Environment{}
		err := gc.ShouldBindJSON(&env)
		if err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as an environment: %s", err.Error())
			return
		}
		// path wins over body, so a document can be copied to a new id
		env.Id = gc.Param("id")
		// read out before anything else touches the document: this is the one
		// field of the body that is about the write rather than about the content
		carried := env.Version

		// previous stays nil for a document that is new under this id, including a
		// copy put under a fresh one: nothing of it was provisioned by moses, so
		// none of its devices may be deleted with it later
		var previous *domain.Environment

		existing, err := environments.Get(gc.Request.Context(), env.Id)
		switch {
		case err == nil:
			if !mayAccess(token, existing) {
				gc.String(http.StatusNotFound, "not found")
				return
			}
			// ownership never transfers through an import
			env.Owner = existing.Owner
			previous = &existing
			// The conflict is decided HERE, before anything outside the store is
			// touched. Everything below has an effect that outlives a refused
			// request: provisioning creates platform devices, mirrorGraph rewrites
			// a graph, and the cleanup after the write deletes devices - and it is
			// that last one the version exists for, because a loser deleting a
			// device the winner still publishes through is the damage.
			//
			// This check is NOT the guard. Two callers can pass it in the same
			// instant; the compare-and-swap in the store is what decides. What it
			// is, is the difference between the ordinary conflict - a second
			// editor working from a stale document - having no side effects at
			// all, and having the side effects of a write that never happened.
			if carried > 0 && carried != existing.Version {
				writeVersionConflict(gc, env.Id, carried, existing.Version)
				return
			}
		case errors.Is(err, repo.ErrNotFound):
			env.Owner = token.GetUserId()
			// a version carried against a document that is not stored is not a
			// conflict: putting an export under a new id is how a document is
			// copied, and that export carries the version of the original
			carried = 0
		default:
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}

		domain.AssignIds(&env)

		err = domain.Validate(env)
		if err != nil {
			writeValidationError(gc, err)
			return
		}

		//which devices moses may delete is decided from the stored document, not
		//from the flags the client sent back
		reconcileManagedFlags(previous, &env)
		//and so is which graph it owns: a copy under a new id must not write into
		//the graph of the document it was copied from
		reconcileGraphRef(previous, &env)

		//after validation, before the write: a refused document creates nothing
		if err = provisionDevices(gc.Request.Context(), catalog, token, &env); err != nil {
			gc.String(http.StatusInternalServerError, "%s", err.Error())
			return
		}

		//after provisioning, so the graph carries the devices this save created,
		//and before the write, so the ref it assigns is stored with the document.
		//Best effort: see mirrorGraph
		mirrorGraph(mirror, token, &env)

		stored, err := storeEnvironment(gc.Request.Context(), environments, env, carried)
		conflict := &repo.VersionConflictError{}
		switch {
		case err == nil:
		case errors.As(err, &conflict):
			// the narrow case the check above cannot catch: the winning write
			// landed between the read and this one. The devices this call may
			// have created are left standing and logged, which is the same thing
			// a failed write has always left behind - and the cleanup below,
			// the one that deletes, is what does not run
			util.Logger.Warn("a concurrent write won, this one was refused",
				"environment", env.Id, "carried_version", conflict.Expected, "stored_version", conflict.Stored)
			gc.String(http.StatusConflict, "%s", conflict.Error())
			return
		default:
			util.Logger.Error("unable to store environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to store environment")
			return
		}
		// the store counts the version, so the answer carries what it decided and
		// not what the request brought
		env.Version = stored
		// after the write: the runtime reads the definition back, so notifying
		// earlier would restart it on the old one
		notifyReload(notifier, env.Id)
		// also after the write, and for the same reason in reverse: a failed write
		// must leave every device standing, or the stored document would point at
		// devices that no longer exist
		deleteDevices(gc.Request.Context(), catalog, token, env.Id, orphanedDevices(previous, &env))
		gc.JSON(http.StatusOK, env)
	}
}

// storeEnvironment writes with the concurrency check the caller asked for.
// carried is the version the client sent back, and zero is what a client that
// does not know the field sends - those writes go through unchecked, and are
// still given a version of their own by the store.
func storeEnvironment(ctx context.Context, environments repo.Environments, env domain.Environment, carried int64) (int64, error) {
	if carried > 0 {
		return environments.PutIfVersion(ctx, env, carried)
	}
	return environments.Put(ctx, env)
}

// writeVersionConflict answers a refused write with both versions in the
// message: the only useful reaction is to read the document again, and a caller
// that cannot see how far behind it was cannot tell a stale editor from a bug.
func writeVersionConflict(gc *gin.Context, id string, carried int64, stored int64) {
	conflict := &repo.VersionConflictError{Id: id, Expected: carried, Stored: stored}
	util.Logger.Info("refused a write against an outdated version",
		"environment", id, "carried_version", carried, "stored_version", stored)
	gc.String(http.StatusConflict, "%s", conflict.Error())
}

// @Summary Create an environment with a server assigned id
// @Description Any id in the body is ignored. Nested entities may omit their ids and get one assigned. An asset without an external_ref gets a platform device created for it, which is then deleted again when the asset or the environment is gone; a device attached to an asset by the caller is never deleted. external_managed is decided by the server and is always false on create. The environment is also mirrored as a graph in the device-repository; external_graph_ref names that graph and is assigned by the server, so sending it has no effect. A version in the body is ignored as well: a document that is being created has nothing to be concurrent with, and the one in the response is the one to send back on the next PUT.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param environment body domain.Environment true "the environment"
// @Success 201 {object} domain.Environment
// @Failure 400 {object} domain.ValidationError "every problem, with the path of the offending field"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /environments [post]
func postEnvironmentH(environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPost, "/environments", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env := domain.Environment{}
		err := gc.ShouldBindJSON(&env)
		if err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as an environment: %s", err.Error())
			return
		}
		env.Id = ""
		env.Owner = token.GetUserId()
		//a create has nothing to be concurrent with: the id is assigned here and
		//nobody else knows it yet, so a version in the body is as meaningless as
		//the id in it
		env.Version = 0
		domain.AssignIds(&env)

		err = domain.Validate(env)
		if err != nil {
			writeValidationError(gc, err)
			return
		}

		//nothing here was provisioned by moses yet, so an external_managed the
		//client sent along claims a right over a device it does not have, and an
		//external_graph_ref a right over somebody's graph
		reconcileManagedFlags(nil, &env)
		reconcileGraphRef(nil, &env)

		//after validation, before the write: a refused document creates nothing
		if err = provisionDevices(gc.Request.Context(), catalog, token, &env); err != nil {
			gc.String(http.StatusInternalServerError, "%s", err.Error())
			return
		}

		mirrorGraph(mirror, token, &env)

		//unchecked on purpose: the id is fresh, so there is no stored version to
		//compare against, and the store starts a new document at 1
		stored, err := environments.Put(gc.Request.Context(), env)
		if err != nil {
			util.Logger.Error("unable to store environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to store environment")
			return
		}
		env.Version = stored
		notifyReload(notifier, env.Id)
		gc.JSON(http.StatusCreated, env)
	}
}

// @Summary Delete one environment and its runtime state
// @Description The platform devices moses created for the assets of this environment are deleted with it. Devices attached to an asset by the caller stay, together with their timeseries. The graph this environment is mirrored as is deleted with it. Neither a device nor a graph that cannot be deleted fails the request.
// @Tags Environment
// @Security Bearer
// @Param id path string true "environment id"
// @Success 204 {string} string "deleted, or there was nothing to delete"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "the environment belongs to somebody else"
// @Failure 500 {string} string "error message"
// @Router /environments/{id} [delete]
func deleteEnvironmentH(environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodDelete, "/environments/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env, err := environments.Get(gc.Request.Context(), gc.Param("id"))
		if errors.Is(err, repo.ErrNotFound) {
			// deleting something that is not there is not an error
			gc.Status(http.StatusNoContent)
			return
		}
		if err != nil {
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}
		if !mayAccess(token, env) {
			gc.String(http.StatusNotFound, "not found")
			return
		}
		err = environments.Delete(gc.Request.Context(), env.Id)
		if err != nil {
			util.Logger.Error("unable to delete environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete environment")
			return
		}
		notifyRemove(notifier, env.Id)
		//after the delete: a failed delete leaves the environment, and it has to
		//keep the devices it publishes through. Devices the user picked stay in
		//either case, they are inventory of the platform and not ours to remove
		deleteDevices(gc.Request.Context(), catalog, token, env.Id, managedDevicesOf(&env))
		//also after the delete, and best effort for the same reason: a graph
		//without an environment is cheaper than a delete that fails
		deleteGraph(mirror, token, &env)
		gc.Status(http.StatusNoContent)
	}
}

// patchEnvironmentStateH turns a boundary condition from outside the simulation.
// The room climate case is what it exists for: set a hall temperature and the
// sensors in that hall pick it up on their next tick, which is what the legacy
// world/room state was reaching for and never finished.
//
// @Summary Set live state of one running environment
// @Description Merges the given values into the live state. Only the keys named are touched; everything else keeps running. Context is the shared surroundings every zone reads, zones and assets are keyed by their id from the definition. Takes effect on the next tick of the scripts that read it, and is not a change to the definition.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Param state body repo.StateChange true "the values to set"
// @Success 204 {string} string "applied"
// @Failure 400 {string} string "the body is unreadable, empty, or names a zone or asset the definition does not have"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, no access to it, or it is not running here"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/state [patch]
func patchEnvironmentStateH(environments repo.Environments, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPatch, "/environments/:id/state", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")

		existing, err := environments.Get(gc.Request.Context(), id)
		switch {
		case errors.Is(err, repo.ErrNotFound):
			gc.String(http.StatusNotFound, "not found")
			return
		case err != nil:
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}
		if !mayAccess(token, existing) {
			gc.String(http.StatusNotFound, "not found")
			return
		}

		change := repo.StateChange{}
		if err = gc.ShouldBindJSON(&change); err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as a state change: %s", err.Error())
			return
		}
		if change.Empty() {
			gc.String(http.StatusBadRequest, "the state change is empty, so it would do nothing")
			return
		}

		err = setState(notifier, id, change)
		unknownIds := &repo.UnknownIdsError{}
		switch {
		case err == nil:
			gc.Status(http.StatusNoContent)
		case errors.As(err, &unknownIds):
			gc.String(http.StatusBadRequest, "%s", unknownIds.Error())
		case errors.Is(err, repo.ErrNotRunning), errors.Is(err, ErrNoRuntime):
			//404 and not 409: from outside, an environment this instance does not
			//run is indistinguishable from one that does not exist here
			gc.String(http.StatusNotFound, "the environment is not running here")
		default:
			util.Logger.Error("unable to set the environment state", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to set the environment state")
		}
	}
}

// @Summary The generated openapi specification of this service
// @Tags Doc
// @Produce json
// @Success 200 {string} string "the specification"
// @Failure 500 {string} string "error message"
// @Router /doc [get]
func getSwaggerDocH(_ repo.Environments, _ DeviceCatalog, _ GraphMirror, _ RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/doc", func(gc *gin.Context) {
		// generated at image build time by go generate, not committed
		if _, err := os.Stat(swaggerDocPath); err != nil {
			util.Logger.Warn("the openapi specification is not present", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "the specification was not generated for this build")
			return
		}
		gc.Header("Content-Type", gin.MIMEJSON)
		gc.File(swaggerDocPath)
	}
}

const swaggerDocPath = "docs/swagger.json"

// requireUser reads the caller's token. Every handler goes through here, which
// makes it the trust boundary.
//
// TRUST BOUNDARY, BY DESIGN: the token is parsed, not verified - the api gateway
// checks signature and claims first, as it does for the sibling services, so a
// garbage signature or expired token passes here. environment_test.go pins that
// so it is not read as an oversight. Expose moses without a validating gateway
// and this function is the one place to change.
//
// The check that is ours: a payload without a subject parses fine and yields an
// empty user id, which would be stored as owner and match every other
// subjectless token. That is a 401; an unparseable token is a 400.
//
// The returned token carries the raw credential, because downstream calls
// forward it. Never serialise it into a response or a log line.
func requireUser(gc *gin.Context) (sc_jwt.Token, bool) {
	token, err := sc_jwt.GetParsedToken(gc.Request)
	if err != nil {
		gc.String(http.StatusBadRequest, "%s", err.Error())
		return token, false
	}
	if token.GetUserId() == "" {
		gc.String(http.StatusUnauthorized, "the token carries no subject")
		return token, false
	}
	return token, true
}

func listFor(gc *gin.Context, environments repo.Environments, token sc_jwt.Token) ([]domain.Environment, error) {
	if token.IsAdmin() {
		return environments.All(gc.Request.Context())
	}
	return environments.ListByOwner(gc.Request.Context(), token.GetUserId())
}

// mayAccess is the single place that decides access; owner based for now,
// permissions-v2 sharing replaces the body without touching a handler.
func mayAccess(token sc_jwt.Token, env domain.Environment) bool {
	return env.Owner == token.GetUserId() || token.IsAdmin()
}

// accessibleEnvironment reads one environment and answers the access question in
// one step, writing the response itself when the answer is no. ok is false when
// the handler must return without doing anything else.
//
// Missing and forbidden are both 404, as everywhere else here: existence is not
// information for a caller without access.
func accessibleEnvironment(gc *gin.Context, environments repo.Environments, token sc_jwt.Token, id string) (domain.Environment, bool) {
	env, err := environments.Get(gc.Request.Context(), id)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		gc.String(http.StatusNotFound, "not found")
		return env, false
	case err != nil:
		util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
		gc.String(http.StatusInternalServerError, "unable to read environment")
		return env, false
	}
	if !mayAccess(token, env) {
		gc.String(http.StatusNotFound, "not found")
		return env, false
	}
	return env, true
}

// writeValidationError returns every problem with its path, so a caller can
// mark the offending fields.
func writeValidationError(gc *gin.Context, err error) {
	var invalid *domain.ValidationError
	if !errors.As(err, &invalid) {
		gc.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	gc.JSON(http.StatusBadRequest, invalid)
}
