package goavro

import (
	"errors"
	"testing"
)

const moduleA = `{
  "type": "record",
  "name": "A",
  "namespace": "com.acme.x",
  "fields": [
    {"name": "b", "type": ["null", "B"]},
    {"name": "a", "type": ["null", "A"], "default": null}
  ]
}`

const moduleB = `{
  "type": "record",
  "name": "B",
  "namespace": "com.acme.x",
  "fields": [
    {"name": "a", "type": ["null", "A"]},
    {"name": "kind", "type": {"type": "enum", "name": "Kind", "symbols": ["ONE", "TWO"]}},
    {"name": "tag", "type": {"type": "fixed", "name": "Tag", "size": 4}}
  ]
}`

func moduleSet() []Module {
	return []Module{
		{Source: "a.avsc", Schema: moduleA},
		{Source: "b.avsc", Schema: moduleB},
	}
}

func shuffledModules() []Module {
	return []Module{
		{Source: "b.avsc", Schema: moduleB},
		{Source: "a.avsc", Schema: moduleA},
	}
}

func TestModularBuildCrossFileRecursion(t *testing.T) {
	codec, err := NewCodecFromModules(moduleSet(), "com.acme.x.A")
	if err != nil {
		t.Fatalf("build: %s", err)
	}
	if got, want := codec.TypeName().fullName, "com.acme.x.A"; got != want {
		t.Fatalf("root type: got %q want %q", got, want)
	}

	datum := map[string]interface{}{
		"b": map[string]interface{}{
			"com.acme.x.B": map[string]interface{}{
				"a": map[string]interface{}{
					"com.acme.x.A": map[string]interface{}{
						"b": map[string]interface{}{
							"com.acme.x.B": map[string]interface{}{
								"a": map[string]interface{}{
									"com.acme.x.A": map[string]interface{}{
										"b": nil,
										"a": nil,
									},
								},
								"kind": "ONE",
								"tag":  []byte("abcd"),
							},
						},
						"a": nil,
					},
				},
				"kind": "TWO",
				"tag":  []byte("wxyz"),
			},
		},
		"a": nil,
	}

	binary1, err := codec.BinaryFromNative(nil, datum)
	if err != nil {
		t.Fatalf("encode: %s", err)
	}

	decoded, remaining, err := codec.NativeFromBinary(binary1)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining bytes: %d", len(remaining))
	}
	if decoded == nil {
		t.Fatal("decoded datum is nil")
	}
}

