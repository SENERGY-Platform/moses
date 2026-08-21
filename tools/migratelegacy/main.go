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

// Command migratelegacy converts the legacy worlds of this service into
// environments of the new domain model.
//
// A one-shot migration against production data, so a dry run unless -apply is
// given. It never deletes or modifies a legacy world, and never overwrites an
// existing environment, so re-running it is safe.
//
// Usage:
//
//	migratelegacy [-config config.json] [-type industrial_site] [-world <id>] [-apply]
//
// Exit codes: 0 the plan is clean or the apply fully succeeded, 1 at least one
// world could not be written (an invalid document or a failed write), 2 the
// migration could not be run at all (config, database, usage).
//
// The report on stdout is the interface of this tool, only failures go to
// stderr. The unmapped change routines it lists are the expected finding and the
// work list: their javascript is not in the converted document.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/migration"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/state"
)

const (
	exitClean   = 0
	exitProblem = 1
	exitBroken  = 2
)

// migrationTimeout bounds the whole run. The stores apply their own per
// operation deadlines; this one exists so that a hanging migration fails instead
// of sitting on an operator's terminal forever.
const migrationTimeout = 30 * time.Minute

type options struct {
	configLocation string
	// worldId restricts the run to one legacy world. Empty means all of them.
	worldId string
	apply   bool
	envType domain.EnvironmentType
}

// legacyStore is the part of state.PersistenceInterface this tool uses. Narrowed
// to one method on purpose: a migration must not be able to write or delete a
// legacy world by accident, and the compiler is a better guarantee of that than
// a comment.
type legacyStore interface {
	LoadWorlds() (map[string]*state.World, error)
}

func main() {
	opts, err := parseOptions(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		//the usage was already printed by the FlagSet; repeating it as an error
		//would suggest something went wrong. it still exits non-zero, because
		//nothing was migrated and 0 is reserved for a clean run.
		os.Exit(exitBroken)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "migratelegacy: %v\n", err)
		os.Exit(exitBroken)
	}
	code, err := run(os.Stdout, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migratelegacy: %v\n", err)
		os.Exit(exitBroken)
	}
	os.Exit(code)
}

// parseOptions uses its own FlagSet rather than the global one, so that it can
// be tested and so that config.LoadConfigFlag's flag.Parse() - which the service
// binary uses - is not involved here.
func parseOptions(args []string, errOut io.Writer) (options, error) {
	flags := flag.NewFlagSet("migratelegacy", flag.ContinueOnError)
	flags.SetOutput(errOut)
	configLocation := flags.String("config", "config.json", "configuration file, the same one the service reads")
	worldId := flags.String("world", "", "migrate only the legacy world with this id, instead of all of them")
	apply := flags.Bool("apply", false, "write the planned environments; without this flag nothing is written")
	envType := flags.String("type", string(domain.IndustrialSite), "environment type of the converted environments")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	// a leftover argument is almost always a mistyped flag ("-apply true" leaves
	// "true" here and would otherwise be ignored, and "-world" without a value
	// followed by an id would too)
	if flags.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q, flags are given as -flag=value", flags.Arg(0))
	}
	result := options{
		configLocation: strings.TrimSpace(*configLocation),
		worldId:        strings.TrimSpace(*worldId),
		apply:          *apply,
		envType:        domain.EnvironmentType(strings.TrimSpace(*envType)),
	}
	if result.configLocation == "" {
		return options{}, errors.New("-config must name a configuration file")
	}
	if err := migration.ValidateEnvironmentType(result.envType); err != nil {
		return options{}, fmt.Errorf("invalid -type: %w", err)
	}
	return result, nil
}

// run opens both stores and migrates. The two stores are separate connections to
// the same database on purpose: they are separate collections and the legacy one
// is opened read only by this tool (ref legacyStore).
func run(out io.Writer, opts options) (int, error) {
	conf, err := config.LoadConfigLocation(opts.configLocation)
	if err != nil {
		return exitBroken, fmt.Errorf("unable to load the config from %v: %w", opts.configLocation, err)
	}

	legacy, err := state.NewMongoPersistence(conf)
	if err != nil {
		return exitBroken, fmt.Errorf("unable to open the legacy store: %w", err)
	}
	defer legacy.Close()

	environments, err := repo.NewMongo(conf)
	if err != nil {
		return exitBroken, fmt.Errorf("unable to open the environment store: %w", err)
	}
	defer environments.Close()

	ctx, cancel := context.WithTimeout(context.Background(), migrationTimeout)
	defer cancel()
	return migrate(ctx, out, legacy, environments, opts)
}

