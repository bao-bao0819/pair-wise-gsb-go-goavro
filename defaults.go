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
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// defaultContext validates the JSON value declared by a schema "default"
// attribute and converts it to the library's native Go representation. It is
// also used to materialize an independent default object for a record.
//
// A single defaultContext is used per materialization so that the chain of
// records currently being expanded via their own declared defaults can be
// tracked. When that chain revisits a record codec, the defaults form a
// non-terminating expansion and materialization stops with the offending field
// path rather than recursing forever.
type defaultContext struct {
	// path is the field path from the root of the materialization to the value
	// currently being converted. It is only used for error reporting.
	path []string
	// defaultExpansion records record codecs that are currently being filled
	// from their declared default value. A codec is pushed only when entering
	// a record through its own default, and popped again on return.
	defaultExpansion []*Codec
	// coerceNullString controls whether the JSON string literal "null" is
	// accepted in place of an actual null value for union defaults.
	coerceNullString bool
}

// enter appends a segment to the field path used in error messages.
func (dc *defaultContext) enter(segment string) func() {
	dc.path = append(dc.path, segment)
	return func() { dc.path = dc.path[:len(dc.path)-1] }
}

// pathString renders the current field path using Avro-style dot notation,
// e.g. "outer.inner[0].leaf".
func (dc *defaultContext) pathString() string {
	if len(dc.path) == 0 {
		return ""
	}
	return strings.Join(dc.path, ".")
}

func (dc *defaultContext) failf(format string, args ...interface{}) error {
	msg := fmt.Sprintf(format, args...)
	if path := dc.pathString(); path != "" {
		return fmt.Errorf("at field %q: %s", path, msg)
	}
	return errors.New(msg)
}

// materializeFromDefault converts a schema-declared default value to its
// native Go form. Unlike a value explicitly present in the datum, a record
// reached through its own default participates in non-terminating expansion
// detection.
func (dc *defaultContext) materializeFromDefault(c *Codec, raw interface{}) (interface{}, error) {
	return dc.convert(c, raw, true)
}

// convertExplicit converts a value explicitly present in the datum or default
// JSON. Explicit record values do not by themselves count as default-driven
// recursion.
func (dc *defaultContext) convertExplicit(c *Codec, raw interface{}) (interface{}, error) {
	return dc.convert(c, raw, false)
}

func (dc *defaultContext) convert(c *Codec, raw interface{}, fromDefault bool) (interface{}, error) {
	switch {
	case c.recordFields != nil:
		return dc.convertRecord(c, raw, fromDefault)
	case c.arrayItemCodec != nil:
		return dc.convertArray(c, raw)
	case c.mapValueCodec != nil:
		return dc.convertMap(c, raw)
	case c.unionMemberCodecs != nil:
		return dc.convertUnion(c, raw, fromDefault)
	case c.enumSymbols != nil:
		return dc.convertEnum(c, raw)
	case c.fixedSize != nil:
		if c.logicalDecimal {
			return dc.convertDecimal(c, raw)
		}
		return dc.convertFixed(c, raw)
	case c.logicalDecimal:
		return dc.convertDecimal(c, raw)
	}

	switch c.typeName.short() {
	case "null":
		if raw == nil {
			return nil, nil
		}
		return nil, dc.failf("default value ought to be null; received: %s", jsonTypeName(raw))
	case "boolean":
		v, ok := raw.(bool)
		if !ok {
			return nil, dc.failf("default value ought to have a bool type, got: %s", jsonTypeName(raw))
		}
		return v, nil
	case "int":
		v, err := dc.jsonNumber(raw)
		if err != nil {
			return nil, err
		}
		if math.Trunc(v) != v || v < math.MinInt32 || v > math.MaxInt32 {
			return nil, dc.failf("default value ought to be a 32-bit integer; received: %v", raw)
		}
		return int32(v), nil
	case "long":
		v, err := dc.jsonNumber(raw)
		if err != nil {
			return nil, err
		}
		if math.Trunc(v) != v || v < math.MinInt64 || v > math.MaxInt64 {
			return nil, dc.failf("default value ought to be a 64-bit integer; received: %v", raw)
		}
		return int64(v), nil
	case "float":
		v, err := dc.jsonNumber(raw)
		if err != nil {
			return nil, dc.failf("default value ought to have a float type got: %s", jsonTypeName(raw))
		}
		f := float32(v)
		if math.IsInf(float64(f), 0) {
			return nil, dc.failf("default value ought to fit in a 32-bit float; received: %v", raw)
		}
		return f, nil
	case "double":
		v, err := dc.jsonNumber(raw)
		if err != nil {
			return nil, dc.failf("default value ought to have a double type got: %s", jsonTypeName(raw))
		}
		return v, nil
	case "bytes":
		return dc.convertBytes(raw)
	case "string":
		v, ok := raw.(string)
		if !ok {
			return nil, dc.failf("default value ought to have a string type got: %s", jsonTypeName(raw))
		}
		return v, nil
	default:
		// Primitive-backed logical types (numeric time types, unknown logical
		// types that fell back to their primitive) validate against the
		// underlying primitive but keep the JSON-decoded native form.
		return dc.convertPrimitiveBacked(c, raw)
	}
}

