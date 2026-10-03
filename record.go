// Copyright [2019] LinkedIn Corp. Licensed under the Apache License, Version
// 2.0 (the "License"); you may not use this file except in compliance with the
// License.  You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.

package goavro

import (
	"fmt"
)

func makeRecordCodec(st map[string]*Codec, enclosingNamespace string, schemaMap map[string]interface{}, cb *codecBuilder) (*Codec, error) {
	// NOTE: To support recursive data types, create the codec and register it
	// using the specified name, and fill in the codec functions later.
	c, err := registerNewCodec(st, schemaMap, enclosingNamespace)
	if err != nil {
		return nil, fmt.Errorf("Record ought to have valid name: %s", err)
	}

	fields, ok := schemaMap["fields"]
	if !ok {
		return nil, fmt.Errorf("Record %q ought to have fields key", c.typeName)
	}
	fieldSchemas, ok := fields.([]interface{})
	if !ok || fieldSchemas == nil {
		return nil, fmt.Errorf("Record %q fields ought to be non-nil array: %v", c.typeName, fields)
	}

	codecFromFieldName := make(map[string]*Codec)
	codecFromIndex := make([]*Codec, len(fieldSchemas))
	nameFromIndex := make([]string, len(fieldSchemas))
	defaultValueFromName := make(map[string]interface{}, len(fieldSchemas))
	fieldSchemasFromIndex := make([]*recordFieldSchema, len(fieldSchemas))
	// Publish field metadata before validating defaults below: a field
	// default may refer back to this record (recursive schema) and default
	// validation traverses the codec structure.
	c.recordFields = fieldSchemasFromIndex

	for i, fieldSchema := range fieldSchemas {
		fieldSchemaMap, ok := fieldSchema.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("Record %q field %d ought to be valid Avro named type; received: %v", c.typeName, i+1, fieldSchema)
		}

		// NOTE: field names are not registered in the symbol table, because
		// field names are not individually addressable codecs.

		fieldCodec, err := buildCodec(st, c.typeName.namespace, fieldSchemaMap, cb)
		if err != nil {
			return nil, fmt.Errorf("Record %q field %d ought to be valid Avro named type: %s", c.typeName, i+1, err)
		}

		// However, when creating a full name for the field name, be sure to use
		// record's namespace
		n, err := newNameFromSchemaMap(c.typeName.namespace, fieldSchemaMap)
		if err != nil {
			return nil, fmt.Errorf("Record %q field %d ought to have valid name: %v", c.typeName, i+1, fieldSchemaMap)
		}
		fieldName := n.short()
		if _, ok := codecFromFieldName[fieldName]; ok {
			return nil, fmt.Errorf("Record %q field %d ought to have unique name: %q", c.typeName, i+1, fieldName)
		}

		fieldInfo := &recordFieldSchema{name: fieldName, codec: fieldCodec}
		fieldSchemasFromIndex[i] = fieldInfo

		rawDefault, hasDefault := fieldSchemaMap["default"]
		if hasDefault {
			// NOTE: To support a null default value, the string literal
			// "null" is coerced to nil when EnableStringNull is true.
			// see https://github.com/linkedin/goavro/issues/280
			if cb.option.EnableStringNull && rawDefault == "null" {
				rawDefault = nil
			}

			// Publish hasDefault and the raw default before validation below:
			// a recursive default reaches back into this same field, and
			// non-terminating expansion detection must see it as a default.
			// rawDefault is a value decoded from the schema JSON, so mutating
			// this local never rewrites the original schema text.
			fieldInfo.hasDefault = true
			fieldInfo.defaultRaw = rawDefault

			// Validate the default against the field schema field by field at
			// codec construction time, and convert it to its native Go form.
			// Every field's default is validated here, rather than lazily when
			// data happens to omit the field.
			dctx := &defaultContext{coerceNullString: cb.option.EnableStringNull}
			defer dctx.enter(fieldName)()
			defaultValue, derr := dctx.materializeFromDefault(fieldCodec, rawDefault)
			if derr != nil {
				return nil, fmt.Errorf("Record %q field %q: default value ought to encode using field schema: %s", c.typeName, fieldName, derr)
			}
			// Final check that the converted native value round-trips through
			// the field's binary encoder, which validates logical type
			// constraints not covered above (e.g. decimal scale/precision).
			if _, err = fieldCodec.binaryFromNative(nil, defaultValue); err != nil {
				return nil, fmt.Errorf("Record %q field %q: default value ought to encode using field schema: %s", c.typeName, fieldName, err)
			}
			fieldInfo.defaultNative = defaultValue
			defaultValueFromName[fieldName] = defaultValue
		}

		nameFromIndex[i] = fieldName
		codecFromIndex[i] = fieldCodec
		codecFromFieldName[fieldName] = fieldCodec
	}

	c.binaryFromNative = func(buf []byte, datum interface{}) ([]byte, error) {
		valueMap, ok := datum.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("cannot encode binary record %q: expected map[string]interface{}; received: %T", c.typeName, datum)
		}

		// records encoded in order fields were defined in schema
		for i, fieldCodec := range codecFromIndex {
			fieldName := nameFromIndex[i]

			// NOTE: If field value was not specified in map, then set
			// fieldValue to its default value (which may or may not have been
			// specified).
			fieldValue, ok := valueMap[fieldName]
			if !ok {
				if fieldValue, ok = defaultValueFromName[fieldName]; !ok {
					return nil, fmt.Errorf("cannot encode binary record %q field %q: schema does not specify default value and no value provided", c.typeName, fieldName)
				}
				fieldValue = deepCopyNative(codecFromIndex[i], fieldValue)
			}

			var err error
			buf, err = fieldCodec.binaryFromNative(buf, fieldValue)
			if err != nil {
				return nil, fmt.Errorf("cannot encode binary record %q field %q: value does not match its schema: %s", c.typeName, fieldName, err)
			}
		}
		return buf, nil
	}

	c.nativeFromBinary = func(buf []byte) (interface{}, []byte, error) {
		recordMap := make(map[string]interface{}, len(codecFromIndex))
		for i, fieldCodec := range codecFromIndex {
			name := nameFromIndex[i]
			var value interface{}
			var err error
			value, buf, err = fieldCodec.nativeFromBinary(buf)
			if err != nil {
				return nil, nil, fmt.Errorf("cannot decode binary record %q field %q: %s", c.typeName, name, err)
			}
			recordMap[name] = value
		}
		return recordMap, buf, nil
	}

	// Capture the ignoreExtraFields option for use in the closure
	ignoreExtraFields := cb.option != nil && cb.option.IgnoreExtraFieldsFromTextual

	c.nativeFromTextual = func(buf []byte) (interface{}, []byte, error) {
		var mapValues map[string]interface{}
		var err error
		// NOTE: Setting `defaultCodec == nil` instructs genericMapTextDecoder
		// to return an error when a field name is not found in the
		// codecFromFieldName map, unless ignoreExtraFields is true.
		mapValues, buf, err = genericMapTextDecoder(buf, nil, codecFromFieldName, ignoreExtraFields)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot decode textual record %q: %s", c.typeName, err)
		}
		if actual, expected := len(mapValues), len(codecFromFieldName); actual != expected {
			// set missing field keys to their respective default values, then
			// re-check number of keys
			for fieldName, defaultValue := range defaultValueFromName {
				if _, ok := mapValues[fieldName]; !ok {
					fieldInfo := codecFromFieldName[fieldName]
					mapValues[fieldName] = deepCopyNative(fieldInfo, defaultValue)
				}
			}
			if actual, expected = len(mapValues), len(codecFromFieldName); actual != expected {
				return nil, nil, fmt.Errorf("cannot decode textual record %q: only found %d of %d fields", c.typeName, actual, expected)
			}
		}
		return mapValues, buf, nil
	}

	c.textualFromNative = func(buf []byte, datum interface{}) ([]byte, error) {
		// NOTE: Ensure only schema defined field names are encoded; and if
		// missing in datum, either use the provided field default value or
		// return an error.
		sourceMap, ok := datum.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("cannot encode textual record %q: expected map[string]interface{}; received: %T", c.typeName, datum)
		}
		destMap := make(map[string]interface{}, len(codecFromIndex))
		for fieldName := range codecFromFieldName {
			fieldValue, ok := sourceMap[fieldName]
			if !ok {
				defaultValue, ok := defaultValueFromName[fieldName]
				if !ok {
					return nil, fmt.Errorf("cannot encode textual record %q field %q: schema does not specify default value and no value provided", c.typeName, fieldName)
				}
				fieldValue = deepCopyNative(codecFromFieldName[fieldName], defaultValue)
			}
			destMap[fieldName] = fieldValue
		}
		datum = destMap
		// NOTE: Setting `defaultCodec == nil` instructs genericMapTextEncoder
		// to return an error when a field name is not found in the
		// codecFromFieldName map.
		return genericMapTextEncoder(buf, datum, nil, codecFromFieldName)
	}

	return c, nil
}
