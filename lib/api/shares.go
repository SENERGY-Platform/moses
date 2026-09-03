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
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
	permModel "github.com/SENERGY-Platform/permissions-v2/pkg/model"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"
)

func init() {
	environmentEndpoints = append(environmentEndpoints, ShareEndpoints)
}

// devicesTopic and graphsTopic are the permissions-v2 topics the platform keeps
// the rights of a device and of a graph under. graphsTopic is the
// device-repository's GraphTopic.
const devicesTopic = "devices"
const graphsTopic = "graphs"

// shareKindDevice and shareKindGraph say which of the two a failure was about,
// because the fix differs: a device is one asset, the graph is the whole view.
const shareKindDevice = "device"
const shareKindGraph = "graph"

// maxShares bounds one set, maxPrincipalRunes one entry of it. The rights are
// applied device by device, so a set nobody meant to send would turn one call
// into thousands of requests.
const maxShares = 100
const maxPrincipalRunes = 256

// maxShareBytes bounds the request body, as the dataset upload does: the set is
// small, and a body that is not has no business being parsed.
const maxShareBytes = 64 * 1024

// shareDeadline bounds the whole application. It is under the api's write
// timeout of ten seconds, so a share of a very large environment reports what it
// did not reach instead of running past the answer. A variable so a test does
// not have to wait it out.
var shareDeadline = 8 * time.Second

// shareWorkers is how many resources are worked on at once. The api's write
// timeout is ten seconds and a share touches every device twice, so thirty
// devices sequentially would be cutting it close on a slow permissions-v2.
const shareWorkers = 8

// Permissions is what the api needs from permissions-v2: read the rights of one
// resource and write them back. Implemented in lib/permissions.go rather than by
// the module's client, which does not pass the context on to the request.
type Permissions interface {
	GetResource(ctx context.Context, token string, topicId string, id string) (permModel.Resource, error, int)
	SetPermission(ctx context.Context, token string, topicId string, id string, rights permModel.ResourcePermissions) (permModel.ResourcePermissions, error, int)
}

// ShareEndpoints give the devices of one environment to other accounts. See
// docs/sharing.md.
func ShareEndpoints(config config.Config, environments repo.Environments, shares repo.Shares, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, permissions Permissions, router gin.IRouter) {
	for _, route := range []func(repo.Environments, repo.Shares, Permissions) (string, string, gin.HandlerFunc){
		getSharesH,
		putSharesH,
	} {
		method, path, handler := route(environments, shares, permissions)
		router.Handle(method, path, handler)
	}
}

// ShareTargets are the accounts a share names: user ids as keycloak issues them
// and group paths with a leading slash.
type ShareTargets struct {
	Users  []string `json:"users"`
	Groups []string `json:"groups"`
}

// SharesResponse is the stored set together with the number of devices it acts
// on, which is the only feedback a caller gets that a share reached anything.
type SharesResponse struct {
	ShareTargets
	Devices int `json:"devices" example:"32"`

	// Graph says whether the graph this environment is mirrored as is shared
	// along with the devices. False for an environment that has none, which is
	// one whose mirror never succeeded.
	Graph bool `json:"graph" example:"true"`
}

// ShareFailure names one resource the share could not be applied to. The message
// is the one permissions-v2 gave, so a caller can tell a right they do not have
// from an unreachable service.
type ShareFailure struct {
	Id string `json:"id"`
	// Kind is "device" or "graph".
	Kind string `json:"kind" example:"device"`
	// Status is what permissions-v2 answered, or 0 where it could not be
	// reached at all. It is what decides whether the call as a whole is the
	// caller's fault.
	Status int    `json:"status" example:"400"`
	Error  string `json:"error"`
}

// ShareFailures is the body of the 502: which resources failed and why. The
// field is called devices because that is what it is almost always about; the
// graph appears in the same list with kind "graph".
type ShareFailures struct {
	Devices []ShareFailure `json:"devices"`
}

