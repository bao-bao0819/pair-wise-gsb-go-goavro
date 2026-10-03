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
	"math"
	"strings"
)

// validateRecordDefaults is invoked after the entire type graph for a schema
// has been built. It validates every field default value field by field,
// coerces each valid default into its native Go form, and rejects defaults
// whose expansion never terminates (for example a recursive record whose
// default keeps requiring another copy of itself).
func validateRecordDefaults(root *Codec) error {
	var records []*Codec
	collectRecordCodecs(root, map[*Codec]bool{}, &records)
	for _, rc := range records {
		for i := range rc.defaultFields {
			fd := &rc.defaultFields[i]
			if !fd.has {
				continue
			}
			env := &coerceEnv{stringNull: root.defaultStringNull}
			raw, err := coerceDefault(fd.codec, fd.raw, env)
			if err != nil {
				return fmt.Errorf("record %q field %q: default value: %s", rc.typeName, fd.name, err)
			}
			native, err := materializeDefault(fd.codec, raw, nil)
			if err != nil {
				return fmt.Errorf("record %q field %q: default value: %s", rc.typeName, fd.name, err)
			}
			fd.raw = raw
			fd.native = native
		}
	}
	return nil
}

// coerceEnv carries per-build state used while validating defaults: the set
// of records currently being expanded (cycle detection) and whether the
// string literal "null" must be treated as a null union value.
type coerceEnv struct {
	active     map[*Codec]bool
	stringNull bool
	path       []string
}

// collectRecordCodecs walks the codec type graph and returns each record
// codec exactly once. The walk tolerates recursive references.
func collectRecordCodecs(c *Codec, visited map[*Codec]bool, records *[]*Codec) {
	if c == nil || visited[c] {
		return
	}
	visited[c] = true
	if c.defaultKind == defaultRecord {
		*records = append(*records, c)
		for i := range c.defaultFields {
			collectRecordCodecs(c.defaultFields[i].codec, visited, records)
		}
		return
	}
	if c.defaultElements != nil {
		collectRecordCodecs(c.defaultElements, visited, records)
	}
	if c.defaultValues != nil {
		collectRecordCodecs(c.defaultValues, visited, records)
	}
	if c.defaultUnionFirst != nil {
		collectRecordCodecs(c.defaultUnionFirst, visited, records)
	}
	for _, member := range c.defaultUnionAll {
		collectRecordCodecs(member, visited, records)
	}
}

// coerceDefault validates a raw JSON default value against its schema and
// returns a canonical raw value that can be consumed by materializeDefault.
// env tracks records currently being expanded (to detect defaults that
// recurse forever) and parser compatibility options.
func coerceDefault(c *Codec, raw interface{}, env *coerceEnv) (interface{}, error) {
	switch c.defaultKind {
	case defaultRecord:
		return coerceRecordDefault(c, raw, env)
	case defaultUnion:
		return coerceUnionDefault(c, raw, env)
	case defaultArray:
		return coerceArrayDefault(c, raw, env)
	case defaultMap:
		return coerceMapDefault(c, raw, env)
	case defaultEnum:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("ought to be a string member of symbols, got: %T", raw)
		}
		for _, symbol := range c.defaultSymbols {
			if symbol == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("ought to be member of symbols %v; got: %q", c.defaultSymbols, s)
	case defaultFixed:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("ought to be a string, got: %T", raw)
		}
		if len(s) != c.defaultSize {
			return nil, fmt.Errorf("fixed size ought to equal schema size: %d != %d", len(s), c.defaultSize)
		}
		return s, nil
	case defaultLogical:
		if _, err := c.defaultToNative(raw); err != nil {
			return nil, err
		}
		return raw, nil
	default:
		return coercePrimitiveDefault(c, raw)
	}
}

