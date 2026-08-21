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
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/migration"
)

// action is what happened, or would happen, to one legacy world. The two
// blocking outcomes are upper case because they are the only ones an operator
// has to act on, and a report of thirty worlds is skimmed, not read.
type action string

const (
	actionWouldCreate action = "would create"
	actionCreated     action = "created"
	actionWouldSkip   action = "would skip"
	actionSkipped     action = "skipped"
	actionBlocked     action = "NOT WRITTEN"
	actionWriteFailed action = "WRITE FAILED"
)

// worldResult is one line item of the report: the plan and what became of it.
type worldResult struct {
	plan   migration.WorldPlan
	action action
	// note carries a remark about the action itself, for example that the
	// environment appeared in the store between the plan and the write.
	note     string
	writeErr error
}

type reportHeader struct {
	apply   bool
	envType domain.EnvironmentType
	// worldFilter is the -world argument, empty when all worlds were planned.
	worldFilter string
}

const labelWidth = 22

// report writes the whole report to out. It is the interface of this tool, so it
// is deliberately verbose: every problem the conversion found is printed with its
// path, because those paths are the work list of the phase that follows.
func report(out io.Writer, header reportHeader, results []worldResult) {
	fmt.Fprintf(out, "moses legacy world migration\n")
	mode := "DRY RUN - nothing is written"
	if header.apply {
		mode = "APPLY - planned environments are written"
	}
	field(out, "mode", mode)
	field(out, "environment type", string(header.envType))
	field(out, "legacy worlds", strconv.Itoa(len(results)))
	if header.worldFilter != "" {
		field(out, "restricted to world", header.worldFilter)
	}
	field(out, "legacy documents", "never deleted or modified by this tool")

	for _, result := range results {
		reportWorld(out, result)
	}
	reportSummary(out, header, results)
}

func reportWorld(out io.Writer, result worldResult) {
	plan := result.plan
	name := plan.WorldName
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Fprintf(out, "\nworld %q\n", name)
	field(out, "legacy world id", orDash(plan.WorldId))
	field(out, "environment id", orDash(plan.Environment.Id))
	field(out, "environment name", orDash(plan.Environment.Name))
	field(out, "owner", orNone(plan.Environment.Owner))
	zones, assets, channels := plan.Counts()
	field(out, "contents", fmt.Sprintf("%s, %s, %s",
		count(zones, "zone", "zones"), count(assets, "asset", "assets"), count(channels, "channel", "channels")))
	field(out, "seed", strconv.FormatInt(plan.Environment.Seed, 10))

	reportValidation(out, plan)
	field(out, "action", string(result.action))
	if plan.Skip {
		field(out, "reason", plan.SkipReason)
	}
	if result.note != "" {
		field(out, "note", result.note)
	}
	if result.writeErr != nil {
		field(out, "write error", result.writeErr.Error())
	}
	if plan.Skip && plan.Err != nil {
		field(out, "note", "the conversion of this legacy world would not be valid, but the world is skipped, so nothing was written; the stored environment is what runs")
	}

	//both kinds of finding are reported even when there are none, so that an
	//empty section is a statement rather than a gap in the report
	routines := plan.UnmappedRoutines()
	if len(routines) > 0 {
		fmt.Fprintf(out, "  UNMAPPED CHANGE ROUTINES (%d) - not migrated, their javascript stays in the legacy world only:\n", len(routines))
		listProblems(out, routines)
	} else {
		field(out, "change routines", "none unmapped")
	}
	others := plan.OtherProblems()
	if len(others) > 0 {
		fmt.Fprintf(out, "  other findings (%d):\n", len(others))
		listProblems(out, others)
	} else {
		field(out, "other findings", "none")
	}
}

// reportValidation prints why a plan may or may not be written. The three
// blocking reasons read differently on purpose: an invalid document is a data
// problem with a path, the others are a broken legacy document or a caller
// mistake.
func reportValidation(out io.Writer, plan migration.WorldPlan) {
	if plan.Err == nil {
		field(out, "validation", "ok")
		return
	}
	validation := &domain.ValidationError{}
	if errors.As(plan.Err, &validation) {
		field(out, "validation", fmt.Sprintf("INVALID, %s", count(len(validation.Problems), "problem", "problems")))
		listProblems(out, validation.Problems)
		return
	}
	field(out, "validation", "CANNOT BE MIGRATED: "+plan.Err.Error())
}