// shareResource is one permissions-v2 resource a share acts on: a device moses
// created for an asset, or the graph the environment is mirrored as. The two go
// through the same merge, they only differ in topic.
type shareResource struct {
	kind    string
	topicId string
	id      string
	// assetId is carried for the log line only, and is empty for the graph.
	assetId string
}

// shareResourcesOf is everything a share of this environment reaches: its
// managed devices in document order, and its graph last. An environment whose
// mirror never succeeded carries no ref and contributes no graph.
func shareResourcesOf(env *domain.Environment) []shareResource {
	result := []shareResource{}
	for _, device := range managedDevicesOf(env) {
		result = append(result, deviceResource(device))
	}
	if env.ExternalGraphRef != "" {
		result = append(result, graphResource(env.ExternalGraphRef))
	}
	return result
}

func deviceResource(device managedDevice) shareResource {
	return shareResource{kind: shareKindDevice, topicId: devicesTopic, id: device.deviceId, assetId: device.assetId}
}

func graphResource(ref string) shareResource {
	return shareResource{kind: shareKindGraph, topicId: graphsTopic, id: ref}
}

// newlySharedResources is what a save has to hand the stored set to: the devices
// it created, and the graph only where the save created one rather than
// rewriting the graph the environment already had - that one carries the set
// since it was shared.
func newlySharedResources(created []managedDevice, graphBefore string, graphAfter string) []shareResource {
	result := []shareResource{}
	for _, device := range created {
		result = append(result, deviceResource(device))
	}
	if graphAfter != "" && graphAfter != graphBefore {
		result = append(result, graphResource(graphAfter))
	}
	return result
}

// @Summary The accounts the devices of one environment are shared with
// @Description The stored set, plus the number of managed devices it acts on and whether the graph of this environment is shared along with them. Only devices moses created for the assets of this environment count; a device attached by the caller is never shared, because moses does not own it.
// @Description
// @Description After a failed share the set stands at the union of what was stored and what was asked for, which is what the next call needs to withdraw the devices that did go through.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 200 {object} SharesResponse
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, or no access to it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/shares [get]
func getSharesH(environments repo.Environments, shares repo.Shares, permissions Permissions) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments/:id/shares", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env, ok := accessibleEnvironment(gc, environments, token, gc.Param("id"))
		if !ok {
			return
		}
		stored, ok := loadShares(gc, shares, env.Id)
		if !ok {
			return
		}
		gc.JSON(http.StatusOK, SharesResponse{
			ShareTargets: targetsOf(stored),
			Devices:      len(managedDevicesOf(&env)),
			Graph:        env.ExternalGraphRef != "",
		})
	}
}

