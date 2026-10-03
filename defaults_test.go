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
	"strings"
	"testing"
)

// TestDefaultValidationAtBuildTime verifies that invalid default values are
// rejected when the codec is built, rather than the first time data happens to
// omit a field. Each rule from the Avro specification is exercised: union
// branch selection, enum symbols, fixed length, bytes ISO-8859-1 mapping and
// numeric ranges.
func TestDefaultValidationAtBuildTime(t *testing.T) {
	t.Run("union default must match first branch", func(t *testing.T) {
		// value 13 matches "int" but the first branch is "null"
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":["null","int"],"default":13}]}`,
			`union default value ought to match first member "null"`)
		// null matches "null" only when it is the first branch
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":["int","null"],"default":null}]}`,
			`union default value ought to match first member "int"`)
		// default matching first branch succeeds
		testSchemaValid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":["null","int"],"default":null}]}`)
		testSchemaValid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":["int","null"],"default":13}]}`)
	})

	t.Run("enum symbol", func(t *testing.T) {
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":{"type":"enum","name":"E","symbols":["A","B"]},"default":"C"}]}`,
			"default value ought to be one of enum symbols")
		testSchemaValid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":{"type":"enum","name":"E","symbols":["A","B"]},"default":"A"}]}`)
	})

	t.Run("fixed length", func(t *testing.T) {
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":{"type":"fixed","name":"F","size":4},"default":"ab"}]}`,
			"default value byte length ought to equal fixed size: 2 != 4")
		testSchemaValid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":{"type":"fixed","name":"F","size":4},"default":"abcd"}]}`)
	})

	t.Run("bytes character mapping", func(t *testing.T) {
		// code point above U+00FF cannot be mapped to a single byte
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":"bytes","default":"\u0100"}]}`,
			"cannot be mapped through ISO-8859-1")
		// U+0000..U+00FF are each mapped to exactly one byte
		testSchemaValid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":"bytes","default":"\u00ff\u0080\u0000"}]}`)
	})

	t.Run("numeric ranges", func(t *testing.T) {
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":"int","default":2147483648}]}`,
			"default value ought to be a 32-bit integer")
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":"int","default":1.5}]}`,
			"default value ought to be a 32-bit integer")
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":"long","default":99999999999999999999}]}`,
			"default value ought to be a 64-bit integer")
		testSchemaInvalid(t,
			`{"type":"record","name":"r1","fields":[{"name":"f1","type":"float","default":1e40}]}`,
			"default value ought to fit in a 32-bit float")
	})
}