func TestModularBuildOrderIndependence(t *testing.T) {
	c1, err := NewCodecFromModules(moduleSet(), "com.acme.x.A")
	if err != nil {
		t.Fatalf("build ordered: %s", err)
	}
	c2, err := NewCodecFromModules(shuffledModules(), "com.acme.x.A")
	if err != nil {
		t.Fatalf("build shuffled: %s", err)
	}
	if c1.CanonicalSchema() != c2.CanonicalSchema() {
		t.Fatalf("canonical schema differs by input order:\n%s\n%s", c1.CanonicalSchema(), c2.CanonicalSchema())
	}
	if c1.Rabin != c2.Rabin {
		t.Fatalf("rabin fingerprint differs by input order: %d vs %d", c1.Rabin, c2.Rabin)
	}

	datum := map[string]interface{}{
		"b": map[string]interface{}{
			"com.acme.x.B": map[string]interface{}{
				"a":    nil,
				"kind": "ONE",
				"tag":  []byte("0123"),
			},
		},
		"a": nil,
	}

	b1, err := c1.BinaryFromNative(nil, datum)
	if err != nil {
		t.Fatalf("encode c1: %s", err)
	}
	b2, err := c2.BinaryFromNative(nil, datum)
	if err != nil {
		t.Fatalf("encode c2: %s", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("binary encoding differs by input order: %v vs %v", b1, b2)
	}
}

func TestModularBuildSameShortNameDifferentNamespace(t *testing.T) {
	modules := []Module{
		{Source: "north.avsc", Schema: `{
			"type": "record", "name": "Widget", "namespace": "com.north",
			"fields": [{"name": "id", "type": "long"}]
		}`},
		{Source: "south.avsc", Schema: `{
			"type": "record", "name": "Widget", "namespace": "com.south",
			"fields": [{"name": "id", "type": "string"}]
		}`},
		{Source: "north-holder.avsc", Schema: `{
			"type": "record", "name": "Holder", "namespace": "com.north",
			"fields": [{"name": "w", "type": "Widget"}]
		}`},
	}

	// Short name resolves against the enclosing namespace, not whichever
	// module was read first.
	codec, err := NewCodecFromModules(modules, "com.north.Holder")
	if err != nil {
		t.Fatalf("build: %s", err)
	}
	if got, want := codec.CanonicalSchema(), `{"name":"com.north.Holder","type":"record","fields":[{"name":"w","type":"com.north.Widget"}]}`; got != want {
		t.Fatalf("canonical schema:\n got %s\nwant %s", got, want)
	}

	// A bare short root name that matches several namespaces is ambiguous.
	_, err = NewCodecFromModules(modules, "Widget")
	if err == nil {
		t.Fatal("expected ambiguous root error")
	}
	var ambErr *ErrAmbiguousRoot
	if !errors.As(err, &ambErr) {
		t.Fatalf("expected ErrAmbiguousRoot, got %T: %s", err, err)
	}
}

func TestModularBuildDuplicateNameRejectedWithTwoSources(t *testing.T) {
	body := func(ns string) string {
		return `{"type":"record","name":"Dup","namespace":"` + ns + `","fields":[]}`
	}
	modules := []Module{
		{Source: "one.avsc", Schema: body("com.x")},
		{Source: "two.avsc", Schema: body("com.x")},
	}

	result, err := BuildCodecFromModules(modules, "com.x.Dup", nil)
	if err == nil {
		t.Fatal("expected duplicate definition error")
	}
	var dup *ErrDuplicateName
	if !errors.As(err, &dup) {
		t.Fatalf("expected ErrDuplicateName, got %T: %s", err, err)
	}
	if dup.FullName != "com.x.Dup" {
		t.Fatalf("duplicate full name: %q", dup.FullName)
	}
	if len(dup.Sources) != 2 || dup.Sources[0] != "one.avsc" || dup.Sources[1] != "two.avsc" {
		t.Fatalf("expected both sources reported, got %v", dup.Sources)
	}

	// Even identical bodies follow the explicit policy.
	modules[1].Schema = modules[0].Schema
	_, err = BuildCodecFromModules(modules, "com.x.Dup", nil)
	if err == nil {
		t.Fatal("identical duplicate bodies ought to be rejected by default")
	}

	// AcceptFirst tolerates the duplicate and records an info diagnostic.
	result, err = BuildCodecFromModules(modules, "com.x.Dup", &ModuleBuildOptions{
		DuplicateNames: DuplicateNameAcceptFirst,
	})
	if err != nil {
		t.Fatalf("accept-first build: %s", err)
	}
	var infoCount int
	for _, d := range result.Diagnostics {
		if d.Severity == DiagnosticInfo {
			infoCount++
		}
	}
	if infoCount == 0 {
		t.Fatal("expected an info diagnostic for accepted duplicate")
	}
}

func TestModularBuildUnknownReferenceDiagnosticChain(t *testing.T) {
	modules := []Module{
		{Source: "a.avsc", Schema: `{
			"type": "record", "name": "A", "namespace": "com.acme.x",
			"fields": [{"name": "b", "type": "B"}]
		}`},
		{Source: "b.avsc", Schema: `{
			"type": "record", "name": "B", "namespace": "com.acme.x",
			"fields": [{"name": "ghost", "type": "Missing"}]
		}`},
	}

	// The dangling reference is in the reachable closure: build must fail.
	_, err := NewCodecFromModules(modules, "com.acme.x.A")
	if err == nil {
		t.Fatal("expected unknown type error")
	}
	var unk *ErrUnknownType
	if !errors.As(err, &unk) {
		t.Fatalf("expected ErrUnknownType, got %T: %s", err, err)
	}
	if unk.Reference != "com.acme.x.Missing" && unk.Reference != "Missing" {
		t.Fatalf("unexpected reference %q", unk.Reference)
	}
	if unk.ReferencedBy != "com.acme.x.B" {
		t.Fatalf("diagnostic chain ought to name B as referrer, got %q", unk.ReferencedBy)
	}

	result, _ := BuildCodecFromModules(modules, "com.acme.x.A", nil)
	found := false
	for _, d := range result.Diagnostics {
		if d.Source == "b.avsc" && d.Severity == DiagnosticError {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostic ought to attribute the dangling ref to b.avsc: %+v", result.Diagnostics)
	}
}

func TestModularBuildUnreferencedModulePolicy(t *testing.T) {
	modules := []Module{
		{Source: "good.avsc", Schema: `{
			"type": "record", "name": "Good", "namespace": "com.acme",
			"fields": [{"name": "id", "type": "int"}]
		}`},
		{Source: "broken.avsc", Schema: `{
			"type": "record", "name": "Broken", "namespace": "com.acme",
			"fields": [{"name": "ghost", "type": "DoesNotExist"}]
		}`},
	}

	// Default: the broken module is not reachable; build succeeds but keeps
	// a diagnostic.
	result, err := BuildCodecFromModules(modules, "com.acme.Good", nil)
	if err != nil {
		t.Fatalf("lenient build: %s", err)
	}
	var brokenDiag bool
	for _, d := range result.Diagnostics {
		if d.Source == "broken.avsc" {
			brokenDiag = true
		}
	}
	if !brokenDiag {
		t.Fatal("expected diagnostic for unreferenced broken module")
	}

	// Strict: any module error fails the build even when unreferenced.
	_, err = BuildCodecFromModules(modules, "com.acme.Good", &ModuleBuildOptions{
		ErrorOnUnreferencedModules: true,
	})
	if err == nil {
		t.Fatal("expected strict build to fail on unreferenced broken module")
	}
}

func TestModularBuildReachableUnknownAlwaysRejected(t *testing.T) {
	modules := []Module{
		{Source: "root.avsc", Schema: `{
			"type": "record", "name": "Root", "namespace": "com.acme",
			"fields": [{"name": "x", "type": "Vanish"}]
		}`},
		{Source: "other.avsc", Schema: `{
			"type": "record", "name": "Other", "namespace": "com.acme",
			"fields": []
		}`},
	}
	// Even with lenient unreferenced-module handling, the closure error is
	// fatal.
	_, err := BuildCodecFromModules(modules, "com.acme.Root", &ModuleBuildOptions{
		ErrorOnUnreferencedModules: false,
	})
	if err == nil {
		t.Fatal("unknown type in reachable closure must always reject the build")
	}
}

func TestModularBuildNoImplicitIO(t *testing.T) {
	// The API takes schema text directly; there is no path/URL parameter to
	// dereference. This test documents that contract by passing sources that
	// do not exist on disk and confirming they are labels only.
	codec, err := NewCodecFromModules([]Module{
		{Source: "memory://does-not-exist/record.avsc", Schema: `{
			"type": "record", "name": "Only", "fields": [{"name": "v", "type": "int"}]
		}`},
	}, "Only")
	if err != nil {
		t.Fatalf("build: %s", err)
	}
	buf, err := codec.BinaryFromNative(nil, map[string]interface{}{"v": 7})
	if err != nil {
		t.Fatalf("encode: %s", err)
	}
	if len(buf) != 1 || buf[0] != 0x0e {
		t.Fatalf("unexpected encoding: %v", buf)
	}
}

func TestModularBuildNestedDefReferencedFromAnotherModule(t *testing.T) {
	modules := []Module{
		// Leaf is nested inside a container in one module.
		{Source: "container.avsc", Schema: `{
			"type": "record", "name": "Container", "namespace": "com.shared",
			"fields": [
				{"name": "leaf", "type": {
					"type": "record", "name": "Leaf",
					"fields": [{"name": "v", "type": "int"}]
				}}
			]
		}`},
		// A different module references the nested type by its short name;
		// the short name must resolve against the shared namespace.
		{Source: "user.avsc", Schema: `{
			"type": "record", "name": "User", "namespace": "com.shared",
			"fields": [{"name": "leaf", "type": "Leaf"}]
		}`},
	}
	codec, err := NewCodecFromModules(modules, "com.shared.User")
	if err != nil {
		t.Fatalf("build: %s", err)
	}
	want := `{"name":"com.shared.User","type":"record","fields":[{"name":"leaf","type":"com.shared.Leaf"}]}`
	if got := codec.CanonicalSchema(); got != want {
		t.Fatalf("canonical:\n got %s\nwant %s", got, want)
	}

	buf, err := codec.BinaryFromNative(nil, map[string]interface{}{
		"leaf": map[string]interface{}{"v": int32(3)},
	})
	if err != nil {
		t.Fatalf("encode: %s", err)
	}
	decoded, remaining, err := codec.NativeFromBinary(buf)
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining: %d", len(remaining))
	}
	m, ok := decoded.(map[string]interface{})
	if !ok {
		t.Fatalf("decoded type: %T", decoded)
	}
	leaf := m["leaf"].(map[string]interface{})
	if leaf["v"].(int32) != 3 {
		t.Fatalf("leaf.v: %v", leaf["v"])
	}
}

func TestModularBuildFixedAndEnumSharedAcrossModules(t *testing.T) {
	modules := []Module{
		{Source: "types.avsc", Schema: `{
			"type": "fixed", "name": "MD5", "namespace": "com.sec", "size": 16
		}`},
		{Source: "colors.avsc", Schema: `{
			"type": "enum", "name": "Color", "namespace": "com.sec",
			"symbols": ["RED", "GREEN", "BLUE"]
		}`},
		{Source: "doc.avsc", Schema: `{
			"type": "record", "name": "Doc", "namespace": "com.sec",
			"fields": [
				{"name": "hash", "type": "MD5"},
				{"name": "color", "type": "Color"}
			]
		}`},
	}
	// Reverse the inputs so fixed/enum come after the record that uses them.
	reversed := []Module{modules[2], modules[1], modules[0]}
	c1, err := NewCodecFromModules(modules, "com.sec.Doc")
	if err != nil {
		t.Fatalf("build: %s", err)
	}
	c2, err := NewCodecFromModules(reversed, "com.sec.Doc")
	if err != nil {
		t.Fatalf("build reversed: %s", err)
	}
	if c1.CanonicalSchema() != c2.CanonicalSchema() {
		t.Fatalf("canonical mismatch:\n%s\n%s", c1.CanonicalSchema(), c2.CanonicalSchema())
	}
	datum := map[string]interface{}{
		"hash":  []byte("0123456789abcdef"),
		"color": "GREEN",
	}
	b1, _ := c1.BinaryFromNative(nil, datum)
	b2, _ := c2.BinaryFromNative(nil, datum)
	if string(b1) != string(b2) {
		t.Fatalf("encoding differs: %v vs %v", b1, b2)
	}
}

func TestModularBuildNullNamespaceShortName(t *testing.T) {
	modules := []Module{
		{Source: "n1.avsc", Schema: `{
			"type": "record", "name": "Alpha",
			"fields": [{"name": "z", "type": "Zeta"}]
		}`},
		{Source: "n2.avsc", Schema: `{
			"type": "record", "name": "Zeta",
			"fields": [{"name": "back", "type": "Alpha"}]
		}`},
	}
	nullable := []Module{
		{Source: "n1.avsc", Schema: modules[0].Schema},
		{Source: "n2.avsc", Schema: `{
			"type": "record", "name": "Zeta",
			"fields": [{"name": "back", "type": ["null", "Alpha"]}]
		}`},
	}
	codec, err := NewCodecFromModules(nullable, "Alpha")
	if err != nil {
		t.Fatalf("build nullable: %s", err)
	}
	datum := map[string]interface{}{
		"z": map[string]interface{}{
			"back": map[string]interface{}{
				"Alpha": map[string]interface{}{
					"z": map[string]interface{}{
						"back": nil,
					},
				},
			},
		},
	}
	if _, err := codec.BinaryFromNative(nil, datum); err != nil {
		t.Fatalf("encode: %s", err)
	}
}

func TestModularBuildUnknownRoot(t *testing.T) {
	modules := []Module{
		{Source: "a.avsc", Schema: `{"type":"record","name":"A","fields":[]}`},
	}
	if _, err := NewCodecFromModules(modules, "Nope"); err == nil {
		t.Fatal("expected unknown root error")
	}
	if _, err := NewCodecFromModules(nil, "A"); err == nil {
		t.Fatal("expected no-modules error")
	}
}