// @Summary Share the devices of one environment with users and groups
// @Description Replaces the set: everyone named gets `read` and `execute` on every device moses created for this environment and on the graph it is mirrored as, everyone who was in the stored set and is not named any more loses their entry. The rights are fixed and the environment document itself is not shared — it stays with its owner and the platform administrators.
// @Description
// @Description A device attached to an asset by the caller is never touched, since moses does not own it. An environment whose graph was never mirrored has none to share and is not treated as an error. An entry carrying `administrate` is never changed or removed, which is what keeps the owner and the administrators out of the set. `write` an entry already had stays as it is while it is shared, and goes with the entry when the share is withdrawn.
// @Description
// @Description Applied resource by resource with the caller's own token, so the platform's own rule decides who may be named: a caller without the `admin` role may share with groups they are a member of and with users who share a group with them. Such a refusal comes back per resource, and when every failure of a call is one of them the answer is a `400` with that list; a `502` means at least one failure was not the caller's fault.
// @Description
// @Description Before anything is written, the union of the stored and the requested set is recorded; the requested set replaces it once every resource went through. If a resource fails, the union stands — so the next call, with any set, withdraws what the failed one managed to grant. The whole application is bounded at eight seconds: a very large environment can run out of it, reports the resources it did not reach as failures and needs a second call.
// @Description
// @Description Assets added to the environment later inherit the stored set when their device is created, so a share does not have to be renewed after an edit.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Param shares body ShareTargets true "the accounts to share with; users are keycloak user ids, groups are group paths with a leading slash"
// @Success 200 {object} SharesResponse
// @Failure 400 {object} ShareFailures "either the request itself is refused - unreadable or too large body, an empty user id, a group that is not a path, an entry beyond 256 characters, or a set that would carry more than 100 accounts - and then the answer is a plain message, or every resource refused the caller and then it is the list"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, or no access to it"
// @Failure 409 {string} string "another share of this environment was stored in between; read the set again and repeat"
// @Failure 500 {string} string "error message"
// @Failure 502 {object} ShareFailures "the resources the rights could not be written on, and why; `kind` says whether it was a device or the graph, `status` what permissions-v2 answered"
// @Router /environments/{id}/shares [put]
func putSharesH(environments repo.Environments, shares repo.Shares, permissions Permissions) (string, string, gin.HandlerFunc) {
	return http.MethodPut, "/environments/:id/shares", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		env, ok := accessibleEnvironment(gc, environments, token, id)
		if !ok {
			return
		}

		requested := ShareTargets{}
		gc.Request.Body = http.MaxBytesReader(gc.Writer, gc.Request.Body, maxShareBytes)
		if err := gc.ShouldBindJSON(&requested); err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as a share set (limit %d bytes): %s", maxShareBytes, err.Error())
			return
		}
		desired, err := normalizeShares(requested)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		if permissions == nil || shares == nil {
			//storing the set would claim a grant that was never made
			gc.String(http.StatusInternalServerError, "this instance runs without a permissions client and cannot share devices")
			return
		}

		stored, ok := loadShares(gc, shares, id)
		if !ok {
			return
		}
		union := unionOf(targetsOf(stored), desired)
		//a set that only shrinks is never refused over the limit: what is over it
		//can only be leftovers of failed attempts, and this is how they go away
		if count := len(union.Users) + len(union.Groups); count > maxShares && !sameTargets(union, targetsOf(stored)) {
			gc.String(http.StatusBadRequest,
				"this environment is already recorded as shared with %d accounts and the request would make it %d, more than the %d allowed. Entries an earlier failed attempt left behind are among them; send a smaller set, or an empty one, to remove them",
				len(stored.Users)+len(stored.Groups), count, maxShares)
			return
		}

		//The union goes in FIRST, and with the compare-and-swap: it is the record
		//of everybody who may end up with rights below, and nothing is granted
		//before it is written. A second share arriving at the same time loses the
		//swap here, before it touched a single resource.
		pending := setOf(id, union)
		pending.Version = stored.Version
		version, err := shares.Save(gc.Request.Context(), pending)
		if err != nil {
			if writeShareConflict(gc, id, err) {
				return
			}
			util.Logger.Error("unable to store the pending share set", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to store the share set")
			return
		}

		resources := shareResourcesOf(&env)
		failures := applyShares(gc.Request.Context(), permissions, token, resources, desired, withoutTargets(union, desired))
		if len(failures) > 0 {
			util.Logger.Error("unable to apply a share to every resource of an environment",
				"environment", id, "resources", len(resources), "failed", len(failures))
			//the union stands, so every right that was written is recorded
			gc.JSON(shareFailureStatus(failures), ShareFailures{Devices: failures})
			return
		}

		final := setOf(id, desired)
		final.Version = version
		if _, err = shares.Save(gc.Request.Context(), final); err != nil {
			if writeShareConflict(gc, id, err) {
				return
			}
			util.Logger.Error("unable to store the share set of an environment", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to store the share set")
			return
		}
		gc.JSON(http.StatusOK, SharesResponse{
			ShareTargets: desired,
			Devices:      len(managedDevicesOf(&env)),
			Graph:        env.ExternalGraphRef != "",
		})
	}
}