// convertPrimitiveBacked validates a default whose codec is a logical type
// layered over a primitive. Range and JSON type are checked, but the value is
// returned unconverted because the logical-type encoders accept the JSON
// numeric kinds directly, preserving the historical default representation.
func (dc *defaultContext) convertPrimitiveBacked(c *Codec, raw interface{}) (interface{}, error) {
	switch primitiveNameOf(c.typeName.fullName) {
	case "boolean":
		if _, ok := raw.(bool); !ok {
			return nil, dc.failf("default value ought to have a bool type, got: %s", jsonTypeName(raw))
		}
		return raw, nil
	case "int":
		v, err := dc.jsonNumber(raw)
		if err != nil {
			return nil, err
		}
		if math.Trunc(v) != v || v < math.MinInt32 || v > math.MaxInt32 {
			return nil, dc.failf("default value ought to be a 32-bit integer; received: %v", raw)
		}
		return raw, nil
	case "long":
		v, err := dc.jsonNumber(raw)
		if err != nil {
			return nil, err
		}
		if math.Trunc(v) != v || v < math.MinInt64 || v > math.MaxInt64 {
			return nil, dc.failf("default value ought to be a 64-bit integer; received: %v", raw)
		}
		return raw, nil
	case "float":
		v, err := dc.jsonNumber(raw)
		if err != nil {
			return nil, dc.failf("default value ought to have a float type got: %s", jsonTypeName(raw))
		}
		if math.IsInf(float64(float32(v)), 0) {
			return nil, dc.failf("default value ought to fit in a 32-bit float; received: %v", raw)
		}
		return raw, nil
	case "double":
		if _, err := dc.jsonNumber(raw); err != nil {
			return nil, dc.failf("default value ought to have a double type got: %s", jsonTypeName(raw))
		}
		return raw, nil
	case "string":
		if _, ok := raw.(string); !ok {
			return nil, dc.failf("default value ought to have a string type got: %s", jsonTypeName(raw))
		}
		return raw, nil
	case "bytes":
		return dc.convertBytes(raw)
	case "null":
		if raw != nil {
			return nil, dc.failf("default value ought to be null; received: %s", jsonTypeName(raw))
		}
		return nil, nil
	default:
		return nil, dc.failf("default value has unsupported schema type: %q", c.typeName)
	}
}

func primitiveNameOf(fullName string) string {
	if i := strings.IndexByte(fullName, '.'); i > 0 {
		return fullName[:i]
	}
	return fullName
}

func (dc *defaultContext) jsonNumber(raw interface{}) (float64, error) {
	v, ok := raw.(float64)
	if !ok {
		return 0, dc.failf("default value ought to have a number type got: %s", jsonTypeName(raw))
	}
	return v, nil
}

