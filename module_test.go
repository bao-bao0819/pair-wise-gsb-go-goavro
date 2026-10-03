package goavro

import (
	"reflect"
	"strings"
	"testing"
)

// mod is a compact test helper for a labelled schema document.
func mod(source, schema string) SchemaModule {
	return SchemaModule{Source: source, Schema: schema}
}

// Shuffling module order must not change the root canonical schema nor encoded
// bytes, and cross-file mutually recursive records must link.
func TestModularCodecOrderIndependent(t *testing.T) {
	// A -> B -> A: A holds a B, B holds an A, declared in separate documents.
	moduleA := `{
		"type":"record","name":"com.example.A","namespace":"com.example",
		"fields":[{"name":"b","type":["null","com.example.B"],"default":null}]
	}`
	moduleB := `{
		"type":"record","name":"com.example.B","namespace":"com.example",
		"fields":[{"name":"a","type":["null","com.example.A"],"default":null}]
	}`

	orders := [][]SchemaModule{
		{mod("a.avsc", moduleA), mod("b.avsc", moduleB)},
		{mod("b.avsc", moduleB), mod("a.avsc", moduleA)},
	}

	datum := map[string]interface{}{
		"b": map[string]interface{}{
			"com.example.B": map[string]interface{}{
				"a": map[string]interface{}{
					"com.example.A": map[string]interface{}{
						"b": nil,
					},
				},
			},
		},
	}

	var canonical string
	var binary []byte

	for i, modules := range orders {
		result, err := BuildModularCodec(modules, &ModularOptions{RootName: "com.example.A"})
		if err != nil {
			t.Fatalf("order %d: build failed: %s", i, err)
		}
		if result.RootName != "com.example.A" {
			t.Fatalf("order %d: unexpected root name %q", i, result.RootName)
		}
		if i == 0 {
			canonical = result.Codec.CanonicalSchema()
			wantCanonical := `{"name":"com.example.A","type":"record","fields":[{"name":"b","type":["null",{"name":"com.example.B","type":"record","fields":[{"name":"a","type":["null","com.example.A"]}]}]}]}`
			if canonical != wantCanonical {
				t.Fatalf("canonical schema mismatch:\nwant %s\ngot  %s", wantCanonical, canonical)
			}
		} else if result.Codec.CanonicalSchema() != canonical {
			t.Fatalf("order %d: canonical schema differs:\nwant %s\ngot  %s",
				i, canonical, result.Codec.CanonicalSchema())
		}

		encoded, err := result.Codec.BinaryFromNative(nil, datum)
		if err != nil {
			t.Fatalf("order %d: encode failed: %s", i, err)
		}
		if i == 0 {
			binary = encoded
		} else if !reflect.DeepEqual(encoded, binary) {
			t.Fatalf("order %d: binary encoding differs: %v vs %v", i, binary, encoded)
		}

		decoded, remaining, err := result.Codec.NativeFromBinary(encoded)
		if err != nil {
			t.Fatalf("order %d: decode failed: %s", i, err)
		}
		if len(remaining) != 0 {
			t.Fatalf("order %d: trailing bytes: %v", i, remaining)
		}
		if !reflect.DeepEqual(decoded, datum) {
			t.Fatalf("order %d: round trip mismatch:\nwant %#v\ngot  %#v", i, datum, decoded)
		}
	}
}