// writeShareConflict answers a lost compare-and-swap and reports whether it took
// the response. Rights this call had already written are covered by the winner's
// union, which was read after this call's own union was stored.
func writeShareConflict(gc *gin.Context, environmentId string, err error) bool {
	if !errors.Is(err, repo.ErrVersionConflict) {
		return false
	}
	util.Logger.Warn("a concurrent share of the same environment won", attributes.ErrorKey, err, "environment", environmentId)
	gc.String(http.StatusConflict, "the share set of this environment changed meanwhile, read it again and repeat")
	return true
}

// shareFailureStatus tells the caller's fault from the platform's. Only the
// answers that mean "you may not do this" become a 400; a 404 from
// permissions-v2 is a device it does not know yet, which is worth retrying and
// not worth correcting.
func shareFailureStatus(failures []ShareFailure) int {
	for _, failure := range failures {
		switch failure.Status {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		default:
			return http.StatusBadGateway
		}
	}
	return http.StatusBadRequest
}

// loadShares reads the stored set and writes the response itself when it cannot.
func loadShares(gc *gin.Context, shares repo.Shares, environmentId string) (repo.ShareSet, bool) {
	if shares == nil {
		//an empty set would claim this environment is shared with nobody, which
		//is not something this instance can know
		gc.String(http.StatusInternalServerError, "this instance runs without a share store")
		return repo.ShareSet{}, false
	}
	stored, err := shares.Load(gc.Request.Context(), environmentId)
	if err != nil {
		util.Logger.Error("unable to read the share set of an environment", attributes.ErrorKey, err, "environment", environmentId)
		gc.String(http.StatusInternalServerError, "unable to read the share set")
		return stored, false
	}
	return stored, true
}

// targetsOf is the stored record as the api serves it: empty lists rather than
// null, or a client has two cases for "nobody".
func targetsOf(stored repo.ShareSet) ShareTargets {
	result := ShareTargets{Users: stored.Users, Groups: stored.Groups}
	if result.Users == nil {
		result.Users = []string{}
	}
	if result.Groups == nil {
		result.Groups = []string{}
	}
	return result
}

func setOf(environmentId string, targets ShareTargets) repo.ShareSet {
	return repo.ShareSet{EnvironmentId: environmentId, Users: targets.Users, Groups: targets.Groups}
}

// normalizeShares turns the request into the set that is applied and stored:
// trimmed, deduplicated and checked. Every problem is reported at once, so a
// caller does not need a second round trip per bad entry.
//
// A group is a keycloak group path and therefore starts with a slash; the bare
// slash is the root and names no group.
func normalizeShares(in ShareTargets) (ShareTargets, error) {
	problems := []string{}
	users := []string{}
	seen := map[string]bool{}
	for _, user := range in.Users {
		user = strings.TrimSpace(user)
		if user == "" {
			problems = append(problems, "a user id must not be empty")
			continue
		}
		if utf8.RuneCountInString(user) > maxPrincipalRunes {
			problems = append(problems, fmt.Sprintf("a user id is at most %d characters", maxPrincipalRunes))
			continue
		}
		if seen[user] {
			continue
		}
		seen[user] = true
		users = append(users, user)
	}
	groups := []string{}
	seen = map[string]bool{}
	for _, group := range in.Groups {
		group = strings.TrimSpace(group)
		if !strings.HasPrefix(group, "/") || group == "/" {
			problems = append(problems, fmt.Sprintf("a group is a path with a leading slash, got %q", group))
			continue
		}
		if utf8.RuneCountInString(group) > maxPrincipalRunes {
			problems = append(problems, fmt.Sprintf("a group path is at most %d characters", maxPrincipalRunes))
			continue
		}
		if seen[group] {
			continue
		}
		seen[group] = true
		groups = append(groups, group)
	}
	if len(users)+len(groups) > maxShares {
		problems = append(problems, fmt.Sprintf("a share set holds at most %d users and groups together, got %d", maxShares, len(users)+len(groups)))
	}
	if len(problems) > 0 {
		return ShareTargets{}, errors.New(strings.Join(problems, "; "))
	}
	return ShareTargets{Users: users, Groups: groups}, nil
}