func listProblems(out io.Writer, problems []domain.Problem) {
	for _, problem := range problems {
		path := problem.Path
		if path == "" {
			path = "(whole document)"
		}
		fmt.Fprintf(out, "      %s\n        %s\n", sanitize(path), sanitize(problem.Message))
	}
}

func reportSummary(out io.Writer, header reportHeader, results []worldResult) {
	created, skipped, blocked, failed := 0, 0, 0, 0
	routines, routineWorlds, others := 0, 0, 0
	for _, result := range results {
		switch result.action {
		case actionCreated, actionWouldCreate:
			created++
		case actionSkipped, actionWouldSkip:
			skipped++
		case actionBlocked:
			blocked++
		case actionWriteFailed:
			failed++
		}
		unmapped := len(result.plan.UnmappedRoutines())
		routines += unmapped
		if unmapped > 0 {
			routineWorlds++
		}
		others += len(result.plan.OtherProblems())
	}

	fmt.Fprintf(out, "\nsummary\n")
	field(out, "worlds planned", strconv.Itoa(len(results)))
	field(out, verb(header.apply, "created", "would create"), strconv.Itoa(created))
	field(out, verb(header.apply, "skipped", "would skip"), strconv.Itoa(skipped))
	field(out, "NOT WRITTEN", strconv.Itoa(blocked))
	field(out, "WRITE FAILED", strconv.Itoa(failed))
	field(out, "other findings", strconv.Itoa(others))

	fmt.Fprintf(out, "\n  UNMAPPED CHANGE ROUTINES: %d in %d of %d worlds\n", routines, routineWorlds, len(results))
	if routines > 0 {
		fmt.Fprintf(out, "      This is the work list for the declarative sources phase. Every routine\n")
		fmt.Fprintf(out, "      listed above has to be re-created as a channel source; until then its\n")
		fmt.Fprintf(out, "      javascript exists only in the legacy world document, which this tool\n")
		fmt.Fprintf(out, "      never deletes.\n")
	}

	fmt.Fprintf(out, "\n%s\n", resultLine(header, len(results), created, skipped, blocked, failed))
}

// result is the last line, and the one a script or a reader in a hurry looks at.
func resultLine(header reportHeader, planned int, created int, skipped int, blocked int, failed int) string {
	if blocked > 0 || failed > 0 {
		return fmt.Sprintf("result: FAILED - of %d legacy worlds, %d were not written and %d writes failed. Nothing was lost: no legacy world was deleted or modified.",
			planned, blocked, failed)
	}
	if header.apply {
		return fmt.Sprintf("result: OK - %s created, %s skipped because they already exist. No legacy world was deleted or modified.",
			count(created, "environment", "environments"), count(skipped, "environment", "environments"))
	}
	return fmt.Sprintf("result: OK - the plan is clean. Nothing was written; %s would be created and %s skipped. Re-run with -apply to write.",
		count(created, "environment", "environments"), count(skipped, "environment", "environments"))
}

func field(out io.Writer, label string, value string) {
	fmt.Fprintf(out, "  %-*s : %s\n", labelWidth, label, sanitize(value))
}

// sanitize escapes the control characters of a value that comes from the
// database, which is every name, id, owner and problem message in this report.
//
// Those values are user input: a world is named through the api, and a name
// containing a newline would let its author write additional lines into this
// report - "result: OK - the plan is clean." below a world that was in fact not
// written. An operator decides on the basis of this report whether a production
// migration succeeded, so its line structure must not be forgeable. Escape
// sequences are dropped for the same reason: they can hide text on a terminal.
func sanitize(value string) string {
	if strings.IndexFunc(value, isControl) < 0 {
		return value
	}
	result := strings.Builder{}
	result.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n':
			result.WriteString("\\n")
		case r == '\r':
			result.WriteString("\\r")
		case r == '\t':
			result.WriteString("\\t")
		case isControl(r):
			fmt.Fprintf(&result, "\\x%02x", r)
		default:
			result.WriteRune(r)
		}
	}
	return result.String()
}

// isControl covers the c0 and c1 control ranges and delete. Anything else -
// umlauts, degree signs, any other printable rune - is left alone: this is a
// report a human reads, not a serialisation format.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func verb(apply bool, applied string, planned string) string {
	if apply {
		return applied
	}
	return planned
}

func count(n int, singular string, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}
	return value
}
