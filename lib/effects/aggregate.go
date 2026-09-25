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

package effects

import (
	"strings"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// aggregateLink is one channel an aggregate sums: input on child, summed by
// channel on meter.
type aggregateLink struct {
	meter, channel string
	child, input   string
}

// aggregateLinks duplicates the resolution of lib/runtime (indexAggregates),
// which cannot be shared without importing the runtime here. The runtime's
// parity test pins the two to the same answer.
func aggregateLinks(b *builder) []aggregateLink {
	children := map[string][]*assetEntry{}
	for _, entry := range b.assetOrder {
		meter := entry.asset.SubmeteredBy
		if meter == "" || meter == entry.asset.Id {
			continue
		}
		children[meter] = append(children[meter], entry)
	}
	seen := map[string]bool{}
	result := []aggregateLink{}
	for _, entry := range b.assetOrder {
		for _, channel := range entry.asset.Channels {
			if channel.Source.Kind != domain.SourceAggregate || channel.Id == "" || seen[channel.Id] {
				continue
			}
			seen[channel.Id] = true
			characteristic := strings.TrimSpace(channel.CharacteristicId)
			if characteristic == "" {
				continue
			}
			for _, child := range children[entry.asset.Id] {
				for _, sub := range child.asset.Channels {
					if sub.Id == "" || sub.Id == channel.Id || strings.TrimSpace(sub.CharacteristicId) != characteristic {
						continue
					}
					result = append(result, aggregateLink{
						meter: entry.asset.Id, channel: channel.Id, child: child.asset.Id, input: sub.Id,
					})
				}
			}
		}
	}
	return result
}

// AggregateInputs maps every aggregate channel to the channel ids it sums, in
// the order the runtime sums them. It exists for the parity test against
// lib/runtime and carries an entry, possibly empty, per aggregate channel.
func AggregateInputs(env domain.Environment) map[string][]string {
	b := newBuilder(env)
	result := map[string][]string{}
	for _, entry := range b.assetOrder {
		for _, channel := range entry.asset.Channels {
			if channel.Source.Kind == domain.SourceAggregate && channel.Id != "" {
				if _, known := result[channel.Id]; !known {
					result[channel.Id] = []string{}
				}
			}
		}
	}
	for _, link := range aggregateLinks(b) {
		result[link.channel] = append(result[link.channel], link.input)
	}
	return result
}