func (dc *defaultContext) convertEnum(c *Codec, raw interface{}) (interface{}, error) {
	v, ok := raw.(string)
	if !ok {
		return nil, dc.failf("default value ought to have a string type got: %s", jsonTypeName(raw))
	}
	for _, symbol := range c.enumSymbols {
		if symbol == v {
			return v, nil
		}
	}
	return nil, dc.failf("default value ought to be one of enum symbols %v; received: %q", c.enumSymbols, v)
}

func (dc *defaultContext) convertFixed(c *Codec, raw interface{}) (interface{}, error) {
	v, ok := raw.(string)
	if !ok {
		return nil, dc.failf("default value ought to have a string type got: %s", jsonTypeName(raw))
	}
	b, err := isoLatin1Bytes(v)
	if err != nil {
		return nil, dc.failf("%s", err)
	}
	if count := uint(len(b)); count != *c.fixedSize {
		return nil, dc.failf("default value byte length ought to equal fixed size: %d != %d", count, *c.fixedSize)
	}
	return b, nil
}

func (dc *defaultContext) convertBytes(raw interface{}) (interface{}, error) {
	v, ok := raw.(string)
	if !ok {
		return nil, dc.failf("default value ought to have a string type got: %s", jsonTypeName(raw))
	}
	b, err := isoLatin1Bytes(v)
	if err != nil {
		return nil, dc.failf("%s", err)
	}
	return b, nil
}

// isoLatin1Bytes maps each rune of a JSON string default to a single byte.
// Per the Avro specification, bytes and fixed defaults encode bytes using the
// ISO-8859-1 character mapping, therefore every code point must be in the
// range U+0000 through U+00FF.
func isoLatin1Bytes(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i, r := range s {
		if r > 0xFF {
			return nil, fmt.Errorf("default value byte %d cannot be mapped through ISO-8859-1: code point U+%04X", i, r)
		}
		out = append(out, byte(r))
	}
	return out, nil
}

