/*
 * Copyright 2018 InfAI (CC SES)
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
	"github.com/cbroglie/mustache"
)

func GetTemplateParameterList(str string) (result []string, err error) {
	templ, err := mustache.ParseString(str)
	if err != nil {
		return result, err
	}
	tags := append([]mustache.Tag{}, templ.Tags()...)
	index := 0
	for index < len(tags) {
		tag := tags[index]
		if tag.Type() != mustache.Variable {
			tags = append(tags, tag.Tags()...)
		}
		result = append(result, tag.Name())
		index++
	}
	return
}

func RenderTempl(templ string, parameter interface{}) (result string, err error) {
	return mustache.Render(templ, parameter)
}

var defaultTemplates = map[string]RoutineTemplate{
	"default_linear": RoutineTemplate{
		Name: "linear",
		Description: `default linear template
compareState is for example world
compareValue is for example temp
changeState is for example world.getRoom("room_1")
changeValue is for example temp
increment is the value by which the changeValue is incremented ore decremented`,
		Parameter: []string{"compareState", "compareValue", "changeState", "changeValue", "increment"},
		Template: `var targetValue = moses.{{compareState}}.state.get("{{compareValue}}");
var isValue = moses.{{changeState}}.state.get("{{changeValue}}");
if(targetValue > isValue){
	isValue = isValue + {{increment}};
}else if(targetValue < isValue){
	isValue = isValue - {{increment}};
}
moses.{{changeState}}.state.set("{{changeValue}}", isValue);`,
	},
}