// unionOf is what has to be recorded before anything is granted: everybody who
// may end up with rights on a device of this environment.
func unionOf(stored ShareTargets, desired ShareTargets) ShareTargets {
	return ShareTargets{
		Users:  merged(stored.Users, desired.Users),
		Groups: merged(stored.Groups, desired.Groups),
	}
}

func merged(first []string, second []string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, entry := range append(append([]string{}, first...), second...) {
		if seen[entry] {
			continue
		}
		seen[entry] = true
		result = append(result, entry)
	}
	return result
}

// withoutTargets is the set difference that decides what a share withdraws.
func withoutTargets(from ShareTargets, remove ShareTargets) ShareTargets {
	return ShareTargets{
		Users:  missing(from.Users, remove.Users),
		Groups: missing(from.Groups, remove.Groups),
	}
}

func missing(from []string, remove []string) []string {
	dropped := make(map[string]bool, len(remove))
	for _, entry := range remove {
		dropped[entry] = true
	}
	result := []string{}
	for _, entry := range from {
		if !dropped[entry] {
			result = append(result, entry)
		}
	}
	return result
}

// sameTargets compares as sets: both sides come out of normalizeShares or the
// store, so order carries no meaning.
func sameTargets(first ShareTargets, second ShareTargets) bool {
	return sameEntries(first.Users, second.Users) && sameEntries(first.Groups, second.Groups)
}

func sameEntries(first []string, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	known := make(map[string]bool, len(first))
	for _, entry := range first {
		known[entry] = true
	}
	for _, entry := range second {
		if !known[entry] {
			return false
		}
	}
	return true
}

// applyShares writes the rights of every resource and collects what failed
// rather than stopping at the first one, because one refused group and an
// unreachable platform are different problems. The resources are worked on
// concurrently - a share of thirty devices is sixty round trips - and each worker
// writes its own slot, so the report keeps document order and needs no lock.
func applyShares(ctx context.Context, permissions Permissions, token sc_jwt.Token, resources []shareResource, add ShareTargets, remove ShareTargets) []ShareFailure {
	//one deadline over all of them, under the api's write timeout: a
	//permissions-v2 that answers slowly must end as a report and not as a
	//request that is still running when the server gives up on it
	ctx, cancel := context.WithTimeout(ctx, shareDeadline)
	defer cancel()

	results := make([]*ShareFailure, len(resources))
	work := make(chan int)
	waiting := sync.WaitGroup{}
	workers := shareWorkers
	if len(resources) < workers {
		workers = len(resources)
	}
	for worker := 0; worker < workers; worker++ {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for index := range work {
				results[index] = shareOneResource(ctx, permissions, token, resources[index], add, remove)
			}
		}()
	}
	for index := range resources {
		work <- index
	}
	close(work)
	waiting.Wait()

	failures := []ShareFailure{}
	for _, result := range results {
		if result != nil {
			failures = append(failures, *result)
		}
	}
	return failures
}

// shareOneResource is applyResourceShares plus the failure report. The recover is
// what keeps a panic on this worker goroutine, which gin's recovery does not see,
// from taking the process down over one resource.
func shareOneResource(ctx context.Context, permissions Permissions, token sc_jwt.Token, resource shareResource, add ShareTargets, remove ShareTargets) (failure *ShareFailure) {
	defer func() {
		if panicked := recover(); panicked != nil {
			util.Logger.Error("panic while sharing a resource of an environment",
				"kind", resource.kind, "resource", resource.id, "panic", fmt.Sprint(panicked))
			failure = &ShareFailure{Id: resource.id, Kind: resource.kind,
				Error: "internal error while writing the rights"}
		}
	}()
	status, err := applyResourceShares(ctx, permissions, token, resource, add, remove)
	if err != nil {
		util.Logger.Warn("unable to share a resource of an environment", attributes.ErrorKey, err,
			"kind", resource.kind, "asset", resource.assetId, "resource", resource.id)
		return &ShareFailure{Id: resource.id, Kind: resource.kind, Status: status, Error: err.Error()}
	}
	return nil
}