func (dc *defaultContext) convertArray(c *Codec, raw interface{}) (interface{}, error) {
	items, ok := raw.([]interface{})
	if !ok {
		return nil, dc.failf("default value ought to be an array; received: %s", jsonTypeName(raw))
	}
	out := make([]interface{}, len(items))
	for i, item := range items {
		pop := dc.enter(fmt.Sprintf("[%d]", i))
		v, err := dc.convertExplicit(c.arrayItemCodec, item)
		pop()
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (dc *defaultContext) convertMap(c *Codec, raw interface{}) (interface{}, error) {
	entries, ok := raw.(map[string]interface{})
	if !ok {
		return nil, dc.failf("default value ought to be an object; received: %s", jsonTypeName(raw))
	}
	out := make(map[string]interface{}, len(entries))
	for key, item := range entries {
		pop := dc.enter(fmt.Sprintf("[%q]", key))
		v, err := dc.convertExplicit(c.mapValueCodec, item)
		pop()
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}

// convertUnion validates a union default against the first member schema only,
// per the Avro specification: "default value of a union corresponds to the
// first schema in the union". The resulting native value is wrapped in the
// single-key map union encoders expect; a null first member stays unwrapped.
func (dc *defaultContext) convertUnion(c *Codec, raw interface{}, fromDefault bool) (interface{}, error) {
	first := c.unionMemberCodecs[0]

	var memberValue interface{}
	if raw == nil || (dc.coerceNullString && raw == "null") {
		if first.typeName.short() != "null" {
			return nil, dc.failf("union default value ought to match first member %q; received %s", first.typeName.fullName, jsonTypeName(raw))
		}
		return nil, nil
	}

	pop := dc.enter("{" + first.typeName.fullName + "}")
	v, err := dc.convert(first, raw, fromDefault)
	pop()
	if err != nil {
		return nil, fmt.Errorf("union default value ought to match first member %q: %s", first.typeName.fullName, err)
	}
	memberValue = v

	if memberValue == nil {
		return nil, nil
	}
	return map[string]interface{}{first.typeName.fullName: memberValue}, nil
}

func (dc *defaultContext) convertRecord(c *Codec, raw interface{}, fromDefault bool) (interface{}, error) {
	if raw == nil {
		return nil, dc.failf("default value ought to be an object; received: null")
	}
	fields, ok := raw.(map[string]interface{})
	if !ok {
		return nil, dc.failf("default value ought to be an object; received: %s", jsonTypeName(raw))
	}

	// Inside another field, qualify nested errors with this record's name so
	// the full field path is visible. The root record contributes no segment.
	if len(dc.path) > 0 {
		defer dc.enter(c.typeName.short())()
	}

	if fromDefault {
		for _, active := range dc.defaultExpansion {
			if active == c {
				return nil, dc.failf("record %q default value forms a non-terminating expansion", c.typeName.fullName)
			}
		}
		dc.defaultExpansion = append(dc.defaultExpansion, c)
		defer func() {
			dc.defaultExpansion = dc.defaultExpansion[:len(dc.defaultExpansion)-1]
		}()
	}

	out := make(map[string]interface{}, len(c.recordFields))
	for _, field := range c.recordFields {
		popField := dc.enter(field.name)
		value, present := fields[field.name]
		if present {
			converted, err := dc.convertExplicit(field.codec, value)
			popField()
			if err != nil {
				return nil, err
			}
			out[field.name] = converted
			continue
		}
		if !field.hasDefault {
			return nil, dc.failf("field %q has no default value and none was provided", field.name)
		}
		filled, err := dc.materializeFromDefault(field.codec, field.defaultRaw)
		popField()
		if err != nil {
			return nil, err
		}
		out[field.name] = filled
	}
	return out, nil
}

// convertDecimal handles bytes and fixed decimal logical types. The default is
// a JSON string mapped through ISO-8859-1 to its encoded bytes, then decoded
// through the codec's binary decoder to obtain the native *big.Rat.
func (dc *defaultContext) convertDecimal(c *Codec, raw interface{}) (interface{}, error) {
	v, ok := raw.(string)
	if !ok {
		return nil, dc.failf("default value ought to have a string type got: %s", jsonTypeName(raw))
	}
	encoded, err := bytesBinaryFromNative(nil, v)
	if err != nil {
		return nil, dc.failf("default value ought to be encodable from native binary: %s", err)
	}
	decoded, _, err := c.nativeFromBinary(encoded)
	if err != nil {
		return nil, dc.failf("default value ought to decode: %s", err)
	}
	return decoded, nil
}

// jsonTypeName describes a JSON-decoded value's Go type for error messages.
func jsonTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// NativeFromDefaults returns a new native Go value containing the default
// values declared by this Codec's schema. It is intended for record schemas:
// the result is a map[string]interface{} with one entry per schema field.
//
// Fields without a declared default are not populated with Go zero values;
// instead an error identifies the first field that has neither a default nor a
// provided value.
//
// Every returned map, slice, byte slice and nested record is freshly
// allocated. Repeated calls return independent objects: mutating one result
// never affects other results, later calls, or the schema used to build the
// Codec.
func (c *Codec) NativeFromDefaults() (interface{}, error) {
	if c.recordFields == nil {
		return nil, fmt.Errorf("cannot materialize defaults: schema %q is not a record", c.typeName.fullName)
	}
	dctx := &defaultContext{}
	return dctx.fillRecordFromDefaults(c, nil)
}

// fillRecordFromDefaults builds a native record map, using explicit values
// from datum when supplied and the schema-declared defaults otherwise.
// Explicitly provided values are deep copied as well, so the caller cannot
// accidentally share mutable sub-structure with the returned object.
func (dc *defaultContext) fillRecordFromDefaults(c *Codec, datum map[string]interface{}) (interface{}, error) {
	for _, active := range dc.defaultExpansion {
		if active == c {
			return nil, dc.failf("record %q default value forms a non-terminating expansion", c.typeName.fullName)
		}
	}
	dc.defaultExpansion = append(dc.defaultExpansion, c)
	defer func() {
		dc.defaultExpansion = dc.defaultExpansion[:len(dc.defaultExpansion)-1]
	}()

	out := make(map[string]interface{}, len(c.recordFields))
	for _, field := range c.recordFields {
		popField := dc.enter(field.name)
		if datum != nil {
			if value, present := datum[field.name]; present {
				copied, err := deepCopyNativeWith(dc, field.codec, value)
				popField()
				if err != nil {
					return nil, err
				}
				out[field.name] = copied
				continue
			}
		}
		if !field.hasDefault {
			err := dc.failf("field %q has no default value and none was provided", field.name)
			popField()
			return nil, err
		}
		filled, err := dc.fillDefault(field.codec, field.defaultNative)
		popField()
		if err != nil {
			return nil, err
		}
		out[field.name] = filled
	}
	return out, nil
}

// fillDefault returns an independent copy of an already-converted native
// default value, recursively materializing nested records and containers.
func (dc *defaultContext) fillDefault(c *Codec, native interface{}) (interface{}, error) {
	return deepCopyNativeWith(dc, c, native)
}

// deepCopyNative returns an independent deep copy of a native Go value for the
// supplied codec, without tracking a default-expansion path. It is used when
// inserting cached defaults during ordinary decode/encode paths.
func deepCopyNative(c *Codec, native interface{}) interface{} {
	dctx := &defaultContext{}
	out, err := deepCopyNativeWith(dctx, c, native)
	if err != nil {
		// Cached defaults were fully validated at construction time, so a
		// failure here indicates a programming error rather than bad input.
		panic(err)
	}
	return out
}

// deepCopyNativeWith copies native, following the schema structure so that
// records fill missing fields from their declared defaults and every mutable
// value gets fresh storage.
func deepCopyNativeWith(dc *defaultContext, c *Codec, native interface{}) (interface{}, error) {
	switch {
	case c.recordFields != nil:
		m, ok := native.(map[string]interface{})
		if !ok {
			return nil, dc.failf("expected map[string]interface{}; received: %T", native)
		}
		return dc.fillRecordFromDefaults(c, m)
	case c.arrayItemCodec != nil:
		items, err := convertArray(native)
		if err != nil {
			return nil, dc.failf("%s", err)
		}
		out := make([]interface{}, len(items))
		for i, item := range items {
			pop := dc.enter(fmt.Sprintf("[%d]", i))
			copied, err := deepCopyNativeWith(dc, c.arrayItemCodec, item)
			pop()
			if err != nil {
				return nil, err
			}
			out[i] = copied
		}
		return out, nil
	case c.mapValueCodec != nil:
		entries, err := convertMap(native)
		if err != nil {
			return nil, dc.failf("%s", err)
		}
		out := make(map[string]interface{}, len(entries))
		for key, item := range entries {
			pop := dc.enter(fmt.Sprintf("[%q]", key))
			copied, err := deepCopyNativeWith(dc, c.mapValueCodec, item)
			pop()
			if err != nil {
				return nil, err
			}
			out[key] = copied
		}
		return out, nil
	case c.unionMemberCodecs != nil:
		if native == nil {
			return nil, nil
		}
		m, ok := native.(map[string]interface{})
		if !ok || len(m) != 1 {
			return nil, dc.failf("expected union value; received: %T", native)
		}
		for key, value := range m {
			member, found := unionMemberByName(c, key)
			if !found {
				return nil, dc.failf("unknown union member %q", key)
			}
			pop := dc.enter("{" + key + "}")
			copied, err := deepCopyNativeWith(dc, member, value)
			pop()
			if err != nil {
				return nil, err
			}
			if copied == nil {
				return nil, nil
			}
			return map[string]interface{}{key: copied}, nil
		}
		return nil, nil
	}

	// Immutable and leaf native types are returned as-is. Byte slices are the
	// one mutable leaf type and must be copied.
	if b, ok := native.([]byte); ok {
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil
	}
	if r, ok := native.(*big.Rat); ok {
		return new(big.Rat).Set(r), nil
	}
	return native, nil
}

// unionMemberByName finds a union member codec by its full name.
func unionMemberByName(c *Codec, fullName string) (*Codec, bool) {
	for _, member := range c.unionMemberCodecs {
		if member.typeName.fullName == fullName {
			return member, true
		}
	}
	return nil, false
}
