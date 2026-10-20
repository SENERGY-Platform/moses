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
	"fmt"
	"net/http"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/graphs"
	"github.com/SENERGY-Platform/moses/lib/util"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
)

// reconcileGraphRef decides which graph the document about to be stored owns.
// Same rule and same reason as reconcileManagedFlags: the client sends the whole
// document back, so the ref it carries is worth nothing.
//
//   - An update of a stored document keeps the stored ref. A client that sends a
//     different one is trying to make this environment write into a graph it does
//     not own.
//   - Anything else starts without a ref and gets a fresh graph. The case this
//     is for is the export copied to a new id: its ref still points at the graph
//     of the original, and honouring it would have the copy overwrite the
//     original's graph on save and delete it on delete.
//
// The meter graph ref follows the same rule, so only a stored one survives for
// dropMeterGraph to clean up.
//
// previous is nil exactly when nothing is stored under this id - a create, or a
// put to an id that is new here.
func reconcileGraphRef(previous *domain.Environment, env *domain.Environment) {
	if previous == nil {
		env.ExternalGraphRef = ""
		env.ExternalMeterGraphRef = ""
		return
	}
	env.ExternalGraphRef = previous.ExternalGraphRef
	env.ExternalMeterGraphRef = previous.ExternalMeterGraphRef
}

// GraphMirror is what the api needs from the device-repository's graph api.
// Narrow because it is a boundary: two methods can be faked in a test, the full
// client cannot.
type GraphMirror interface {
	// SetGraph creates a graph when graph.Id is empty and replaces it otherwise.
	// The returned graph carries the id the repository assigned.
	SetGraph(token string, graph models.Graph) (models.Graph, error, int)
	DeleteGraph(token string, id string) (error, int)
}

// mirrorGraph writes the environment to the device-repository as a graph and
// records the id it got under. Called with the document as it will be stored,
// after provisioning, so the device references the graph needs are set.
//
// It runs BEFORE the write, unlike the device cleanup, and the difference is
// deliberate. The id of a graph that does not exist yet can only come from the
// repository - a self-invented id is refused, because a put to an unknown graph
// id is a permission check against a resource that has no permissions yet. So
// either the graph is written first and the document stores the ref it returned,
// or the document is written twice. Mirroring first costs a graph that outlives
// a failed write; writing twice costs a second write that can fail on its own
// and then leaks a graph nothing references. The first is the cheaper failure,
// and the same one the delete path already accepts.
//
// Best effort throughout: a repository that is down leaves a stale or missing
// graph and a warning, a panic included, and the save succeeds. The mirror
// exists for other applications to read; refusing to store a simulation because
// a reader is unavailable would be the wrong trade.
//
// The graph is rebuilt in full every time, so it is a mirror and not a document:
// a node moved or renamed by hand in a graph editor does not survive the next
// save of the environment. A graph somebody deleted is created anew, see
// graphIsGone.
func mirrorGraph(ctx context.Context, mirror GraphMirror, permissions Permissions, token sc_jwt.Token, env *domain.Environment) {
	if mirror == nil {
		return
	}
	ref := env.ExternalGraphRef
	defer func() {
		if panicked := recover(); panicked != nil {
			util.Logger.Error("panic while mirroring an environment as a graph",
				"environment", env.Id, "graph", ref, "panic", fmt.Sprint(panicked))
			env.ExternalGraphRef = ref
		}
	}()
	graph := graphs.Build(*env)
	result, err, code := mirror.SetGraph(token.Jwt(), graph)
	if err != nil && graphIsGone(ctx, permissions, token, env.Id, ref, code) {
		graph.Id = ""
		result, err, code = mirror.SetGraph(token.Jwt(), graph)
		if err == nil {
			util.Logger.Info("created a new graph for an environment whose graph was deleted",
				"environment", env.Id, "deleted_graph", ref, "graph", result.Id)
		}
	}
	if err != nil {
		util.Logger.Warn("unable to mirror an environment as a graph", attributes.ErrorKey, err,
			"environment", env.Id, "graph", ref, "status", code)
		return
	}
	// the repository assigns the id of a new graph; on an update it echoes the
	// one that was sent. An empty answer is not allowed to blank a ref that
	// worked - that would orphan the graph and create a second one next time.
	if result.Id != "" {
		env.ExternalGraphRef = result.Id
	}
}

// graphIsGone reports whether the stored graph a write was refused for has been
// deleted. The repository answers a write to a deleted graph with 403, which is
// also what a refused caller gets, so only permissions-v2 saying it stores no
// rights for the graph counts: the device-repository removes them on delete.
// Anything else, a failed check included, is not proof and keeps the ref.
func graphIsGone(ctx context.Context, permissions Permissions, token sc_jwt.Token, environmentId string, ref string, code int) bool {
	if ref == "" || code != http.StatusForbidden || permissions == nil {
		return false
	}
	_, err, status := permissions.GetResource(ctx, token.Jwt(), graphsTopic, ref)
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnknownResource) {
		return true
	}
	util.Logger.Warn("unable to tell whether the graph of an environment still exists", attributes.ErrorKey, err,
		"environment", environmentId, "graph", ref, "status", status)
	return false
}

// locationGraphKind and meterGraphKind name the graphs in the log.
const (
	locationGraphKind = "location"
	meterGraphKind    = "meter"
)

// dropMeterGraph deletes a meter graph that an earlier version wrote and the
// stored document still names. It runs before the write, so the document is
// stored without the ref; a failed delete keeps the ref for the next save and
// never fails the request.
func dropMeterGraph(mirror GraphMirror, token sc_jwt.Token, env *domain.Environment) {
	if mirror == nil || env.ExternalMeterGraphRef == "" {
		return
	}
	if deleteOneGraph(mirror, token, env.Id, meterGraphKind, env.ExternalMeterGraphRef) {
		env.ExternalMeterGraphRef = ""
	}
}

// deleteGraph removes the mirror of a deleted environment, and a meter graph it
// still names. After the delete of the document, for the same reason
// deleteDevices runs there: a failed delete leaves the environment, and it keeps
// its graph.
//
// Best effort as well, and per graph. What a failure leaves behind is a graph
// without an environment, which is recoverable by hand - the opposite, a delete
// that fails over an unreachable reader, is not something a caller can do
// anything about.
func deleteGraph(mirror GraphMirror, token sc_jwt.Token, env *domain.Environment) {
	if mirror == nil {
		return
	}
	deleteOneGraph(mirror, token, env.Id, locationGraphKind, env.ExternalGraphRef)
	deleteOneGraph(mirror, token, env.Id, meterGraphKind, env.ExternalMeterGraphRef)
}

// deleteOneGraph reports whether the graph is gone afterwards; an empty ref
// names none and is gone already.
func deleteOneGraph(mirror GraphMirror, token sc_jwt.Token, environmentId string, kind string, ref string) bool {
	if ref == "" {
		return true
	}
	//a graph that is already gone is what the caller wanted. The repository
	//answers a delete of an unknown graph with a success of its own, so this
	//covers the graph somebody removed by hand and a retry after a partial
	//cleanup
	if err, code := mirror.DeleteGraph(token.Jwt(), ref); err != nil && code != http.StatusNotFound {
		util.Logger.Warn("unable to delete a graph of an environment", attributes.ErrorKey, err,
			"environment", environmentId, "kind", kind, "graph", ref, "status", code)
		return false
	}
	util.Logger.Info("deleted a graph of an environment", "environment", environmentId, "kind", kind, "graph", ref)
	return true
}
