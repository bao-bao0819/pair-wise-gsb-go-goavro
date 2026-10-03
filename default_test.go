// Copyright [2019] LinkedIn Corp. Licensed under the Apache License, Version
// 2.0 (the "License"); you may not use this file except in compliance with the
// License.  You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0

package goavro

import (
	"strings"
	"testing"
)

// A default declared several records deep must be located at build time with
// a path that names every record and field traversed.
func TestDefaultsDeepInvalidDefaultLocatedAtBuild(t *testing.T) {
	schema := `
{
  "type": "record",
  "name": "Outer",
  "fields": [
    {"name": "middle", "type": {
      "type": "record",
      "name": "Middle",
      "fields": [
        {"name": "inner", "type": {
          "type": "record",
          "name": "Inner",
          "fields": [
            {"name": "color", "type": {
              "type": "enum",
              "name": "Color",
              "symbols": ["RED", "GREEN", "BLUE"]
            }, "default": "PURPLE"}
          ]
        }, "default": {"color": "PURPLE"}}
      ]
    }, "default": {"inner": {"color": "PURPLE"}}}
  ]
}`
	_, err := NewCodec(schema)
	if err == nil {
		t.Fatal("expected build error for deeply nested invalid enum default")
	}
	for _, want := range []string{`record "Outer"`, `field "middle"`, "field \"inner\"", "field \"color\"", "member of symbols"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestDefaultsRuleEnforcement(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{
			name:   "enum symbol",
			schema: `{"type":"record","name":"R","fields":[{"name":"e","type":{"type":"enum","name":"E","symbols":["A","B"]},"default":"C"}]}`,
		},
		{
			name:   "fixed length too short",
			schema: `{"type":"record","name":"R","fields":[{"name":"f","type":{"type":"fixed","name":"F","size":4},"default":"ab"}]}`,
		},
		{
			name:   "fixed length too long",
			schema: `{"type":"record","name":"R","fields":[{"name":"f","type":{"type":"fixed","name":"F","size":2},"default":"abcd"}]}`,
		},
		{
			name:   "bytes wrong json mapping",
			schema: `{"type":"record","name":"R","fields":[{"name":"b","type":"bytes","default":12}]}`,
		},
		{
			name:   "int range high",
			schema: `{"type":"record","name":"R","fields":[{"name":"n","type":"int","default":2147483648}]}`,
		},
		{
			name:   "int range low",
			schema: `{"type":"record","name":"R","fields":[{"name":"n","type":"int","default":-2147483649}]}`,
		},
		{
			name:   "long range",
			schema: `{"type":"record","name":"R","fields":[{"name":"n","type":"long","default":9223372036854775808}]}`,
		},
		{
			name:   "float range",
			schema: `{"type":"record","name":"R","fields":[{"name":"n","type":"float","default":1e40}]}`,
		},
		{
			name:   "union default must match first branch (null on [int,null])",
			schema: `{"type":"record","name":"R","fields":[{"name":"u","type":["int","null"],"default":null}]}`,
		},
		{
			name:   "union default must match first branch (int on [null,int])",
			schema: `{"type":"record","name":"R","fields":[{"name":"u","type":["null","int"],"default":13}]}`,
		},
		{
			name:   "boolean wrong json mapping",
			schema: `{"type":"record","name":"R","fields":[{"name":"b","type":"boolean","default":"true"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCodec(tc.schema); err == nil {
				t.Fatal("expected NewCodec to reject invalid default")
			}
		})
	}

	// Valid counterparts build successfully.
	valid := []string{
		`{"type":"record","name":"R","fields":[{"name":"e","type":{"type":"enum","name":"E","symbols":["A","B"]},"default":"A"}]}`,
		`{"type":"record","name":"R","fields":[{"name":"f","type":{"type":"fixed","name":"F","size":4},"default":"abcd"}]}`,
		`{"type":"record","name":"R","fields":[{"name":"b","type":"bytes","default":"hi"}]}`,
		`{"type":"record","name":"R","fields":[{"name":"n","type":"int","default":2147483647}]}`,
		`{"type":"record","name":"R","fields":[{"name":"u","type":["int","null"],"default":13}]}`,
		`{"type":"record","name":"R","fields":[{"name":"u","type":["null","int"],"default":null}]}`,
	}
	for i, schema := range valid {
		if _, err := NewCodec(schema); err != nil {
			t.Fatalf("valid case %d unexpectedly rejected: %s", i, err)
		}
	}
}

// Fields without a declared default stay "not provided": the materialized map
// simply omits them instead of inserting Go zero values.
func TestMaterializeOmitsFieldsWithoutDefaults(t *testing.T) {
	schema := `
{
  "type": "record",
  "name": "R",
  "fields": [
    {"name": "withDefault", "type": "int", "default": 7},
    {"name": "withoutDefault", "type": "int"},
    {"name": "nested", "type": {
      "type": "record",
      "name": "N",
      "fields": [
        {"name": "a", "type": "string", "default": "x"},
        {"name": "b", "type": "string"}
      ]
    }, "default": {"a": "x"}}
  ]
}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := codec.MaterializeDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["withoutDefault"]; ok {
		t.Errorf("field without default ought to be omitted, got: %v", obj)
	}
	nested := obj["nested"].(map[string]interface{})
	if _, ok := nested["b"]; ok {
		t.Errorf("nested field without default ought to be omitted, got: %v", nested)
	}
	if got := obj["withDefault"]; got != int32(7) {
		t.Errorf("GOT: %v; WANT: int32(7)", got)
	}
}

// A named recursive type is legal; it is the default expansion that must be
// judged separately for termination.
func TestNamedRecursionAcceptedButNonTerminatingDefaultRejected(t *testing.T) {
	// LongList is recursive, but the null-first union terminates expansion.
	recursive := `
{
  "type": "record",
  "name": "LongList",
  "fields": [
    {"name": "next", "type": ["null", "LongList"], "default": null}
  ]
}`
	codec, err := NewCodec(recursive)
	if err != nil {
		t.Fatalf("named recursion ought to be allowed: %s", err)
	}
	obj, err := codec.MaterializeDefaults()
	if err != nil {
		t.Fatal(err)
	}
	if obj["next"] != nil {
		t.Errorf("null-first recursive default ought to materialize as nil: %v", obj)
	}

	// Node directly contains itself and declares a default object, forcing
	// endless expansion.
	nonTerminating := `
{
  "type": "record",
  "name": "Node",
  "fields": [
    {"name": "child", "type": "Node", "default": {}}
  ]
}`
	_, err = NewCodec(nonTerminating)
	if err == nil {
		t.Fatal("expected build error for non-terminating default expansion")
	}
	for _, want := range []string{`field "child"`, "never terminates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

// Two consecutive materializations are independent of each other and of any
// subsequently materialized object: maps, slices, byte slices and nested
// records are freshly allocated every time.
func TestMaterializeDefaultsAreIndependent(t *testing.T) {
	schema := `
{
  "type": "record",
  "name": "R",
  "fields": [
    {"name": "bytes", "type": "bytes", "default": "abc"},
    {"name": "fixed", "type": {"type": "fixed", "name": "F", "size": 2}, "default": "xy"},
    {"name": "tags", "type": {"type": "array", "items": "string"}, "default": ["a", "b"]},
    {"name": "lookup", "type": {"type": "map", "values": "int"}, "default": {"k": 1}},
    {"name": "child", "type": {
      "type": "record",
      "name": "C",
      "fields": [
        {"name": "deep", "type": {"type": "array", "items": "bytes"}, "default": ["q"]}
      ]
    }, "default": {"deep": ["q"]}},
    {"name": "opt", "type": ["null", "string"], "default": null}
  ]
}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatal(err)
	}

	first, err := codec.MaterializeDefaults()
	if err != nil {
		t.Fatal(err)
	}
	second, err := codec.MaterializeDefaults()
	if err != nil {
		t.Fatal(err)
	}

	// Mutate the first object aggressively, including nested writable state.
	first["bytes"].([]byte)[0] = 'Z'
	first["fixed"].([]byte)[0] = 'Q'
	first["tags"].([]interface{})[0] = "mutated"
	first["lookup"].(map[string]interface{})["k"] = int32(99)
	firstChild := first["child"].(map[string]interface{})
	firstChild["deep"].([]interface{})[0].([]byte)[0] = 'M'

	assertPristine := func(t *testing.T, obj map[string]interface{}) {
		if got := string(obj["bytes"].([]byte)); got != "abc" {
			t.Errorf("bytes: GOT %q; WANT %q", got, "abc")
		}
		if got := string(obj["fixed"].([]byte)); got != "xy" {
			t.Errorf("fixed: GOT %q; WANT %q", got, "xy")
		}
		if got := obj["tags"].([]interface{})[0]; got != "a" {
			t.Errorf("tags[0]: GOT %v; WANT a", got)
		}
		if got := obj["lookup"].(map[string]interface{})["k"]; got != int32(1) {
			t.Errorf("lookup[k]: GOT %v; WANT int32(1)", got)
		}
		child := obj["child"].(map[string]interface{})
		if got := string(child["deep"].([]interface{})[0].([]byte)); got != "q" {
			t.Errorf("child.deep[0]: GOT %q; WANT q", got)
		}
		if obj["opt"] != nil {
			t.Errorf("opt: GOT %v; WANT nil", obj["opt"])
		}
	}

	assertPristine(t, second)

	// A third object generated after the mutation is pristine too.
	third, err := codec.MaterializeDefaults()
	if err != nil {
		t.Fatal(err)
	}
	assertPristine(t, third)

	// Backing storage must not be shared even when values look identical.
	if &first["tags"].([]interface{})[0] == &second["tags"].([]interface{})[0] {
		t.Error("array backing storage unexpectedly shared")
	}
}

// Materializing never rewrites the schema JSON the codec was created with.
func TestSchemaJSONNotMutatedByDefaults(t *testing.T) {
	schema := `{"type":"record","name":"R","fields":[{"name":"e","type":{"type":"enum","name":"E","symbols":["A"]},"default":"A"}]}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.MaterializeDefaults(); err != nil {
		t.Fatal(err)
	}
	if got := codec.Schema(); got != schema {
		t.Errorf("schema changed:\nGOT:  %s\nWANT: %s", got, schema)
	}
}

func TestMaterializeDefaultsRejectsNonRecord(t *testing.T) {
	codec, err := NewCodec(`"int"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.MaterializeDefaults(); err == nil {
		t.Fatal("expected error when materializing a non-record schema")
	}
}

func TestDefaultsInsideMapAndArrayLocated(t *testing.T) {
	mapSchema := `{"type":"record","name":"R","fields":[{"name":"m","type":{"type":"map","values":{"type":"fixed","name":"F","size":2}},"default":{"k":"abc"}}]}`
	if _, err := NewCodec(mapSchema); err == nil ||
		!strings.Contains(err.Error(), `key "k"`) {
		t.Fatalf("expected map key path in error, got: %v", err)
	}

	arraySchema := `{"type":"record","name":"R","fields":[{"name":"a","type":{"type":"array","items":"int"},"default":[1,2,9999999999]}]}`
	if _, err := NewCodec(arraySchema); err == nil ||
		!strings.Contains(err.Error(), "item 3") {
		t.Fatalf("expected array item path in error, got: %v", err)
	}
}

func TestMutuallyRecursiveNonTerminatingDefault(t *testing.T) {
	schema := `
{
  "type": "record",
  "name": "A",
  "fields": [
    {"name": "b", "type": {
      "type": "record",
      "name": "B",
      "fields": [
        {"name": "a", "type": "A", "default": {}}
      ]
    }, "default": {}}
  ]
}`
	_, err := NewCodec(schema)
	if err == nil {
		t.Fatal("expected build error for mutually recursive non-terminating default")
	}
	for _, want := range []string{`field "b"`, `field "a"`, "never terminates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}