// applyResourceShares moves one resource to the wanted rights. The read before
// the write is mandatory: SetPermission replaces the whole rights object, so
// writing a set that was not read would take the owner's own administrate away.
func applyResourceShares(ctx context.Context, permissions Permissions, token sc_jwt.Token, target shareResource, add ShareTargets, remove ShareTargets) (int, error) {
	resource, err, code := permissions.GetResource(ctx, token.Jwt(), target.topicId, target.id)
	if err != nil {
		return code, fmt.Errorf("unable to read the rights of the %s (status %d): %w", target.kind, code, err)
	}
	rights := resource.ResourcePermissions
	rights.UserPermissions = grant(rights.UserPermissions, add.Users)
	rights.GroupPermissions = grant(rights.GroupPermissions, add.Groups)
	rights.UserPermissions = revoke(rights.UserPermissions, remove.Users)
	rights.GroupPermissions = revoke(rights.GroupPermissions, remove.Groups)
	if _, err, code = permissions.SetPermission(ctx, token.Jwt(), target.topicId, target.id, rights); err != nil {
		return code, fmt.Errorf("unable to write the rights of the %s (status %d): %w", target.kind, code, err)
	}
	return code, nil
}

// grant adds read and execute and leaves write and administrate as they are: a
// share must not narrow what an entry already carries.
func grant(entries map[string]permModel.PermissionsMap, principals []string) map[string]permModel.PermissionsMap {
	if len(principals) == 0 {
		return entries
	}
	if entries == nil {
		entries = map[string]permModel.PermissionsMap{}
	}
	for _, principal := range principals {
		rights := entries[principal]
		rights.Read = true
		rights.Execute = true
		entries[principal] = rights
	}
	return entries
}

// revoke drops the entry of a principal the share no longer names, unless it
// carries administrate: that is the owner or an administrator, and a withdrawal
// that locked them out of their own device could not be undone through this api.
func revoke(entries map[string]permModel.PermissionsMap, principals []string) map[string]permModel.PermissionsMap {
	for _, principal := range principals {
		if entries[principal].Administrate {
			continue
		}
		delete(entries, principal)
	}
	return entries
}

// inheritShares gives what a save has just created - the devices of new assets,
// and a graph that was created rather than replaced - the stored share set.
//
// Best effort per resource: the rights of a device reach permissions-v2
// asynchronously, so one created a moment ago may not be known there yet, and
// the next PUT on /shares repairs it.
func inheritShares(ctx context.Context, shares repo.Shares, permissions Permissions, token sc_jwt.Token, env *domain.Environment, created []shareResource) {
	if permissions == nil || shares == nil || len(created) == 0 {
		return
	}
	stored, err := shares.Load(ctx, env.Id)
	if err != nil {
		util.Logger.Warn("unable to read the share set for a new resource", attributes.ErrorKey, err, "environment", env.Id)
		return
	}
	if stored.Empty() {
		return
	}
	for _, failure := range applyShares(ctx, permissions, token, created, targetsOf(stored), ShareTargets{}) {
		util.Logger.Warn("unable to give a new resource the share set of its environment",
			"environment", env.Id, "kind", failure.Kind, "resource", failure.Id, "reason", failure.Error)
	}
}

// clearShares drops the set of an id that is being deleted or taken over by a new
// environment, so an id used again does not come back shared with the accounts of
// the environment that is gone. Best effort: a set without an environment grants
// nothing by itself.
func clearShares(ctx context.Context, shares repo.Shares, environmentId string) {
	if shares == nil {
		return
	}
	if err := shares.Delete(ctx, environmentId); err != nil {
		util.Logger.Warn("unable to delete the share set of an environment", attributes.ErrorKey, err, "environment", environmentId)
	}
}