// migrate is the whole migration, with the stores handed in. The binary and the
// integration test call exactly this function, so that the test exercises the
// code path the operator runs rather than a copy of it.
func migrate(ctx context.Context, out io.Writer, legacy legacyStore, environments repo.Environments, opts options) (int, error) {
	worlds, err := legacy.LoadWorlds()
	if err != nil {
		return exitBroken, fmt.Errorf("unable to load the legacy worlds: %w", err)
	}
	worlds, err = selectWorld(worlds, opts.worldId)
	if err != nil {
		return exitBroken, err
	}

	existing, err := existingIds(ctx, environments, worlds)
	if err != nil {
		return exitBroken, err
	}

	plans := migration.Plan(worlds, opts.envType, existing)
	results := make([]worldResult, 0, len(plans))
	for _, plan := range plans {
		results = append(results, execute(ctx, environments, plan, opts.apply))
	}

	report(out, reportHeader{apply: opts.apply, envType: opts.envType, worldFilter: opts.worldId}, results)
	return exitCode(results), nil
}

// selectWorld applies -world. An id that matches nothing is a usage error and
// not an empty run: the alternative is a report that says "0 worlds" and looks
// like a finished migration.
func selectWorld(worlds map[string]*state.World, worldId string) (map[string]*state.World, error) {
	if worldId == "" {
		return worlds, nil
	}
	result := map[string]*state.World{}
	for key, world := range worlds {
		if strings.TrimSpace(key) == worldId || (world != nil && strings.TrimSpace(world.Id) == worldId) {
			result[key] = world
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no legacy world with the id %q exists, %d worlds were loaded", worldId, len(worlds))
	}
	return result, nil
}

// existingIds asks the environment store for every id the plan could write.
//
// It asks per id instead of listing everything, because Environments.All() skips
// a document it cannot decode: an environment that exists but is unreadable
// would then be reported as absent and overwritten by the apply, which is the
// one outcome this tool must never produce. Get() returns the decode error
// instead, and that error aborts the run.
func existingIds(ctx context.Context, environments repo.Environments, worlds map[string]*state.World) (map[string]bool, error) {
	result := map[string]bool{}
	for _, id := range migration.CandidateIds(worlds) {
		_, err := environments.Get(ctx, id)
		switch {
		case err == nil:
			result[id] = true
		case errors.Is(err, repo.ErrNotFound):
			// not migrated yet
		default:
			return nil, fmt.Errorf("unable to check whether the environment %v already exists: %w", id, err)
		}
	}
	return result, nil
}

// execute carries out one plan, or reports what it would do.
func execute(ctx context.Context, environments repo.Environments, plan migration.WorldPlan, apply bool) worldResult {
	result := worldResult{plan: plan}
	switch {
	case plan.Skip:
		result.action = actionWouldSkip
		if apply {
			result.action = actionSkipped
		}
	case plan.Err != nil:
		result.action = actionBlocked
	case !apply:
		result.action = actionWouldCreate
	default:
		result.action, result.note, result.writeErr = create(ctx, environments, plan)
	}
	return result
}

// create writes one environment.
//
// The Get() before the Put() is not a formality: Put() is an upsert, and the
// plan decided to write before the other worlds were processed. The store has no
// insert-only write, so this narrows the window rather than closing it -
// acceptable for an operator-run one-shot, not for a service.
func create(ctx context.Context, environments repo.Environments, plan migration.WorldPlan) (action, string, error) {
	_, err := environments.Get(ctx, plan.Environment.Id)
	if err == nil {
		return actionSkipped, fmt.Sprintf("an environment with the id %v appeared in the store after the plan was made and was not overwritten", plan.Environment.Id), nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return actionWriteFailed, "", fmt.Errorf("unable to check whether the environment already exists: %w", err)
	}
	if err := environments.Put(ctx, plan.Environment); err != nil {
		return actionWriteFailed, "", err
	}
	return actionCreated, "", nil
}

// exitCode maps the results onto the process exit code. A problem found by the
// conversion does not appear here: problems are the expected outcome of this
// migration, an unwritable document is not.
func exitCode(results []worldResult) int {
	for _, result := range results {
		if result.action == actionBlocked || result.action == actionWriteFailed {
			return exitProblem
		}
	}
	return exitClean
}