func coerceRecordDefault(c *Codec, raw interface{}, env *coerceEnv) (interface{}, error) {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("ought to be a JSON object, got: %T", raw)
	}
	if env == nil {
		env = &coerceEnv{}
	}
	if env.active == nil {
		env.active = map[*Codec]bool{}
	}
	if env.active[c] {
		return nil, fmt.Errorf("default expansion never terminates at field path %q; record %q re-entered through its own default", strings.Join(env.path, "."), c.typeName)
	}
	env.active[c] = true
	defer delete(env.active, c)
	out := make(map[string]interface{}, len(c.defaultFields))
	for _, fd := range c.defaultFields {
		v, ok := m[fd.name]
		if !ok {
			// Fields without a default remain unspecified; they must not be
			// silently filled with Go zero values.
			if !fd.has {
				continue
			}
			v = fd.raw
		}
		env.path = append(env.path, fd.name)
		coerced, err := coerceDefault(fd.codec, v, env)
		env.path = env.path[:len(env.path)-1]
		if err != nil {
			return nil, fmt.Errorf("field %q: %s", fd.name, err)
		}
		out[fd.name] = coerced
	}
	return out, nil
}

func coerceUnionDefault(c *Codec, raw interface{}, env *coerceEnv) (interface{}, error) {
	// Avro: a union default matches the first branch of the union.
	first := c.defaultUnionFirst
	if env != nil && env.stringNull {
		if s, ok := raw.(string); ok && s == "null" {
			raw = nil
		}
	}
	if raw == nil {
		if first.typeName.short() == "null" {
			return nil, nil
		}
		return nil, fmt.Errorf("default value ought to encode using field schema: ought to match first union branch %q, got: null", first.typeName)
	}
	coerced, err := coerceDefault(first, raw, env)
	if err != nil {
		return nil, fmt.Errorf("default value ought to encode using field schema: ought to match first union branch %q: %s", first.typeName, err)
	}
	return map[string]interface{}{c.defaultUnionName: coerced}, nil
}

func coerceArrayDefault(c *Codec, raw interface{}, env *coerceEnv) (interface{}, error) {
	items, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("ought to be a JSON array, got: %T", raw)
	}
	out := make([]interface{}, len(items))
	for i, item := range items {
		coerced, err := coerceDefault(c.defaultElements, item, env)
		if err != nil {
			return nil, fmt.Errorf("item %d: %s", i+1, err)
		}
		out[i] = coerced
	}
	return out, nil
}

func coerceMapDefault(c *Codec, raw interface{}, env *coerceEnv) (interface{}, error) {
	values, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("ought to be a JSON object, got: %T", raw)
	}
	out := make(map[string]interface{}, len(values))
	for key, value := range values {
		coerced, err := coerceDefault(c.defaultValues, value, env)
		if err != nil {
			return nil, fmt.Errorf("key %q: %s", key, err)
		}
		out[key] = coerced
	}
	return out, nil
}

// coercePrimitiveDefault validates defaults for primitives, enforcing exact
// JSON mappings and numeric ranges instead of relying on lossy Go casts.
func coercePrimitiveDefault(c *Codec, raw interface{}) (interface{}, error) {
	switch c.typeName.short() {
	case "null":
		if raw != nil {
			return nil, fmt.Errorf("ought to be null, got: %T", raw)
		}
		return nil, nil
	case "boolean":
		v, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("ought to have a bool type, got: %T", raw)
		}
		return v, nil
	case "string":
		v, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("ought to have a string type, got: %T", raw)
		}
		return v, nil
	case "int":
		v, ok := raw.(float64)
		if !ok {
			return nil, fmt.Errorf("default value ought to have a number type got: %T", raw)
		}
		cast := float64(int32(v))
		if math.Trunc(v) != v || v != cast {
			return nil, fmt.Errorf("default value ought to encode using field schema: int32 out of range: %v", v)
		}
		return v, nil
	case "long":
		v, ok := raw.(float64)
		if !ok {
			return nil, fmt.Errorf("default value ought to have a number type got: %T", raw)
		}
		cast := float64(int64(v))
		if math.Trunc(v) != v || v != cast {
			return nil, fmt.Errorf("default value ought to encode using field schema: int64 out of range: %v", v)
		}
		return v, nil
	case "float":
		v, ok := raw.(float64)
		if !ok {
			return nil, fmt.Errorf("default value ought to have a float type got: %T", raw)
		}
		if math.IsInf(v, 0) {
			return nil, fmt.Errorf("default value ought to encode using field schema: float is infinite")
		}
		if converted := float32(v); !math.IsNaN(v) && math.IsInf(float64(converted), 0) {
			return nil, fmt.Errorf("default value ought to encode using field schema: float32 out of range: %v", v)
		}
		return v, nil
	case "double":
		if _, ok := raw.(float64); !ok {
			return nil, fmt.Errorf("default value ought to have a double type got: %T", raw)
		}
		return raw, nil
	case "bytes":
		v, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("default value ought to have a string type got: %T", raw)
		}
		return v, nil
	default:
		// Unknown leaf type: accept it as-is and leave structural checks to
		// the codec's own encoder when the value is used.
		return raw, nil
	}
}

