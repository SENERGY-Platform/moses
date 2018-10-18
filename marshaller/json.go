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

package marshaller

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"moses/iotmodel"
	"reflect"
	"strconv"
	"strings"
)

func FormatToJson(value InputOutput) (result string, err error) {
	resultStruct, err := FormatToJsonStruct(value)
	if err != nil {
		return result, err
	}
	b := new(bytes.Buffer)
	encoder := json.NewEncoder(b)
	encoder.SetEscapeHTML(false) //no encoding of < > etc
	encoder.SetIndent("", "    ")
	err = encoder.Encode(resultStruct)
	result = b.String()
	return
}

func FormatToJsonStruct(value InputOutput) (result interface{}, err error) {
	if !iotmodel.GetAllowedValuesBase().IsPrimitive(iotmodel.ValueType{BaseType: value.Type.Base}) {
		if iotmodel.GetAllowedValuesBase().IsSet(iotmodel.ValueType{BaseType: value.Type.Base}) {
			list := []interface{}{}
			for _, val := range value.Values {
				element, err := FormatToJsonStruct(val)
				if err != nil {
					return result, err
				}
				list = append(list, element)
			}
			return list, err
		} else {
			m := map[string]interface{}{}
			for _, val := range value.Values {
				m[val.Name], err = FormatToJsonStruct(val)
				if err != nil {
					return result, err
				}
			}
			return m, err
		}
	} else {
		effectiveValue := value.Value
		switch value.Type.Base {
		case iotmodel.XsdBool:
			return strings.TrimSpace(effectiveValue) == "true", err
		case iotmodel.XsdInt:
			f, err := strconv.ParseFloat(effectiveValue, 64)
			return int64(f), err
		case iotmodel.XsdFloat:
			return strconv.ParseFloat(effectiveValue, 64)
		case iotmodel.XsdString:
			return effectiveValue, err
		default:
			temp, _ := json.MarshalIndent(value, "", "     ")
			log.Println(string(temp))
			return result, errors.New("for value incompatible basetype :" + value.Type.Base)
		}
	}
	return
}

func ParseFromJson(valueType iotmodel.ValueType, value string) (result InputOutput, err error) {
	var valueInterface interface{}
	err = json.Unmarshal([]byte(value), &valueInterface)
	if err != nil {
		return
	}
	result, err = ParseFromJsonInterface(valueType, valueInterface)
	if err != nil {
		log.Println(value)
		log.Println(valueInterface)
	}
	return
}

func ParseFromJsonInterface(valueType iotmodel.ValueType, valueInterface interface{}) (result InputOutput, err error) {
	result.Type.Name = valueType.Name
	result.Type.Desc = valueType.Description
	result.Type.Id = valueType.Id
	result.Type.Base = valueType.BaseType
	switch value := valueInterface.(type) {
	case map[string]interface{}:
		if result.Type.Base != iotmodel.MapBaseType && result.Type.Base != iotmodel.StructBaseType && result.Type.Base != iotmodel.IndexStructBaseType {
			log.Println("WARNING: used basetype is not consistent to map", result.Type)
			return
		}
		for key, val := range value {
			err := handleJsonChild(valueType, val, key, &result.Values)
			if err != nil {
				return result, err
			}
		}
	case []interface{}:
		if result.Type.Base != iotmodel.ListBaseType {
			log.Println("WARNING: used basetype is not consistent to list", result.Type)
			return
		}
		for _, val := range value {
			err := handleJsonChild(valueType, val, "", &result.Values)
			if err != nil {
				return result, err
			}
		}
	case bool:
		if result.Type.Base != iotmodel.XsdBool {
			log.Println("WARNING: used basetype is not consistent to boolean", result.Type)
			return
		}
		if value {
			result.Value = "true"
		} else {
			result.Value = "false"
		}
	case string:
		if result.Type.Base != iotmodel.XsdString {
			log.Println("WARNING: used basetype is not consistent to string", result.Type)
			return
		}
		result.Value = value
	case float64:
		if result.Type.Base != iotmodel.XsdInt && result.Type.Base != iotmodel.XsdFloat {
			log.Println("WARNING: used basetype is not consistent to number", result.Type)
			return
		}
		if result.Type.Base == iotmodel.XsdInt {
			result.Value = strconv.FormatInt(int64(value), 10)
		}
		if result.Type.Base == iotmodel.XsdFloat {
			result.Value = strconv.FormatFloat(value, 'f', -1, 64)
		}
	case nil:
		return
	default:
		err = errors.New("error in ParseFromJsonInterface(): unknown interface type <<" + reflect.TypeOf(valueInterface).Name() + ">>")
	}
	return
}

func handleJsonChild(valueType iotmodel.ValueType, value interface{}, childName string, result *[]InputOutput) (err error) {
	childField, err := getChildFiled(valueType, childName)
	if err != nil || childField.Id == "" {
		log.Println("WARNING: error while trying to find matching field in valuetype (ignore value)", childName, err)
		return nil //ignore field
	}
	child, err := ParseFromJsonInterface(childField.Type, value)
	if err != nil {
		return err
	}
	if childName == "" {
		child.Name = childField.Name
	} else {
		child.Name = childName
	}

	child.FieldId = childField.Id
	*result = append(*result, child)
	return
}

func getChildFiled(valueType iotmodel.ValueType, childName string) (childField iotmodel.FieldType, err error) {
	allowed := iotmodel.GetAllowedValuesBase()
	switch {
	case allowed.IsCollection(valueType):
		return valueType.Fields[0], err
	case allowed.IsStructure(valueType):
		for _, field := range valueType.Fields {
			if field.Name == childName {
				return field, err
			}
		}
	default:
		err = errors.New("error on getChildFiled(): cant find child type")
	}
	return
}