// The same short name in different namespaces must bind to the type in the
// referencing record's namespace, never to whichever file was read first.
func TestModularCodecShortNameResolvedByNamespace(t *testing.T) {
	moduleA := `{
		"type":"record","name":"alpha.Container","namespace":"alpha",
		"fields":[{"name":"kind","type":"Kind"}]
	}`
	moduleB := `{
		"type":"record","name":"beta.Container","namespace":"beta",
		"fields":[{"name":"kind","type":"Kind"}]
	}`
	kindAlpha := `{"type":"enum","name":"alpha.Kind","namespace":"alpha","symbols":["A1","A2"]}`
	kindBeta := `{"type":"enum","name":"beta.Kind","namespace":"beta","symbols":["B1","B2"]}`

	// Present the beta documents first to ensure they would win a naive
	// "first read" resolution strategy.
	modules := []SchemaModule{
		mod("kindBeta", kindBeta),
		mod("b", moduleB),
		mod("kindAlpha", kindAlpha),
		mod("a", moduleA),
	}

	result, err := BuildModularCodec(modules, &ModularOptions{RootName: "alpha.Container"})
	if err != nil {
		t.Fatalf("build failed: %s", err)
	}

	canonical := result.Codec.CanonicalSchema()
	if !strings.Contains(canonical, `"name":"alpha.Kind","type":"enum"`) {
		t.Fatalf("alpha.Container field must expand alpha.Kind; got %s", canonical)
	}

	encoded, err := result.Codec.BinaryFromNative(nil, map[string]interface{}{"kind": "A1"})
	if err != nil {
		t.Fatalf("encode failed: %s", err)
	}
	if got := len(encoded); got != 1 || encoded[0] != 0 {
		t.Fatalf("alpha.Kind A1 ought to encode as one zero byte; got %v", encoded)
	}

	// The other root must independently bind to the beta enum.
	resultB, err := BuildModularCodec(modules, &ModularOptions{RootName: "beta.Container"})
	if err != nil {
		t.Fatalf("build beta failed: %s", err)
	}
	if !strings.Contains(resultB.Codec.CanonicalSchema(), `"name":"beta.Kind","type":"enum"`) {
		t.Fatalf("beta.Container must reference beta.Kind; got %s", resultB.Codec.CanonicalSchema())
	}
	if _, err := resultB.Codec.BinaryFromNative(nil, map[string]interface{}{"kind": "A1"}); err == nil {
		t.Fatalf("A1 is not a member of beta.Kind; encoding ought to fail")
	}
}

// Unknown types in the reachable closure must be rejected with a chain that
// names the types and sources walked from the root.
func TestModularCodecUnknownReferenceChain(t *testing.T) {
	moduleA := `{
		"type":"record","name":"com.example.A",
		"fields":[{"name":"b","type":"com.example.B"}]
	}`
	moduleB := `{
		"type":"record","name":"com.example.B",
		"fields":[{"name":"c","type":"com.example.Missing"}]
	}`

	_, err := BuildModularCodec(
		[]SchemaModule{mod("fileB", moduleB), mod("fileA", moduleA)},
		&ModularOptions{RootName: "com.example.A"})
	if err == nil {
		t.Fatal("expected unknown type error")
	}

	aggregate, ok := err.(*ModularBuildError)
	if !ok || len(aggregate.Errors) != 1 {
		t.Fatalf("expected single ModularBuildError; got %T: %v", err, err)
	}
	unknown, ok := aggregate.Errors[0].(*UnknownTypeError)
	if !ok {
		t.Fatalf("expected *UnknownTypeError; got %T: %v", aggregate.Errors[0], aggregate.Errors[0])
	}
	if unknown.Reference != "com.example.Missing" {
		t.Fatalf("unexpected reference: %q", unknown.Reference)
	}
	if unknown.ReferencedFromType != "com.example.B" || unknown.ReferencedFromSource != "fileB" {
		t.Fatalf("unexpected reference origin: %q in %q",
			unknown.ReferencedFromType, unknown.ReferencedFromSource)
	}

	wantChain := []string{"com.example.A", "com.example.B"}
	if !reflect.DeepEqual(unknown.TypeChain, wantChain) {
		t.Fatalf("resolution chain ought to walk A -> B; got %v", unknown.TypeChain)
	}
	wantSources := []string{"fileA", "fileB"}
	if !reflect.DeepEqual(unknown.SourceChain, wantSources) {
		t.Fatalf("source chain ought to walk fileA -> fileB; got %v", unknown.SourceChain)
	}
	if !strings.Contains(err.Error(), "com.example.A (fileA) -> com.example.B (fileB)") {
		t.Fatalf("error ought to render the diagnostic chain; got: %s", err)
	}
}

// Duplicate full names are rejected by default even with identical bodies, and
// the error names both sources. The winner policy tolerates them and reports a
// diagnostic.
func TestModularCodecDuplicateFullName(t *testing.T) {
	body := `{"type":"enum","name":"com.example.Kind","symbols":["X"]}`
	modules := []SchemaModule{
		mod("one.avsc", body),
		mod("two.avsc", body),
	}

	_, err := BuildModularCodec(modules, &ModularOptions{RootName: "com.example.Kind"})
	if err == nil {
		t.Fatal("identical duplicate bodies ought to be rejected by default")
	}
	duplicate := findDuplicateError(t, err)
	if duplicate.FullName != "com.example.Kind" {
		t.Fatalf("unexpected duplicate name: %q", duplicate.FullName)
	}
	if !reflect.DeepEqual(duplicate.Sources, []string{"one.avsc", "two.avsc"}) {
		t.Fatalf("both sources ought to be reported; got %v", duplicate.Sources)
	}

	result, err := BuildModularCodec(modules, &ModularOptions{
		RootName:        "com.example.Kind",
		DuplicatePolicy: DuplicateAllowWinner,
	})
	if err != nil {
		t.Fatalf("winner policy ought to tolerate the duplicate: %s", err)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("expected one duplicate diagnostic; got %v", result.Diagnostics)
	}
	if !reflect.DeepEqual(result.Diagnostics[0].Sources, []string{"one.avsc", "two.avsc"}) {
		t.Fatalf("diagnostic ought to list both sources; got %v", result.Diagnostics[0].Sources)
	}
}

