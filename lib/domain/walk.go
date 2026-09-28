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

package domain

import "fmt"

// WalkAssets visits every asset Validate checks, with the path Validate reports
// that asset's problems under, so a check outside this package points at the
// same fields. It returns the nodes counted the way Validate counts them.
func WalkAssets(env Environment, visit func(path string, asset Asset)) (nodes int) {
	var walk func(path string, zone Zone, depth int)
	walk = func(path string, zone Zone, depth int) {
		nodes++
		//Validate stops at this depth, so nothing below it is reported there either
		if depth > MaxZoneDepth {
			return
		}
		for i := range zone.Zones {
			walk(fmt.Sprintf("%s.zones[%d]", path, i), zone.Zones[i], depth+1)
		}
		for i := range zone.Assets {
			nodes += 1 + len(zone.Assets[i].Channels)
			visit(fmt.Sprintf("%s.assets[%d]", path, i), zone.Assets[i])
		}
	}
	for i := range env.Zones {
		walk(fmt.Sprintf("zones[%d]", i), env.Zones[i], 1)
	}
	return nodes
}

// ChannelPath is the path Validate reports a problem of an asset's channel under.
func ChannelPath(assetPath string, index int) string {
	return fmt.Sprintf("%s.channels[%d]", assetPath, index)
}