// TestDeepInvalidDefaultLocatedAtBuildTime verifies that a default buried
// several schema levels deep is rejected when the codec is built, and the
// error message identifies the offending field path.
func TestDeepInvalidDefaultLocatedAtBuildTime(t *testing.T) {
	schema := `{
	  "type":"record","name":"Outer","fields":[
	    {"name":"inner","type":{
	      "type":"record","name":"Inner","fields":[
	        {"name":"items","type":{"type":"array","items":{
	          "type":"record","name":"Leaf","fields":[
	            {"name":"color","type":{"type":"enum","name":"Color","symbols":["RED","GREEN"]},"default":"PURPLE"}
	          ]
	        }},"default":[]}
	      ]
	    },"default":{}}
	  ]
	}`
	_, err := NewCodec(schema)
	if err == nil {
		t.Fatal("GOT: nil error; WANT: build error locating the deep invalid default")
	}
	msg := err.Error()
	for _, want := range []string{"Leaf", "color", "PURPLE", "one of enum symbols"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// TestNonTerminatingDefaultExpansion verifies that a recursive schema whose
// default values never terminate is rejected at build time with the field
// path, while a recursive default that terminates (a null branch) is valid.
func TestNonTerminatingDefaultExpansion(t *testing.T) {
	// child is a union whose first branch is Node, and its default is an empty
	// object, forcing Node.child.Node.child... forever.
	cyclic := `{"type":"record","name":"Node","fields":[
		{"name":"child","type":["Node","null"],"default":{}}
	]}`
	_, err := NewCodec(cyclic)
	if err == nil {
		t.Fatal("GOT: nil error; WANT: non-terminating default expansion error")
	}
	if !strings.Contains(err.Error(), "non-terminating expansion") {
		t.Errorf("GOT: %v; WANT: non-terminating expansion", err)
	}
	if !strings.Contains(err.Error(), "child") {
		t.Errorf("error ought to report the field path; GOT: %v", err)
	}

	// Same recursion but the first branch is null and the default is null:
	// expansion terminates immediately and the codec builds.
	terminating := `{"type":"record","name":"Node","fields":[
		{"name":"value","type":"int","default":7},
		{"name":"child","type":["null","Node"],"default":null}
	]}`
	testSchemaValid(t, terminating)
}

// TestFieldWithoutDefaultStaysAbsent verifies that a field lacking a default
// declaration is not silently filled with a Go zero value at build time or
// when materializing.
func TestFieldWithoutDefaultStaysAbsent(t *testing.T) {
	schema := `{"type":"record","name":"r1","fields":[{"name":"required","type":"int"}]}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatalf("schema without defaults ought to build; GOT: %v", err)
	}
	if _, err := codec.NativeFromDefaults(); err == nil {
		t.Fatal("GOT: nil error; WANT: error because field has no default")
	}
}

// TestNativeFromDefaultsIndependence materializes defaults twice, mutates the
// first result, and asserts the second result and a later materialization are
// unaffected. This covers independent maps, slices, byte slices and nested
// records, plus preservation of the original schema JSON.
func TestNativeFromDefaultsIndependence(t *testing.T) {
	schema := `{
	  "type":"record","name":"Node","fields":[
	    {"name":"tag","type":"string","default":"x"},
	    {"name":"count","type":"int","default":5},
	    {"name":"next","type":["null","Node"],"default":null},
	    {"name":"nums","type":{"type":"array","items":"int"},"default":[1,2]},
	    {"name":"meta","type":{"type":"map","values":"int"},"default":{"a":1}},
	    {"name":"blob","type":{"type":"fixed","name":"Fx","size":2},"default":"ab"}
	  ]
	}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}

	first, err := codec.NativeFromDefaults()
	if err != nil {
		t.Fatalf("first materialization: %v", err)
	}
	second, err := codec.NativeFromDefaults()
	if err != nil {
		t.Fatalf("second materialization: %v", err)
	}

	firstMap := first.(map[string]interface{})
	secondMap := second.(map[string]interface{})

	// Mutate every mutable kind in the first result.
	firstMap["tag"] = "changed"
	firstMap["nums"].([]interface{})[0] = int32(99)
	firstMap["meta"].(map[string]interface{})["a"] = int32(7)
	firstMap["blob"].([]byte)[0] = 'Z'

	if got := secondMap["tag"]; got != "x" {
		t.Errorf("second tag: GOT: %v; WANT: x", got)
	}
	if got := secondMap["nums"].([]interface{})[0]; got != int32(1) {
		t.Errorf("second nums[0]: GOT: %v; WANT: 1", got)
	}
	if got := secondMap["meta"].(map[string]interface{})["a"]; got != int32(1) {
		t.Errorf("second meta[a]: GOT: %v; WANT: 1", got)
	}
	if got := secondMap["blob"].([]byte)[0]; got != byte('a') {
		t.Errorf("second blob[0]: GOT: %q; WANT: %q", got, 'a')
	}

	// A fresh materialization after the mutation must also be pristine.
	third, err := codec.NativeFromDefaults()
	if err != nil {
		t.Fatalf("third materialization: %v", err)
	}
	thirdMap := third.(map[string]interface{})
	if got := thirdMap["nums"].([]interface{})[0]; got != int32(1) {
		t.Errorf("third nums[0]: GOT: %v; WANT: 1", got)
	}
	if got := thirdMap["blob"].([]byte); string(got) != "ab" {
		t.Errorf("third blob: GOT: %q; WANT: ab", got)
	}

	// The original schema JSON must not be rewritten.
	if got := codec.Schema(); got != schema {
		t.Errorf("schema JSON was modified")
	}
}

// TestNativeFromDefaultsValues verifies the native Go types and values
// produced by materializing a fully defaulted record.
func TestNativeFromDefaultsValues(t *testing.T) {
	schema := `{"type":"record","name":"r1","fields":[
		{"name":"someBoolean","type":"boolean","default":true},
		{"name":"someBytes","type":"bytes","default":"0"},
		{"name":"someDouble","type":"double","default":0},
		{"name":"someFloat","type":"float","default":0},
		{"name":"someInt","type":"int","default":13},
		{"name":"someLong","type":"long","default":9000000000},
		{"name":"someString","type":"string","default":"hello"},
		{"name":"someEnum","type":{"type":"enum","name":"E","symbols":["A","B"]},"default":"B"}
	]}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	datum, err := codec.NativeFromDefaults()
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	m := datum.(map[string]interface{})
	if got, want := m["someBoolean"].(bool), true; got != want {
		t.Errorf("someBoolean: GOT: %v; WANT: %v", got, want)
	}
	if got := m["someBytes"]; string(got.([]byte)) != "0" {
		t.Errorf("someBytes: GOT: %v; WANT: []byte(\"0\")", got)
	}
	if _, ok := m["someDouble"].(float64); !ok {
		t.Errorf("someDouble: GOT: %T; WANT: float64", m["someDouble"])
	}
	if _, ok := m["someFloat"].(float32); !ok {
		t.Errorf("someFloat: GOT: %T; WANT: float32", m["someFloat"])
	}
	if got, want := m["someInt"].(int32), int32(13); got != want {
		t.Errorf("someInt: GOT: %v; WANT: %v", got, want)
	}
	if got, want := m["someLong"].(int64), int64(9000000000); got != want {
		t.Errorf("someLong: GOT: %v; WANT: %v", got, want)
	}
	if got, want := m["someString"].(string), "hello"; got != want {
		t.Errorf("someString: GOT: %v; WANT: %v", got, want)
	}
	if got, want := m["someEnum"].(string), "B"; got != want {
		t.Errorf("someEnum: GOT: %v; WANT: %v", got, want)
	}
}

// TestRecursiveTerminatingDefaultExpansion verifies that a recursive record
// default that terminates through a null branch expands nested missing fields
// from their defaults rather than looping.
func TestRecursiveTerminatingDefaultExpansion(t *testing.T) {
	schema := `{
	  "type":"record","name":"Node","fields":[
	    {"name":"value","type":"int","default":7},
	    {"name":"next","type":["null","Node"],"default":null}
	  ]
	}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}

	// Provide an explicit nested node with its "value" omitted. Decoding
	// expands the nested record's missing field from its default (7) and the
	// nested "next" from null, terminating the recursion.
	textual := []byte(`{"next":{"Node":{}}}`)
	datum, _, err := codec.NativeFromTextual(textual)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	root := datum.(map[string]interface{})
	nestedUnion := root["next"].(map[string]interface{})
	nested := nestedUnion["Node"].(map[string]interface{})
	if got, want := nested["value"].(int32), int32(7); got != want {
		t.Errorf("nested value: GOT: %v; WANT: %v", got, want)
	}
	if nested["next"] != nil {
		t.Errorf("nested next: GOT: %v; WANT: nil", nested["next"])
	}

	// Round-trip back to text to ensure the expanded defaults are coherent.
	out, err := codec.TextualFromNative(nil, root)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if want := `"next":{"Node":{"value":7,"next":null}}`; !strings.Contains(string(out), want) {
		t.Errorf("GOT: %s; WANT to contain %s", string(out), want)
	}
}