func findDuplicateError(t *testing.T, err error) *DuplicateNameError {
	t.Helper()
	aggregate, ok := err.(*ModularBuildError)
	if !ok {
		t.Fatalf("expected ModularBuildError; got %T: %v", err, err)
	}
	for _, candidate := range aggregate.Errors {
		if duplicate, ok := candidate.(*DuplicateNameError); ok {
			return duplicate
		}
	}
	t.Fatalf("no DuplicateNameError in: %v", err)
	return nil
}

// A broken module not referenced from the root fails the build by default, but
// is downgraded to a diagnostic under LenientUnreferencedModules. Unknown
// types reachable from the root always fail.
func TestModularCodecUnreferencedModulePolicy(t *testing.T) {
	root := `{"type":"record","name":"Root","fields":[{"name":"n","type":"int"}]}`
	broken := `{"type":"record","name":"Orphan","fields":[{"name":"x","type":"NoSuchType"}]}`

	modules := []SchemaModule{
		mod("root", root),
		mod("broken", broken),
	}

	if _, err := BuildModularCodec(modules, &ModularOptions{RootName: "Root"}); err == nil {
		t.Fatal("unreferenced broken module ought to fail the build in strict mode")
	}

	result, err := BuildModularCodec(modules, &ModularOptions{
		RootName:                   "Root",
		LenientUnreferencedModules: true,
	})
	if err != nil {
		t.Fatalf("lenient build ought to ignore the broken module: %s", err)
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("expected one diagnostic for the broken module; got %v", result.Diagnostics)
	}
	if result.Diagnostics[0].FullName != "Orphan" ||
		!reflect.DeepEqual(result.Diagnostics[0].Sources, []string{"broken"}) {
		t.Fatalf("unexpected diagnostic: %#v", result.Diagnostics[0])
	}

	// Reachability overrides leniency.
	rootReaching := `{"type":"record","name":"Root2","fields":[{"name":"o","type":"Orphan"}]}`
	_, err = BuildModularCodec(
		[]SchemaModule{mod("root", rootReaching), mod("broken", broken)},
		&ModularOptions{RootName: "Root2", LenientUnreferencedModules: true})
	if err == nil {
		t.Fatal("unknown type inside the reachable closure ought to reject even in lenient mode")
	}
}

// A shared enum declared in one document and referenced by short name from a
// record in another document links, and the root is inferred from the single
// top level record when RootName is omitted.
func TestModularCodecInferredRootAndSharedType(t *testing.T) {
	recordModule := `{
		"type":"record","name":"widgets.Widget","namespace":"widgets",
		"fields":[{"name":"color","type":"Color"}]
	}`
	colorModule := `{
		"type":"enum","name":"widgets.Color","namespace":"widgets",
		"symbols":["RED","GREEN","BLUE"]
	}`

	result, err := BuildModularCodec(
		[]SchemaModule{mod("colors", colorModule), mod("widgets", recordModule)},
		nil)
	if err != nil {
		t.Fatalf("build failed: %s", err)
	}
	if result.RootName != "widgets.Widget" {
		t.Fatalf("root ought to be inferred; got %q", result.RootName)
	}
	if !strings.Contains(result.Codec.CanonicalSchema(), `"name":"widgets.Color","type":"enum"`) {
		t.Fatalf("canonical schema ought to qualify the shared enum; got %s",
			result.Codec.CanonicalSchema())
	}

	encoded, err := result.Codec.BinaryFromNative(nil, map[string]interface{}{"color": "BLUE"})
	if err != nil {
		t.Fatalf("encode failed: %s", err)
	}
	decoded, _, err := result.Codec.NativeFromBinary(encoded)
	if err != nil {
		t.Fatalf("decode failed: %s", err)
	}
	if got := decoded.(map[string]interface{})["color"]; got != "BLUE" {
		t.Fatalf("round trip mismatch: %v", got)
	}
}
