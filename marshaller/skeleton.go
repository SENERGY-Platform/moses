/*
 * Copyright 2018 SENERGY Team
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

package marshaller

import (
	"errors"
	"fmt"
	"moses/iotmodel"
)

func SkeletonFromAssignment(assignment iotmodel.TypeAssignment, allowedValues iotmodel.AllowedValues) (result InputOutput, err error) {
	result.Name = assignment.Name
	result.Type = typeFromValueType(assignment.Type)
	err = setSkeletonValueFromValueType(&result, assignment.Type, allowedValues)
	return
}

func setSkeletonValueFromValueType(skeleton *InputOutput, valueType iotmodel.ValueType, allowedValues iotmodel.AllowedValues) (err error) {
	switch {
	case allowedValues.IsPrimitive(valueType):
		switch valueType.BaseType {
		case iotmodel.XsdBool:
			skeleton.Value = "true"
		case iotmodel.XsdString:
			skeleton.Value = "STRING"
		case iotmodel.XsdFloat:
			skeleton.Value = "0.0"
		case iotmodel.XsdInt:
			skeleton.Value = "0"
		}
	case allowedValues.IsStructure(valueType):
		for _, field := range valueType.Fields {
			input := InputOutput{
				FieldId: field.Id,
				Name:    field.Name,
				Type:    typeFromValueType(field.Type),
			}
			err = setSkeletonValueFromValueType(&input, field.Type, allowedValues)
			if err != nil {
				return err
			}
			skeleton.Values = append(skeleton.Values, input)
		}
	case allowedValues.IsMap(valueType):
		if len(valueType.Fields) != 1 {
			return errors.New("Collection with more or less then one field")
		}
		subtype := valueType.Fields[0].Type
		input := InputOutput{
			FieldId: valueType.Fields[0].Id,
			Name:    "KEY",
			Type:    typeFromValueType(subtype),
		}
		err = setSkeletonValueFromValueType(&input, subtype, allowedValues)
		if err != nil {
			return err
		}
		skeleton.Values = append(skeleton.Values, input)
	case allowedValues.IsSet(valueType):
		if len(valueType.Fields) != 1 {
			return errors.New("Collection with more or less then one field")
		}
		subtype := valueType.Fields[0].Type
		input := InputOutput{
			Name:    valueType.Fields[0].Name,
			FieldId: valueType.Fields[0].Id,
			Type:    typeFromValueType(subtype),
		}
		err = setSkeletonValueFromValueType(&input, subtype, allowedValues)
		if err != nil {
			return err
		}
		skeleton.Values = append(skeleton.Values, input)
	default:
		fmt.Println("unknown base type: " + valueType.BaseType)
		return errors.New("unknown base type: " + valueType.BaseType)
	}
	return
}

func typeFromValueType(valueType iotmodel.ValueType) Type {
	return Type{Name: valueType.Name, Desc: valueType.Description, Id: valueType.Id, Base: valueType.BaseType}
}