// materializeDefault turns a coerced raw default value into a fresh, fully
// independent native Go value. Values returned across calls never share
// writable maps, slices or byte slices.
func materializeDefault(c *Codec, raw interface{}, active map[*Codec]bool) (interface{}, error) {
	switch c.defaultKind {
	case defaultRecord:
		return materializeRecordDefault(c, raw, active)
	case defaultUnion:
		if raw == nil {
			return nil, nil
		}
		m, ok := raw.(map[string]interface{})
		if !ok || len(m) != 1 {
			return nil, fmt.Errorf("malformed union default: %v", raw)
		}
		inner, ok := m[c.defaultUnionName]
		if !ok {
			return nil, fmt.Errorf("union default key ought to be %q: %v", c.defaultUnionName, raw)
		}
		value, err := materializeDefault(c.defaultUnionFirst, inner, active)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{c.defaultUnionName: value}, nil
	case defaultArray:
		items := raw.([]interface{})
		out := make([]interface{}, len(items))
		for i, item := range items {
			value, err := materializeDefault(c.defaultElements, item, active)
			if err != nil {
				return nil, fmt.Errorf("item %d: %s", i+1, err)
			}
			out[i] = value
		}
		return out, nil
	case defaultMap:
		values := raw.(map[string]interface{})
		out := make(map[string]interface{}, len(values))
		for key, value := range values {
			materialized, err := materializeDefault(c.defaultValues, value, active)
			if err != nil {
				return nil, fmt.Errorf("key %q: %s", key, err)
			}
			out[key] = materialized
		}
		return out, nil
	case defaultEnum:
		return raw.(string), nil
	case defaultFixed:
		// Allocate a fresh byte slice per materialization.
		return []byte(raw.(string)), nil
	case defaultLogical:
		return c.defaultToNative(raw)
	default:
		return materializePrimitiveDefault(c, raw)
	}
}

func materializeRecordDefault(c *Codec, raw interface{}, active map[*Codec]bool) (interface{}, error) {
	if active == nil {
		active = map[*Codec]bool{}
	}
	if active[c] {
		return nil, fmt.Errorf("default expansion never terminates; record %q re-entered through its own default", c.typeName)
	}
	active[c] = true
	defer delete(active, c)

	source, _ := raw.(map[string]interface{})
	out := make(map[string]interface{}, len(c.defaultFields))
	for _, fd := range c.defaultFields {
		value, ok := source[fd.name]
		if !ok {
			if raw == nil && fd.has {
				// Materializing a standalone object: use the field's own
				// declared default.
				value = fd.raw
			} else {
				continue // field without a default stays absent from the object
			}
		}
		materialized, err := materializeDefault(fd.codec, value, active)
		if err != nil {
			return nil, fmt.Errorf("field %q: %s", fd.name, err)
		}
		out[fd.name] = materialized
	}
	return out, nil
}

func materializePrimitiveDefault(c *Codec, raw interface{}) (interface{}, error) {
	switch c.typeName.short() {
	case "int":
		return int32(raw.(float64)), nil
	case "long":
		return int64(raw.(float64)), nil
	case "float":
		return float32(raw.(float64)), nil
	case "bytes":
		return []byte(raw.(string)), nil
	default:
		// null, boolean, string, double and unknown leaf types are immutable
		// scalars and may be returned directly.
		return raw, nil
	}
}
