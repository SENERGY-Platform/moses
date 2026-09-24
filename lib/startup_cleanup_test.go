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

package lib

import (
	"errors"
	"reflect"
	"testing"
)

func TestStartupCleanupUndoesInReverseOrderOnFailure(t *testing.T) {
	var order []string
	var cleanup startupCleanup
	cleanup.add(func() { order = append(order, "persistence") })
	cleanup.add(func() { order = append(order, "environments") })
	cleanup.add(func() { order = append(order, "runtime") })

	cleanup.runIf(errors.New("startup failed"))

	if want := []string{"runtime", "environments", "persistence"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestStartupCleanupKeepsEverythingOnSuccess(t *testing.T) {
	ran := false
	var cleanup startupCleanup
	cleanup.add(func() { ran = true })

	cleanup.runIf(nil)

	if ran {
		t.Error("a successful startup must keep the connections open")
	}
}
