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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/migration"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/state"
)

// fakeEnvironments is an in memory Environments. It counts the writes, so that a
// dry run can be proven to write nothing, and it can fail or mutate on a chosen
// call, which is how the race between the plan and the write is tested.
type fakeEnvironments struct {
	stored map[string]domain.Environment
	puts   int
	gets   int
	// getErr is returned by Get instead of a not-found, for the case of a stored
	// document that cannot be read.
	getErr error
	// putErr makes the write of this id fail.
	putErr map[string]error
	// beforeGet runs before each Get and receives the call number, starting at 1.
	beforeGet func(call int, id string)
	// afterGet runs after the lookup, so that a test can let a document appear
	// between one Get and the next without changing that Get's answer.
	afterGet func(call int, id string)
}

var _ repo.Environments = &fakeEnvironments{}

func newFakeEnvironments(stored ...domain.Environment) *fakeEnvironments {
	result := &fakeEnvironments{stored: map[string]domain.Environment{}, putErr: map[string]error{}}
	for _, env := range stored {
		result.stored[env.Id] = env
	}
	return result
}

func (this *fakeEnvironments) Put(ctx context.Context, env domain.Environment) (int64, error) {
	this.puts++
	if err := this.putErr[env.Id]; err != nil {
		return 0, err
	}
	env.Version = this.stored[env.Id].Version + 1
	this.stored[env.Id] = env
	return env.Version, nil
}

// PutIfVersion is here for the interface; the migration only ever creates.
func (this *fakeEnvironments) PutIfVersion(ctx context.Context, env domain.Environment, expectedVersion int64) (int64, error) {
	return 0, errors.New("the migration must never write against a version")
}

func (this *fakeEnvironments) Get(ctx context.Context, id string) (domain.Environment, error) {
	this.gets++
	call := this.gets
	if this.beforeGet != nil {
		this.beforeGet(call, id)
	}
	if this.afterGet != nil {
		defer this.afterGet(call, id)
	}
	if this.getErr != nil {
		return domain.Environment{}, this.getErr
	}
	env, ok := this.stored[id]
	if !ok {
		return domain.Environment{}, fmt.Errorf("%w: %v", repo.ErrNotFound, id)
	}
	return env, nil
}

func (this *fakeEnvironments) ListByOwner(ctx context.Context, owner string) ([]domain.Environment, error) {
	return nil, errors.New("not used by the migration")
}

func (this *fakeEnvironments) All(ctx context.Context) ([]domain.Environment, error) {
	return nil, errors.New("not used by the migration")
}

func (this *fakeEnvironments) Delete(ctx context.Context, id string) error {
	return errors.New("the migration must never delete an environment")
}

// fakeLegacy is the read only legacy store the migration sees.
type fakeLegacy struct {
	worlds map[string]*state.World
	err    error
	loads  int
}

func (this *fakeLegacy) LoadWorlds() (map[string]*state.World, error) {
	this.loads++
	return this.worlds, this.err
}

func testWorld(id string, name string) *state.World {
	return &state.World{
		Id:     id,
		Name:   name,
		Owner:  "owner-1",
		States: map[string]interface{}{"outside": 12.5},
		ChangeRoutines: map[string]state.ChangeRoutine{
			"world-routine": {Id: "world-routine", Interval: 60, Code: "moses.world.state.set('outside', 1);"},
		},
		Rooms: map[string]*state.Room{
			"room-1": {
				Id:   "room-1",
				Name: "Halle",
				Devices: map[string]*state.Device{
					"device-1": {
						Id:             "device-1",
						Name:           "Kompressor",
						ExternalRef:    "urn:infai:ses:device:7283f08c",
						ExternalTypeId: "urn:infai:ses:device-type:dc5bf705",
						Services: map[string]state.Service{
							"service-1": {
								Id:             "service-1",
								Name:           "getTemperature",
								ExternalRef:    "urn:infai:ses:service:38657ee1",
								SensorInterval: 30,
								Code:           "moses.service.send(1);",
							},
						},
					},
				},
			},
		},
	}
}

func dryRunOptions() options {
	return options{configLocation: "config.json", envType: domain.IndustrialSite}
}

func applyOptions() options {
	result := dryRunOptions()
	result.apply = true
	return result
}

func runMigration(t *testing.T, legacy legacyStore, environments repo.Environments, opts options) (string, int) {
	t.Helper()
	out := &bytes.Buffer{}
	code, err := migrate(context.Background(), out, legacy, environments, opts)
	if err != nil {
		t.Fatalf("the migration reported a broken environment: %v\n%s", err, out.String())
	}
	return out.String(), code
}

func TestParseOptionsDefaultsToADryRunOfEveryWorld(t *testing.T) {
	opts, err := parseOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.apply {
		t.Error("the default has to be a dry run")
	}
	if opts.configLocation != "config.json" {
		t.Errorf("unexpected config location %q", opts.configLocation)
	}
	if opts.worldId != "" {
		t.Errorf("unexpected world filter %q", opts.worldId)
	}
	if opts.envType != domain.IndustrialSite {
		t.Errorf("unexpected environment type %q", opts.envType)
	}
}

func TestParseOptionsReadsEveryFlag(t *testing.T) {
	opts, err := parseOptions([]string{"-config=/etc/moses.json", "-world=world-1", "-apply", "-type=apartment"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := options{configLocation: "/etc/moses.json", worldId: "world-1", apply: true, envType: domain.Apartment}
	if !reflect.DeepEqual(opts, want) {
		t.Errorf("expected %+v, got %+v", want, opts)
	}
}

func TestParseOptionsAcceptsEveryEnvironmentTypeTheModelKnows(t *testing.T) {
	for _, envType := range []domain.EnvironmentType{
		domain.IndustrialSite, domain.OfficeBuilding, domain.ApartmentBuilding, domain.SingleFamilyHome, domain.Apartment,
	} {
		if _, err := parseOptions([]string{"-type=" + string(envType)}, &bytes.Buffer{}); err != nil {
			t.Errorf("%v was rejected: %v", envType, err)
		}
	}
}

func TestParseOptionsRejectsAnUnknownEnvironmentType(t *testing.T) {
	_, err := parseOptions([]string{"-type=space_station"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("an unknown type has to be rejected before anything is read")
	}
	if !strings.Contains(err.Error(), "space_station") || !strings.Contains(err.Error(), string(domain.IndustrialSite)) {
		t.Errorf("the error has to name the rejected value and the accepted ones, got %v", err)
	}
}

func TestParseOptionsRejectsAnEmptyEnvironmentTypeAndConfig(t *testing.T) {
	if _, err := parseOptions([]string{"-type="}, &bytes.Buffer{}); err == nil {
		t.Error("an empty type has to be rejected")
	}
	if _, err := parseOptions([]string{"-config="}, &bytes.Buffer{}); err == nil {
		t.Error("an empty config location has to be rejected")
	}
	if _, err := parseOptions([]string{"-config=   "}, &bytes.Buffer{}); err == nil {
		t.Error("a whitespace only config location has to be rejected")
	}
}

// "-apply true" is the mistake a boolean flag invites: flag sets apply and leaves
// "true" as a positional argument. Ignoring it would be harmless here, but the
// same mistake with "-world 4d27..." would silently migrate every world.
func TestParseOptionsRejectsALeftoverArgument(t *testing.T) {
	_, err := parseOptions([]string{"-apply", "true"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a leftover argument has to be rejected")
	}
	if !strings.Contains(err.Error(), "true") {
		t.Errorf("the error has to quote the argument, got %v", err)
	}
	if _, err := parseOptions([]string{"-world", "world-1", "extra"}, &bytes.Buffer{}); err == nil {
		t.Error("a leftover argument after a value flag has to be rejected")
	}
}

func TestParseOptionsTrimsItsValues(t *testing.T) {
	opts, err := parseOptions([]string{"-world= world-1 ", "-type= industrial_site ", "-config= config.json "}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.worldId != "world-1" || opts.envType != domain.IndustrialSite || opts.configLocation != "config.json" {
		t.Errorf("values were not trimmed: %+v", opts)
	}
}

func TestParseOptionsReportsAnUnknownFlagOnTheGivenWriter(t *testing.T) {
	errOut := &bytes.Buffer{}
	if _, err := parseOptions([]string{"-force"}, errOut); err == nil {
		t.Fatal("an unknown flag has to be rejected")
	}
	if !strings.Contains(errOut.String(), "-force") {
		t.Errorf("the usage message has to go to the given writer, got %q", errOut.String())
	}
}

func TestSelectWorldKeepsOnlyTheRequestedWorld(t *testing.T) {
	worlds := map[string]*state.World{"a": testWorld("a", "Alpha"), "b": testWorld("b", "Beta")}
	selected, err := selectWorld(worlds, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected["b"] == nil {
		t.Errorf("expected only b, got %v", selected)
	}
}

// the key and the id normally agree, but a document id that differs from its key
// has to be findable by the id an operator reads in the api
func TestSelectWorldMatchesTheDocumentIdAsWellAsTheKey(t *testing.T) {
	worlds := map[string]*state.World{"key": testWorld("document-id", "Alpha")}
	selected, err := selectWorld(worlds, "document-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 {
		t.Errorf("expected the world to be found by its document id, got %v", selected)
	}
}

func TestSelectWorldRejectsAnIdThatMatchesNothing(t *testing.T) {
	worlds := map[string]*state.World{"a": testWorld("a", "Alpha")}
	_, err := selectWorld(worlds, "does-not-exist")
	if err == nil {
		t.Fatal("an id that matches nothing has to be an error, not an empty run")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error has to name the id, got %v", err)
	}
}

func TestSelectWorldWithoutAFilterKeepsEverything(t *testing.T) {
	worlds := map[string]*state.World{"a": testWorld("a", "Alpha"), "b": testWorld("b", "Beta")}
	selected, err := selectWorld(worlds, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 {
		t.Errorf("expected both worlds, got %v", selected)
	}
}

func TestExistingIdsAsksForEveryCandidateId(t *testing.T) {
	environments := newFakeEnvironments(domain.Environment{Id: "b"})
	asked := []string{}
	environments.beforeGet = func(call int, id string) { asked = append(asked, id) }
	existing, err := existingIds(context.Background(), environments, map[string]*state.World{
		"a": testWorld("a", "Alpha"),
		"b": testWorld("b", "Beta"),
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(asked)
	if !reflect.DeepEqual(asked, []string{"a", "b"}) {
		t.Errorf("expected both ids to be checked, got %v", asked)
	}
	if !reflect.DeepEqual(existing, map[string]bool{"b": true}) {
		t.Errorf("unexpected existing ids %v", existing)
	}
}

// a stored environment that cannot be read must not be reported as absent: the
// apply would overwrite it. this is why the store is asked per id instead of
// through All(), which skips undecodable documents.
func TestExistingIdsAbortsWhenTheStoreCannotBeQueried(t *testing.T) {
	environments := newFakeEnvironments()
	environments.getErr = errors.New("cannot decode field zones")
	_, err := existingIds(context.Background(), environments, map[string]*state.World{"a": testWorld("a", "Alpha")})
	if err == nil {
		t.Fatal("an unreadable environment has to abort the migration")
	}
	if !strings.Contains(err.Error(), "cannot decode field zones") || !strings.Contains(err.Error(), "a") {
		t.Errorf("the error has to name the id and the cause, got %v", err)
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	environments := newFakeEnvironments()
	legacy := &fakeLegacy{worlds: map[string]*state.World{"a": testWorld("a", "Alpha"), "b": testWorld("b", "Beta")}}
	out, code := runMigration(t, legacy, environments, dryRunOptions())
	if code != exitClean {
		t.Errorf("expected exit %d, got %d\n%s", exitClean, code, out)
	}
	if environments.puts != 0 || len(environments.stored) != 0 {
		t.Errorf("a dry run wrote %d documents", environments.puts)
	}
	if !strings.Contains(out, "DRY RUN") || strings.Count(out, "would create") < 2 {
		t.Errorf("the report does not announce the dry run:\n%s", out)
	}
}

func TestMigrateApplyWritesEveryValidWorld(t *testing.T) {
	environments := newFakeEnvironments()
	legacy := &fakeLegacy{worlds: map[string]*state.World{"a": testWorld("a", "Alpha"), "b": testWorld("b", "Beta")}}
	out, code := runMigration(t, legacy, environments, applyOptions())
	if code != exitClean {
		t.Errorf("expected exit %d, got %d\n%s", exitClean, code, out)
	}
	if len(environments.stored) != 2 {
		t.Errorf("expected two stored environments, got %v", environments.stored)
	}
	stored, ok := environments.stored["a"]
	if !ok {
		t.Fatalf("world a was not written: %v", environments.stored)
	}
	if stored.Zones[0].Assets[0].ExternalRef != "urn:infai:ses:device:7283f08c" {
		t.Errorf("the platform reference was not preserved: %v", stored.Zones[0].Assets[0])
	}
	if !strings.Contains(out, "created") {
		t.Errorf("the report does not report the write:\n%s", out)
	}
}

// an invalid document is reported and skipped, and it does not stop the others:
// a migration that aborts halfway leaves the operator with a half migrated
// database and no report of the rest.
func TestMigrateApplyDoesNotWriteAnInvalidWorldAndKeepsGoing(t *testing.T) {
	broken := testWorld("broken", "Aaa broken")
	broken.Rooms["room-1"].Devices["device-1"].ExternalTypeId = ""
	environments := newFakeEnvironments()
	legacy := &fakeLegacy{worlds: map[string]*state.World{"broken": broken, "ok": testWorld("ok", "Zzz ok")}}
	out, code := runMigration(t, legacy, environments, applyOptions())
	if code != exitProblem {
		t.Errorf("expected exit %d for an invalid world, got %d\n%s", exitProblem, code, out)
	}
	if _, written := environments.stored["broken"]; written {
		t.Error("an invalid document must not be written")
	}
	if _, written := environments.stored["ok"]; !written {
		t.Error("the valid world after the invalid one was not written")
	}
	if !strings.Contains(out, string(actionBlocked)) || !strings.Contains(out, "external_type_id") {
		t.Errorf("the report does not name the rejected document and its path:\n%s", out)
	}
	if !strings.Contains(out, "result: FAILED") {
		t.Errorf("the report does not fail visibly:\n%s", out)
	}
}

// re-running the migration must not clobber the stored environment: by then it
// may carry runtime state, corrected zone types and channel sources that the
// legacy world does not know about.
func TestMigrateApplyTwiceSkipsTheSecondTimeAndChangesNothing(t *testing.T) {
	environments := newFakeEnvironments()
	legacy := &fakeLegacy{worlds: map[string]*state.World{"a": testWorld("a", "Alpha")}}
	if _, code := runMigration(t, legacy, environments, applyOptions()); code != exitClean {
		t.Fatalf("the first run failed with %d", code)
	}
	//the stored document is edited the way a user would: a corrected zone type
	edited := environments.stored["a"]
	edited.Zones[0].Type = domain.ZoneRoom
	edited.Name = "renamed by a user"
	environments.stored["a"] = edited
	putsAfterFirstRun := environments.puts

	out, code := runMigration(t, legacy, environments, applyOptions())
	if code != exitClean {
		t.Errorf("the second run has to be clean, got %d\n%s", code, out)
	}
	if environments.puts != putsAfterFirstRun {
		t.Errorf("the second run wrote %d additional documents", environments.puts-putsAfterFirstRun)
	}
	if environments.stored["a"].Name != "renamed by a user" || environments.stored["a"].Zones[0].Type != domain.ZoneRoom {
		t.Errorf("the second run overwrote the stored environment: %+v", environments.stored["a"])
	}
	if !strings.Contains(out, string(actionSkipped)) || !strings.Contains(out, "already exists") {
		t.Errorf("the report does not explain the skip:\n%s", out)
	}
}

// the plan is made before the first write, so an environment can appear in the
// store in between. Put() is an upsert and would overwrite it.
func TestMigrateDoesNotOverwriteAnEnvironmentThatAppearsAfterThePlan(t *testing.T) {
	environments := newFakeEnvironments()
	legacy := &fakeLegacy{worlds: map[string]*state.World{"a": testWorld("a", "Alpha")}}
	environments.afterGet = func(call int, id string) {
		//the first Get is the plan's question, which still answers "does not
		//exist"; the second is the check right before the write
		if call == 1 {
			environments.stored[id] = domain.Environment{Id: id, Name: "written by somebody else"}
		}
	}
	out, code := runMigration(t, legacy, environments, applyOptions())
	if code != exitClean {
		t.Errorf("expected a clean exit, got %d\n%s", code, out)
	}
	if environments.puts != 0 {
		t.Error("the environment that appeared in between was overwritten")
	}
	if environments.stored["a"].Name != "written by somebody else" {
		t.Errorf("the foreign document was replaced: %+v", environments.stored["a"])
	}
	if !strings.Contains(out, "appeared in the store after the plan was made") {
		t.Errorf("the report does not explain what happened:\n%s", out)
	}
}

func TestMigrateReportsAFailedWrite(t *testing.T) {
	environments := newFakeEnvironments()
	environments.putErr["a"] = errors.New("connection reset")
	legacy := &fakeLegacy{worlds: map[string]*state.World{"a": testWorld("a", "Alpha"), "b": testWorld("b", "Beta")}}
	out, code := runMigration(t, legacy, environments, applyOptions())
	if code != exitProblem {
		t.Errorf("expected exit %d after a failed write, got %d\n%s", exitProblem, code, out)
	}
	if _, written := environments.stored["b"]; !written {
		t.Error("a failed write must not stop the remaining worlds")
	}
	if !strings.Contains(out, string(actionWriteFailed)) || !strings.Contains(out, "connection reset") {
		t.Errorf("the report does not name the failed write:\n%s", out)
	}
}

func TestMigrateReturnsBrokenWhenTheLegacyStoreCannotBeRead(t *testing.T) {
	legacy := &fakeLegacy{err: errors.New("no reachable servers")}
	code, err := migrate(context.Background(), &bytes.Buffer{}, legacy, newFakeEnvironments(), dryRunOptions())
	if err == nil {
		t.Fatal("an unreadable legacy store has to be an error")
	}
	if code != exitBroken {
		t.Errorf("expected exit %d, got %d", exitBroken, code)
	}
}

func TestMigrateReturnsBrokenForAWorldFilterThatMatchesNothing(t *testing.T) {
	legacy := &fakeLegacy{worlds: map[string]*state.World{"a": testWorld("a", "Alpha")}}
	opts := dryRunOptions()
	opts.worldId = "does-not-exist"
	code, err := migrate(context.Background(), &bytes.Buffer{}, legacy, newFakeEnvironments(), opts)
	if err == nil {
		t.Fatal("a world filter that matches nothing has to be an error")
	}
	if code != exitBroken {
		t.Errorf("expected exit %d, got %d", exitBroken, code)
	}
}

func TestMigrateWithAWorldFilterTouchesOnlyThatWorld(t *testing.T) {
	environments := newFakeEnvironments()
	legacy := &fakeLegacy{worlds: map[string]*state.World{"a": testWorld("a", "Alpha"), "b": testWorld("b", "Beta")}}
	opts := applyOptions()
	opts.worldId = "b"
	out, code := runMigration(t, legacy, environments, opts)
	if code != exitClean {
		t.Errorf("expected a clean exit, got %d\n%s", code, out)
	}
	if len(environments.stored) != 1 || environments.stored["b"].Id != "b" {
		t.Errorf("expected only b to be written, got %v", environments.stored)
	}
	if !strings.Contains(out, "restricted to world") {
		t.Errorf("the report does not mention the restriction:\n%s", out)
	}
}

func TestExitCodeIgnoresProblemsAndSkips(t *testing.T) {
	plans := migration.Plan(map[string]*state.World{"a": testWorld("a", "Alpha")}, domain.IndustrialSite, map[string]bool{"a": true})
	results := []worldResult{{plan: plans[0], action: actionWouldSkip}}
	if got := exitCode(results); got != exitClean {
		t.Errorf("a skip is not a failure, got exit %d", got)
	}
	if len(plans[0].Problems) == 0 {
		t.Fatal("the fixture has to produce problems for this test to mean anything")
	}
	results = []worldResult{{plan: plans[0], action: actionWouldCreate}}
	if got := exitCode(results); got != exitClean {
		t.Errorf("informational problems must not change the exit code, got %d", got)
	}
	for _, blocking := range []action{actionBlocked, actionWriteFailed} {
		if got := exitCode([]worldResult{{plan: plans[0], action: blocking}}); got != exitProblem {
			t.Errorf("%v has to exit %d, got %d", blocking, exitProblem, got)
		}
	}
}

// industryPlan plans the real production export. The fixture of lib/domain is
// reused rather than copied: it is the document the conversion is golden tested
// against.
func industryPlan(t *testing.T) migration.WorldPlan {
	t.Helper()
	content, err := os.ReadFile("../../lib/domain/testdata/industry-world.json")
	if err != nil {
		t.Fatal(err)
	}
	world := state.World{}
	if err := json.Unmarshal(content, &world); err != nil {
		t.Fatal(err)
	}
	//World.Owner is bson only, so the exported owner has to be applied by hand
	world.Owner = "aae7e87b-63a2-477f-afb4-caa0db84e3fa"
	plans := migration.Plan(map[string]*state.World{world.Id: &world}, domain.IndustrialSite, nil)
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d", len(plans))
	}
	return plans[0]
}

// TestReportOfTheIndustryFixture pins what an operator sees for the real world
// this migration was written for. Set MIGRATELEGACY_PRINT_REPORT to print the
// report instead of only asserting on it.
func TestReportOfTheIndustryFixture(t *testing.T) {
	plan := industryPlan(t)
	out := &bytes.Buffer{}
	report(out, reportHeader{envType: domain.IndustrialSite}, []worldResult{{plan: plan, action: actionWouldCreate}})
	text := out.String()
	if os.Getenv("MIGRATELEGACY_PRINT_REPORT") != "" {
		fmt.Print(text)
	}

	for _, want := range []string{
		"mode                   : DRY RUN - nothing is written",
		"environment type       : industrial_site",
		`world "Industry"`,
		"legacy world id        : 4d273cd0-838f-4f84-9974-e56d18245255",
		"environment id         : 4d273cd0-838f-4f84-9974-e56d18245255",
		"owner                  : aae7e87b-63a2-477f-afb4-caa0db84e3fa",
		"contents               : 1 zone, 3 assets, 24 channels",
		"validation             : ok",
		"action                 : would create",
		"change routines        : none unmapped",
		"UNMAPPED CHANGE ROUTINES: 0 in 0 of 1 worlds",
		"legacy documents       : never deleted or modified by this tool",
		"result: OK - the plan is clean.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not contain %q:\n%s", want, text)
		}
	}
	//the three room routines are not migrated by decision, and each is named
	if !strings.Contains(text, "other findings (3)") {
		t.Errorf("the three room routines have to be listed as findings:\n%s", text)
	}
	for _, problem := range plan.OtherProblems() {
		if !strings.Contains(text, problem.Path) {
			t.Errorf("the report does not list the finding %v", problem.Path)
		}
	}
}

func TestReportNamesTheSkipAndItsReason(t *testing.T) {
	plans := migration.Plan(map[string]*state.World{"a": testWorld("a", "Alpha")}, domain.IndustrialSite, map[string]bool{"a": true})
	out := &bytes.Buffer{}
	report(out, reportHeader{apply: true, envType: domain.IndustrialSite}, []worldResult{{plan: plans[0], action: actionSkipped}})
	text := out.String()
	for _, want := range []string{
		"mode                   : APPLY",
		"action                 : skipped",
		"delete it first to import the legacy world again",
		"skipped                : 1",
		"result: OK - 0 environments created, 1 environment skipped",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not contain %q:\n%s", want, text)
		}
	}
}

func TestReportNamesEveryValidationProblemOfABlockedWorld(t *testing.T) {
	world := testWorld("a", "Alpha")
	world.Rooms["room-1"].Devices["device-1"].ExternalTypeId = ""
	plans := migration.Plan(map[string]*state.World{"a": world}, domain.IndustrialSite, nil)
	out := &bytes.Buffer{}
	report(out, reportHeader{apply: true, envType: domain.IndustrialSite}, []worldResult{{plan: plans[0], action: actionBlocked}})
	text := out.String()
	for _, want := range []string{
		"validation             : INVALID, 1 problem",
		"zones[0].assets[0].external_type_id",
		"must reference a device type",
		"action                 : NOT WRITTEN",
		"NOT WRITTEN            : 1",
		"result: FAILED",
		"no legacy world was deleted or modified",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not contain %q:\n%s", want, text)
		}
	}
}

func TestReportNamesAWorldThatCannotBeMigratedAtAll(t *testing.T) {
	plans := migration.Plan(map[string]*state.World{"": testWorld("", "Alpha")}, domain.IndustrialSite, nil)
	out := &bytes.Buffer{}
	report(out, reportHeader{envType: domain.IndustrialSite}, []worldResult{{plan: plans[0], action: actionBlocked}})
	text := out.String()
	if !strings.Contains(text, "CANNOT BE MIGRATED") || !strings.Contains(text, "no id") {
		t.Errorf("the report does not explain why the world cannot be migrated:\n%s", text)
	}
	//an id that is missing has to be visible as missing rather than as an empty column
	if !strings.Contains(text, "legacy world id        : -") {
		t.Errorf("the missing id is not visible:\n%s", text)
	}
}

func TestReportMarksAnEnvironmentWithoutAnOwner(t *testing.T) {
	world := testWorld("a", "Alpha")
	world.Owner = ""
	plans := migration.Plan(map[string]*state.World{"a": world}, domain.IndustrialSite, nil)
	out := &bytes.Buffer{}
	report(out, reportHeader{envType: domain.IndustrialSite}, []worldResult{{plan: plans[0], action: actionWouldCreate}})
	text := out.String()
	if !strings.Contains(text, "owner                  : (none)") {
		t.Errorf("a missing owner has to be visible:\n%s", text)
	}
	//the world routine is not migrated by decision and is a finding too
	if !strings.Contains(text, "other findings (2)") {
		t.Errorf("the missing owner has to be listed as a finding:\n%s", text)
	}
	if !strings.Contains(text, "deliberately not migrated") {
		t.Errorf("the world routine has to be reported as not migrated:\n%s", text)
	}
}

func TestReportOfAnEmptyRunSaysSo(t *testing.T) {
	out := &bytes.Buffer{}
	report(out, reportHeader{envType: domain.IndustrialSite}, nil)
	text := out.String()
	for _, want := range []string{
		"legacy worlds          : 0",
		"UNMAPPED CHANGE ROUTINES: 0 in 0 of 0 worlds",
		"result: OK - the plan is clean.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not contain %q:\n%s", want, text)
		}
	}
}

// a world name is user input: it is chosen through the api. A name with a
// newline in it must not be able to write extra lines into a report an operator
// reads to decide whether a production migration succeeded.
func TestReportDoesNotLetAWorldNameForgeReportLines(t *testing.T) {
	world := testWorld("a", "harmless\n  result: OK - the plan is clean.\n  action                 : created")
	plans := migration.Plan(map[string]*state.World{"a": world}, domain.IndustrialSite, nil)
	out := &bytes.Buffer{}
	report(out, reportHeader{apply: true, envType: domain.IndustrialSite}, []worldResult{{plan: plans[0], action: actionBlocked}})
	text := out.String()
	if strings.Contains(text, "\n  result: OK") {
		t.Errorf("the world name forged a result line:\n%s", text)
	}
	//the escaped name stays on one line, so the forged text can never start a
	//line of its own
	actionLines := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "  action ") {
			actionLines++
		}
	}
	if actionLines != 1 {
		t.Errorf("expected exactly one action line, got %d:\n%s", actionLines, text)
	}
	if !strings.Contains(text, `harmless\n`) {
		t.Errorf("the newline was not escaped visibly:\n%s", text)
	}
	if !strings.Contains(text, "result: FAILED") {
		t.Errorf("the real result was lost:\n%s", text)
	}
}

func TestSanitizeEscapesControlCharactersAndKeepsTheRest(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{in: "Halle 1 (30 °C)", want: "Halle 1 (30 °C)"},
		{in: "a\nb", want: `a\nb`},
		{in: "a\r\nb", want: `a\r\nb`},
		{in: "a\tb", want: `a\tb`},
		{in: "a\x1b[2Kb", want: `a\x1b[2Kb`},
		{in: "a\x00b", want: `a\x00b`},
		{in: "a\x7fb", want: `a\x7fb`},
		{in: "", want: ""},
	} {
		if got := sanitize(test.in); got != test.want {
			t.Errorf("sanitize(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}
