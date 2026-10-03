package goavro

// This file provides a modular codec construction entry point.
//
// A large Avro schema may be split into several in-memory documents that share
// named record, enum, and fixed types. Unlike NewCodec, which ingests a single
// JSON document, the modular builder ingests a set of schema documents that
// each carry a caller supplied source identifier, links every name reference
// across the whole set, and produces the Codec for one root type.
//
// The builder performs no file system or network access: callers pass already
// loaded schema text and their own source labels, and the labels are only
// echoed back in errors and diagnostics.
//
// Linking happens in two phases so that the result never depends on the order
// in which modules are presented:
//
//  1. Every named type (record, enum, fixed) in every module is registered
//     under its fully qualified name (see collect).
//  2. Placeholder codecs for all registered types are created up front, then
//     only the reachable closure is filled in. Short names resolve against the
//     namespace dictated by the Avro specification, never against whichever
//     document happened to be read first.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SchemaModule is one in-memory Avro schema document identified by Source.
type SchemaModule struct {
	// Source identifies where this document came from. goavro never reads or
	// interprets it; it is only echoed back in errors and diagnostics so that
	// callers can point users at the offending document.
	Source string
	// Schema is Avro schema JSON: either a single named type or a JSON array
	// of schemas.
	Schema string
}

// DuplicateNamePolicy states how a full name declared by more than one module
// is handled.
type DuplicateNamePolicy int

const (
	// DuplicateReject fails the build when a full name has more than one
	// definition, even when the definition bodies are byte-for-byte identical.
	// Every source that defines the name is reported.
	DuplicateReject DuplicateNamePolicy = iota
	// DuplicateAllowWinner keeps a single deterministic winner that is
	// independent of module input order: the definition whose Source sorts
	// lexicographically smallest (ties broken by definition body) wins. All
	// sources are still surfaced as diagnostics.
	DuplicateAllowWinner
)

// ModularOptions configures a modular codec build.
type ModularOptions struct {
	// RootName selects the root type by full name, or by an unambiguous short
	// name. When empty the root is inferred: exactly one module must carry a
	// single named schema (not wrapped in an array) at its top level.
	RootName string
	// CodecOption tunes native/text conversion; DefaultCodecOption is used
	// when nil.
	CodecOption *CodecOption
	// DuplicatePolicy selects duplicate full-name handling; the zero value is
	// DuplicateReject.
	DuplicatePolicy DuplicateNamePolicy
	// LenientUnreferencedModules controls whether problems confined to modules
	// that are not reachable from the root abort the build. When false (the
	// default) a broken module fails the whole build; when true such problems
	// are surfaced as diagnostics instead. Unknown types inside the reachable
	// closure always reject the build, regardless of this setting.
	LenientUnreferencedModules bool
}

// ModularDiagnostic reports a non-fatal condition encountered while building.
type ModularDiagnostic struct {
	// FullName is the name involved, or "" for a module-wide condition.
	FullName string
	// Sources lists the source identifiers involved: one for a broken module,
	// two or more for a duplicated definition.
	Sources []string
	// Message describes the condition.
	Message string
}

// DuplicateNameError describes a full name declared in more than one place.
type DuplicateNameError struct {
	// FullName is the multiply defined name.
	FullName string
	// Sources lists every source that defines FullName, in deterministic
	// (lexicographic) order.
	Sources []string
}

func (e *DuplicateNameError) Error() string {
	return fmt.Sprintf("duplicate definition of %q in sources: %s",
		e.FullName, strings.Join(quoteAll(e.Sources), ", "))
}

// UnknownTypeError describes a name reference that cannot be linked.
type UnknownTypeError struct {
	// Reference is the name token that could not be resolved.
	Reference string
	// ReferencedFromType is the full name of the type carrying the bad
	// reference, or "" when the reference sits at a module top level.
	ReferencedFromType string
	// ReferencedFromSource is the source of the document carrying the
	// reference.
	ReferencedFromSource string
	// TypeChain is the resolution path walked from the root to the type that
	// carries the broken reference, full name by full name.
	TypeChain []string
	// SourceChain parallels TypeChain with the source of each type.
	SourceChain []string
}

func (e *UnknownTypeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "unknown type name %q", e.Reference)
	if e.ReferencedFromType != "" {
		fmt.Fprintf(&b, " referenced from type %q", e.ReferencedFromType)
	}
	if e.ReferencedFromSource != "" {
		fmt.Fprintf(&b, " in source %q", e.ReferencedFromSource)
	}
	if len(e.TypeChain) > 0 {
		steps := make([]string, len(e.TypeChain))
		for i, typeName := range e.TypeChain {
			if i < len(e.SourceChain) && e.SourceChain[i] != "" {
				steps[i] = typeName + " (" + e.SourceChain[i] + ")"
			} else {
				steps[i] = typeName
			}
		}
		fmt.Fprintf(&b, "; resolution chain: %s", strings.Join(steps, " -> "))
	}
	return b.String()
}

// ModularBuildError aggregates one or more fatal build failures.
type ModularBuildError struct {
	Errors []error
}

func (e *ModularBuildError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		parts[i] = err.Error()
	}
	return "modular schema build failed: " + strings.Join(parts, "; ")
}

// ModularBuildResult is returned by BuildModularCodec.
type ModularBuildResult struct {
	// Codec is the linked Codec for the requested root type.
	Codec *Codec
	// RootName is the resolved full name of the root type.
	RootName string
	// Diagnostics holds non-fatal reports (tolerated duplicates and, in
	// lenient mode, ignored problems in unreachable modules).
	Diagnostics []ModularDiagnostic
}

// BuildModularCodec links the supplied schema modules and returns the Codec for
// the root type. It performs no I/O: modules are in-memory documents and
// Source is an opaque caller supplied label.
func BuildModularCodec(modules []SchemaModule, options *ModularOptions) (*ModularBuildResult, error) {
	opts := &ModularOptions{}
	if options != nil {
		*opts = *options
	}
	if opts.CodecOption == nil {
		opts.CodecOption = DefaultCodecOption()
	}

	mb := &moduleBuild{
		options:    opts,
		candidates: make(map[string][]*namedDef),
		defs:       make(map[string]*namedDef),
		refs:       make(map[string][]refEdge),
		rawSchema:  make(map[string]string),
	}

	duplicateDiags, err := mb.collect(modules)
	if err != nil {
		return nil, err
	}

	root, err := mb.selectRoot()
	if err != nil {
		return nil, err
	}

	mb.collectAllReferences()

	fatal, diagnostics := mb.validate(root)
	diagnostics = append(duplicateDiags, diagnostics...)
	if len(fatal) > 0 {
		return nil, &ModularBuildError{Errors: fatal}
	}

	if err := mb.linkAndBuild(root); err != nil {
		return nil, err
	}

	return &ModularBuildResult{
		Codec:       mb.st[root.fullName],
		RootName:    root.fullName,
		Diagnostics: diagnostics,
	}, nil
}

// NewModularCodec is a convenience wrapper around BuildModularCodec for
// callers that only need the root Codec.
func NewModularCodec(modules []SchemaModule, options *ModularOptions) (*Codec, error) {
	result, err := BuildModularCodec(modules, options)
	if err != nil {
		return nil, err
	}
	return result.Codec, nil
}

// namedDef is one phase-1 registration of a named type.
type namedDef struct {
	fullName  string
	namespace string
	// enclosingNS is the namespace in scope where this definition appeared.
	// It is needed to rebuild inline named types that rely on an inherited
	// namespace rather than declaring one.
	enclosingNS string
	kind        string // "record", "enum", or "fixed"
	schema      map[string]interface{}
	source      string
}

// refEdge is a name reference observed from a named type.
type refEdge struct {
	token     string
	namespace string
	fromType  string
	source    string
}

type sourceError struct {
	source string
	err    error
}

type moduleBuild struct {
	options *ModularOptions

	// every definition keyed by full name, including duplicate candidates
	candidates map[string][]*namedDef
	// winning definitions after duplicate policy application
	defs map[string]*namedDef

	// type references keyed by the full name of the enclosing named type
	refs map[string][]refEdge

	rawSchema map[string]string // source -> original JSON text

	parseErrors []sourceError // malformed JSON, keyed by source
	scanErrors  []sourceError // invalid named type, keyed by source

	rootCandidates []*namedDef // single named type at a non-array top level

	st      map[string]*Codec
	filling map[string]bool
	cb      *codecBuilder

	// rendered tracks named types already expanded while rendering the
	// canonical form; repeat occurrences use the full name string.
	rendered map[string]bool
}

var primitiveTypeNames = map[string]bool{
	"null": true, "boolean": true, "int": true, "long": true, "float": true,
	"double": true, "bytes": true, "string": true,
}

var namedTypeKinds = map[string]bool{
	"record": true, "enum": true, "fixed": true,
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strconv.Quote(v)
	}
	return out
}

// collect is phase 1: parse every module, register every named type under its
// full name, and apply the duplicate full-name policy. It returns diagnostics
// for tolerated duplicates.
func (mb *moduleBuild) collect(modules []SchemaModule) ([]ModularDiagnostic, error) {
	for _, mod := range modules {
		mb.rawSchema[mod.Source] = mod.Schema

		var doc interface{}
		if err := json.Unmarshal([]byte(mod.Schema), &doc); err != nil {
			mb.parseErrors = append(mb.parseErrors, sourceError{
				source: mod.Source,
				err:    fmt.Errorf("source %q: cannot unmarshal schema JSON: %s", mod.Source, err),
			})
			continue
		}

		var topLevelDefs []*namedDef
		switch node := doc.(type) {
		case []interface{}:
			for _, el := range node {
				mb.scanNode(el, "", mod.Source, nil)
			}
		default:
			topLevelDefs = mb.scanNode(doc, "", mod.Source, nil)
		}
		// Root inference only considers top level records: standalone enum and
		// fixed documents are treated as shared leaf types. Callers needing an
		// enum/fixed root set ModularOptions.RootName.
		if len(topLevelDefs) == 1 && topLevelDefs[0].kind == "record" {
			mb.rootCandidates = append(mb.rootCandidates, topLevelDefs[0])
		}
	}

	return mb.resolveDuplicates()
}

// scanNode walks one parsed schema subtree and registers every named type it
// finds under mb.candidates. It returns the named types defined directly at
// the walked node (i.e. at the call site's own level), which is used to infer
// the root from a single, non-array top level document.
func (mb *moduleBuild) scanNode(node interface{}, enclosingNamespace, source string, defsHere []*namedDef) []*namedDef {
	switch value := node.(type) {
	case map[string]interface{}:
		typeName, _ := value["type"].(string)

		if namedTypeKinds[typeName] {
			def, err := mb.registerNamed(value, typeName, enclosingNamespace, source)
			if err != nil {
				mb.scanErrors = append(mb.scanErrors, sourceError{source: source, err: err})
				return defsHere
			}
			defsHere = append(defsHere, def)
			// Children are scanned under the new namespace, but they are not
			// "defined here" relative to the caller.
			switch typeName {
			case "record":
				if fields, ok := value["fields"].([]interface{}); ok {
					for _, field := range fields {
						if fieldMap, ok := field.(map[string]interface{}); ok {
							if fieldType, ok := fieldMap["type"]; ok {
								mb.scanNode(fieldType, def.namespace, source, nil)
							}
						}
					}
				}
			}
			return defsHere
		}

		if items, ok := value["items"]; ok {
			mb.scanNode(items, enclosingNamespace, source, nil)
		}
		if values, ok := value["values"]; ok {
			mb.scanNode(values, enclosingNamespace, source, nil)
		}
		if nested, ok := value["type"]; ok && nested != nil {
			mb.scanNode(nested, enclosingNamespace, source, nil)
		}

	case []interface{}:
		for _, el := range value {
			mb.scanNode(el, enclosingNamespace, source, nil)
		}
	}
	return defsHere
}

// registerNamed computes the full name of one named schema and stores a
// candidate. Duplicate full names are retained so the configured policy can
// decide later.
func (mb *moduleBuild) registerNamed(schemaMap map[string]interface{}, kind, enclosingNamespace, source string) (*namedDef, error) {
	n, err := newNameFromSchemaMap(enclosingNamespace, schemaMap)
	if err != nil {
		return nil, fmt.Errorf("source %q: %s ought to have valid name: %s", source, kind, err)
	}
	def := &namedDef{
		fullName:    n.fullName,
		namespace:   n.namespace,
		enclosingNS: enclosingNamespace,
		kind:        kind,
		schema:      schemaMap,
		source:      source,
	}
	mb.candidates[n.fullName] = append(mb.candidates[n.fullName], def)
	return def, nil
}

// resolveDuplicates applies the configured policy to every full name.
func (mb *moduleBuild) resolveDuplicates() ([]ModularDiagnostic, error) {
	names := make([]string, 0, len(mb.candidates))
	for fullName := range mb.candidates {
		names = append(names, fullName)
	}
	sort.Strings(names)

	var diagnostics []ModularDiagnostic
	var fatal []error

	for _, fullName := range names {
		candidates := mb.candidates[fullName]
		if len(candidates) == 1 {
			mb.defs[fullName] = candidates[0]
			continue
		}

		sorted := make([]*namedDef, len(candidates))
		copy(sorted, candidates)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].source != sorted[j].source {
				return sorted[i].source < sorted[j].source
			}
			return canonicalBody(sorted[i].schema) < canonicalBody(sorted[j].schema)
		})

		sourcesSet := map[string]bool{}
		var sources []string
		for _, c := range sorted {
			if !sourcesSet[c.source] {
				sourcesSet[c.source] = true
				sources = append(sources, c.source)
			}
		}

		switch mb.options.DuplicatePolicy {
		case DuplicateAllowWinner:
			mb.defs[fullName] = sorted[0]
			diagnostics = append(diagnostics, ModularDiagnostic{
				FullName: fullName,
				Sources:  sources,
				Message: fmt.Sprintf(
					"duplicate definition of %q tolerated; source %q wins by DuplicateAllowWinner",
					fullName, sorted[0].source),
			})
		default:
			fatal = append(fatal, &DuplicateNameError{FullName: fullName, Sources: sources})
		}
	}

	if len(fatal) > 0 {
		return diagnostics, &ModularBuildError{Errors: fatal}
	}

	// Drop inferred root candidates that lost a duplicate decision.
	inferred := mb.rootCandidates
	mb.rootCandidates = mb.rootCandidates[:0]
	for _, c := range inferred {
		if winner, ok := mb.defs[c.fullName]; ok && winner == c {
			mb.rootCandidates = append(mb.rootCandidates, c)
		}
	}
	return diagnostics, nil
}

// canonicalBody renders a definition body deterministically for tie-breaking.
func canonicalBody(schemaMap map[string]interface{}) string {
	bytes, err := json.Marshal(schemaMap)
	if err != nil {
		return ""
	}
	return string(bytes)
}

// selectRoot resolves the root type from RootName or the single top level
// candidate.
func (mb *moduleBuild) selectRoot() (*namedDef, error) {
	if mb.options.RootName != "" {
		if def, ok := mb.defs[mb.options.RootName]; ok {
			return def, nil
		}
		short := shortNameOf(mb.options.RootName)
		var matches []*namedDef
		for _, def := range mb.defs {
			if def.short() == short {
				matches = append(matches, def)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			return nil, fmt.Errorf("modular schema build failed: root type %q is not defined by any module", mb.options.RootName)
		default:
			sort.Slice(matches, func(i, j int) bool { return matches[i].fullName < matches[j].fullName })
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.fullName
			}
			return nil, fmt.Errorf("modular schema build failed: root short name %q is ambiguous; candidates: %s",
				mb.options.RootName, strings.Join(names, ", "))
		}
	}

	switch len(mb.rootCandidates) {
	case 1:
		return mb.rootCandidates[0], nil
	case 0:
		return nil, fmt.Errorf("modular schema build failed: cannot infer root type: no module carries a single named schema at its top level; set ModularOptions.RootName")
	default:
		sort.Slice(mb.rootCandidates, func(i, j int) bool {
			return mb.rootCandidates[i].fullName < mb.rootCandidates[j].fullName
		})
		names := make([]string, len(mb.rootCandidates))
		for i, c := range mb.rootCandidates {
			names[i] = c.fullName
		}
		return nil, fmt.Errorf("modular schema build failed: cannot infer unique root type; candidates: %s; set ModularOptions.RootName",
			strings.Join(names, ", "))
	}
}

func (d *namedDef) short() string {
	return shortNameOf(d.fullName)
}

func shortNameOf(fullName string) string {
	if index := strings.LastIndexByte(fullName, '.'); index > -1 {
		return fullName[index+1:]
	}
	return fullName
}

// collectAllReferences gathers name tokens referenced by every winning
// definition. Nested inline named types are skipped here: they become winners
// and own their own references.
func (mb *moduleBuild) collectAllReferences() {
	for _, def := range mb.sortedDefs() {
		edges := mb.refs[def.fullName]
		if def.kind == "record" {
			fields, _ := def.schema["fields"].([]interface{})
			for _, field := range fields {
				fieldMap, ok := field.(map[string]interface{})
				if !ok {
					continue
				}
				if fieldType, ok := fieldMap["type"]; ok {
					edges = mb.collectType(fieldType, def, edges)
				}
			}
		}
		edges = mb.dedupEdges(edges)
		mb.refs[def.fullName] = edges
	}
}

// dedupEdges sorts and de-duplicates reference edges so closure order and
// diagnostics are independent of map iteration and document appearance.
func (mb *moduleBuild) dedupEdges(edges []refEdge) []refEdge {
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].token != edges[j].token {
			return edges[i].token < edges[j].token
		}
		return edges[i].namespace < edges[j].namespace
	})
	out := edges[:0]
	for _, edge := range edges {
		if len(out) > 0 && out[len(out)-1].token == edge.token && out[len(out)-1].namespace == edge.namespace {
			continue
		}
		out = append(out, edge)
	}
	return out
}

// collectType appends reference edges found in one type node.
func (mb *moduleBuild) collectType(node interface{}, owner *namedDef, edges []refEdge) []refEdge {
	switch value := node.(type) {
	case string:
		return mb.addRef(value, owner, edges)
	case []interface{}:
		for _, el := range value {
			edges = mb.collectType(el, owner, edges)
		}
	case map[string]interface{}:
		if typeName, ok := value["type"].(string); ok {
			if namedTypeKinds[typeName] {
				if _, hasName := value["name"]; hasName {
					// Inline named type: it is a separate definition and owns
					// its own references.
					return edges
				}
			}
		}
		if items, ok := value["items"]; ok {
			edges = mb.collectType(items, owner, edges)
		}
		if values, ok := value["values"]; ok {
			edges = mb.collectType(values, owner, edges)
		}
		if nested, ok := value["type"]; ok {
			switch typed := nested.(type) {
			case map[string]interface{}, []interface{}:
				edges = mb.collectType(typed, owner, edges)
			case string:
				edges = mb.addRef(typed, owner, edges)
			}
		}
	}
	return edges
}

// addRef records a referenced name token unless it is a primitive, a complex
// type keyword, or a known primitive-backed logical type.
func (mb *moduleBuild) addRef(token string, owner *namedDef, edges []refEdge) []refEdge {
	if primitiveTypeNames[token] || namedTypeKinds[token] || token == "array" || token == "map" {
		return edges
	}
	if index := strings.IndexByte(token, '.'); index > 0 {
		if primitiveTypeNames[token[:index]] {
			return edges
		}
	}
	return append(edges, refEdge{
		token:     token,
		namespace: owner.namespace,
		fromType:  owner.fullName,
		source:    owner.source,
	})
}

func (mb *moduleBuild) sortedDefs() []*namedDef {
	names := make([]string, 0, len(mb.defs))
	for fullName := range mb.defs {
		names = append(names, fullName)
	}
	sort.Strings(names)
	defs := make([]*namedDef, 0, len(names))
	for _, fullName := range names {
		defs = append(defs, mb.defs[fullName])
	}
	return defs
}

// resolveName applies the Avro name resolution rules: qualified names resolve
// directly; short names resolve first in the enclosing namespace, then in the
// null namespace. Input order never affects the outcome.
func (mb *moduleBuild) resolveName(token, namespace string) (*namedDef, bool) {
	if strings.ContainsRune(token, '.') {
		def, ok := mb.defs[token]
		return def, ok
	}
	if namespace != "" {
		if def, ok := mb.defs[namespace+"."+token]; ok {
			return def, true
		}
	}
	def, ok := mb.defs[token]
	return def, ok
}

// validate walks the reachable closure from the root and classifies every
// parse, scan, and unknown-reference problem as fatal or diagnostic.
func (mb *moduleBuild) validate(root *namedDef) ([]error, []ModularDiagnostic) {
	parent := map[string]string{}
	reachable := map[string]bool{}
	reachableSources := map[string]bool{}
	var order []string

	queue := []string{root.fullName}
	reachable[root.fullName] = true
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		currentDef := mb.defs[current]
		reachableSources[currentDef.source] = true
		order = append(order, current)

		for _, edge := range mb.refs[current] {
			target, ok := mb.resolveName(edge.token, edge.namespace)
			if !ok {
				continue
			}
			if !reachable[target.fullName] {
				reachable[target.fullName] = true
				parent[target.fullName] = current
				queue = append(queue, target.fullName)
			}
		}
	}

	var fatal []error
	var diagnostics []ModularDiagnostic

	emitSourceError := func(se sourceError) {
		if mb.options.LenientUnreferencedModules && !reachableSources[se.source] {
			diagnostics = append(diagnostics, ModularDiagnostic{
				Sources: []string{se.source},
				Message: fmt.Sprintf("ignored problem in unreferenced module: %s", se.err),
			})
			return
		}
		fatal = append(fatal, se.err)
	}

	// Deterministic ordering for module-level failures.
	sort.SliceStable(mb.parseErrors, func(i, j int) bool { return mb.parseErrors[i].source < mb.parseErrors[j].source })
	sort.SliceStable(mb.scanErrors, func(i, j int) bool { return mb.scanErrors[i].source < mb.scanErrors[j].source })
	for _, se := range mb.parseErrors {
		emitSourceError(se)
	}
	for _, se := range mb.scanErrors {
		emitSourceError(se)
	}

	// Unknown references are fatal inside the reachable closure. In strict
	// mode they are fatal everywhere; in lenient mode they are diagnostics
	// outside the closure.
	for _, current := range order {
		currentDef := mb.defs[current]
		for _, edge := range mb.refs[current] {
			if _, ok := mb.resolveName(edge.token, edge.namespace); ok {
				continue
			}
			typeChain, sourceChain := mb.chainTo(current, parent, root)
			fatal = append(fatal, &UnknownTypeError{
				Reference:            edge.token,
				ReferencedFromType:   edge.fromType,
				ReferencedFromSource: edge.source,
				TypeChain:            typeChain,
				SourceChain:          sourceChain,
			})
		}
		_ = currentDef
	}

	if !mb.options.LenientUnreferencedModules {
		for _, def := range mb.sortedDefs() {
			if reachable[def.fullName] {
				continue
			}
			for _, edge := range mb.refs[def.fullName] {
				if _, ok := mb.resolveName(edge.token, edge.namespace); ok {
					continue
				}
				fatal = append(fatal, &UnknownTypeError{
					Reference:            edge.token,
					ReferencedFromType:   edge.fromType,
					ReferencedFromSource: edge.source,
				})
			}
		}
	} else {
		for _, def := range mb.sortedDefs() {
			if reachable[def.fullName] {
				continue
			}
			for _, edge := range mb.refs[def.fullName] {
				if _, ok := mb.resolveName(edge.token, edge.namespace); ok {
					continue
				}
				diagnostics = append(diagnostics, ModularDiagnostic{
					FullName: def.fullName,
					Sources:  []string{def.source},
					Message: fmt.Sprintf(
						"ignored unknown type %q referenced from unreferenced type %q",
						edge.token, def.fullName),
				})
			}
		}
	}

	return fatal, diagnostics
}

// chainTo reconstructs the root -> def resolution chain for diagnostics.
func (mb *moduleBuild) chainTo(fullName string, parent map[string]string, root *namedDef) ([]string, []string) {
	var reversed []string
	for {
		reversed = append(reversed, fullName)
		if fullName == root.fullName {
			break
		}
		next, ok := parent[fullName]
		if !ok {
			break
		}
		fullName = next
	}

	typeChain := make([]string, len(reversed))
	sourceChain := make([]string, len(reversed))
	for i := range reversed {
		name := reversed[len(reversed)-1-i]
		typeChain[i] = name
		sourceChain[i] = mb.defs[name].source
	}
	return typeChain, sourceChain
}

// linkAndBuild is phase 2: create a placeholder for every named type, then
// fill in the reachable closure. In strict mode every winning definition must
// also build successfully, even if the root never references it.
func (mb *moduleBuild) linkAndBuild(root *namedDef) error {
	mb.st = newSymbolTable()
	mb.filling = make(map[string]bool)

	for _, def := range mb.sortedDefs() {
		n, err := newName(def.fullName, nullNamespace, nullNamespace)
		if err != nil {
			return err
		}
		mb.st[def.fullName] = &Codec{typeName: n}
	}

	cb := &codecBuilder{
		mapBuilder:    buildCodecForTypeDescribedByMap,
		stringBuilder: mb.modularStringBuilder,
		sliceBuilder:  buildCodecForTypeDescribedBySlice,
		option:        mb.options.CodecOption,
	}
	mb.cb = cb

	if err := mb.fillDef(root, cb); err != nil {
		return err
	}

	if !mb.options.LenientUnreferencedModules {
		for _, def := range mb.sortedDefs() {
			if mb.filling[def.fullName] {
				continue
			}
			if err := mb.fillDef(def, cb); err != nil {
				return err
			}
		}
	}

	rootCodec := mb.st[root.fullName]
	canonical, err := mb.canonicalSchema(root)
	if err != nil {
		return err
	}
	rootCodec.schemaCanonical = canonical
	rootCodec.schemaOriginal = canonicalBody(root.schema)
	rootCodec.Rabin = rabin([]byte(canonical))
	rootCodec.soeHeader = []byte{0xC3, 0x01, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint64(rootCodec.soeHeader[2:], rootCodec.Rabin)

	return nil
}

// fillDef builds the codec for one named definition, reusing placeholders so
// mutually recursive records across modules link correctly.
func (mb *moduleBuild) fillDef(def *namedDef, cb *codecBuilder) error {
	if mb.filling[def.fullName] {
		return nil
	}
	mb.filling[def.fullName] = true

	if existing := mb.st[def.fullName]; existing != nil && existing.binaryFromNative != nil {
		return nil
	}

	_, err := buildCodec(mb.st, def.enclosingNS, def.schema, cb)
	if err != nil {
		return fmt.Errorf("source %q: cannot build codec for %q: %s", def.source, def.fullName, err)
	}
	return nil
}

// modularStringBuilder resolves abbreviated names per the Avro namespace rules
// before delegating to the standard builder. This is what makes short names
// bind by namespace rather than by which module was read first.
func (mb *moduleBuild) modularStringBuilder(st map[string]*Codec, enclosingNamespace, typeName string, schemaMap map[string]interface{}, cb *codecBuilder) (*Codec, error) {
	searchType := typeName
	if schemaMap != nil {
		if lt, ok := schemaMap["logicalType"].(string); ok && lt != "" {
			searchType = typeName + "." + lt
		}
	}

	// Named definitions win over primitives/keywords: first the qualified form
	// within the enclosing namespace, then the bare full name.
	if enclosingNamespace != "" {
		if def, ok := mb.defs[enclosingNamespace+"."+typeName]; ok {
			if err := mb.fillDef(def, cb); err != nil {
				return nil, err
			}
			return st[def.fullName], nil
		}
	}
	if def, ok := mb.defs[typeName]; ok {
		if err := mb.fillDef(def, cb); err != nil {
			return nil, err
		}
		return st[def.fullName], nil
	}

	return buildCodecForTypeDescribedByString(st, enclosingNamespace, searchType, schemaMap, cb)
}

// canonicalSchema renders the root type in Avro Parsing Canonical Form with
// every name reference fully qualified using the same namespace rules used for
// linking, so it is identical regardless of module input order.
func (mb *moduleBuild) canonicalSchema(root *namedDef) (string, error) {
	mb.rendered = make(map[string]bool)
	return mb.renderType(root.schema, root.enclosingNS)
}

func (mb *moduleBuild) renderType(node interface{}, namespace string) (string, error) {
	switch value := node.(type) {
	case nil:
		return "", fmt.Errorf("cannot render canonical schema from nil")
	case string:
		if def, ok := mb.resolveName(value, namespace); ok && !mb.rendered[def.fullName] {
			mb.rendered[def.fullName] = true
			return mb.renderNamed(def.schema, def.kind, def.enclosingNS)
		}
		return strconv.Quote(mb.qualifyToken(value, namespace)), nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(value), nil
	case []interface{}:
		parts := make([]string, len(value))
		for i, el := range value {
			rendered, err := mb.renderType(el, namespace)
			if err != nil {
				return "", err
			}
			parts[i] = rendered
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	case map[string]interface{}:
		if typeName, ok := value["type"].(string); ok && namedTypeKinds[typeName] {
			if rawName, hasName := value["name"]; hasName {
				if nameText, ok := rawName.(string); ok {
					namespaceValue, _ := value["namespace"].(string)
					if n, err := newName(nameText, namespaceValue, namespace); err == nil {
						if mb.rendered[n.fullName] {
							return strconv.Quote(n.fullName), nil
						}
						mb.rendered[n.fullName] = true
					}
				}
			}
		}
		return mb.renderObject(value, namespace)
	default:
		return "", fmt.Errorf("cannot render canonical schema node: %T", node)
	}
}

func (mb *moduleBuild) renderObject(obj map[string]interface{}, namespace string) (string, error) {
	typeName, _ := obj["type"].(string)

	if namedTypeKinds[typeName] {
		return mb.renderNamed(obj, typeName, namespace)
	}

	// A single-key {"type": "primitive"} reduces to the primitive name.
	if len(obj) == 1 {
		if t, ok := obj["type"].(string); ok && primitiveTypeNames[t] {
			return strconv.Quote(t), nil
		}
	}

	pairs := make(stringPairs, 0, len(obj))
	for key, raw := range obj {
		if _, ok := fieldOrder[key]; !ok {
			continue
		}
		var rendered string
		var err error
		switch key {
		case "name":
			rendered, err = mb.renderNameValue(raw, namespace)
		case "items", "values":
			rendered, err = mb.renderType(raw, namespace)
		case "type":
			rendered, err = mb.renderTypeKeyword(raw, namespace)
		case "size":
			rendered, err = renderSize(raw)
		default:
			rendered, err = mb.renderType(raw, namespace)
		}
		if err != nil {
			return "", err
		}
		pairs = append(pairs, stringPair{A: key, B: strconv.Quote(key) + ":" + rendered})
	}
	sort.Sort(byAvroFieldOrder(pairs))
	return "{" + strings.Join(pairs.Bs(), ",") + "}", nil
}

// renderNameValue renders the "name" attribute of an anonymous named object.
// Anonymous logical decimal fixeds reach this path and must be qualified using
// the enclosing namespace.
func (mb *moduleBuild) renderNameValue(raw interface{}, namespace string) (string, error) {
	text, ok := raw.(string)
	if !ok {
		return mb.renderType(raw, namespace)
	}
	n, err := newName(text, namespace, namespace)
	if err != nil {
		return "", err
	}
	return strconv.Quote(n.fullName), nil
}

func (mb *moduleBuild) renderTypeKeyword(raw interface{}, namespace string) (string, error) {
	switch value := raw.(type) {
	case string:
		return strconv.Quote(mb.qualifyToken(value, namespace)), nil
	default:
		return mb.renderType(raw, namespace)
	}
}

func renderSize(raw interface{}) (string, error) {
	switch value := raw.(type) {
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case string:
		size, err := strconv.ParseUint(value, 10, 0)
		if err != nil {
			return "", fmt.Errorf("fixed size ought to be number greater than zero: %v", value)
		}
		return strconv.FormatUint(size, 10), nil
	default:
		return "", fmt.Errorf("fixed size ought to be a number; received: %T", raw)
	}
}

// renderNamed renders record, enum, and fixed definitions with full names and
// only canonical attributes.
func (mb *moduleBuild) renderNamed(obj map[string]interface{}, kind, namespace string) (string, error) {
	nameValue, _ := obj["name"].(string)
	namespaceValue, _ := obj["namespace"].(string)
	n, err := newName(nameValue, namespaceValue, namespace)
	if err != nil {
		return "", err
	}
	childNamespace := n.namespace

	var pairs stringPairs
	pairs = append(pairs, stringPair{
		A: "name",
		B: strconv.Quote("name") + ":" + strconv.Quote(n.fullName),
	})
	pairs = append(pairs, stringPair{
		A: "type",
		B: strconv.Quote("type") + ":" + strconv.Quote(kind),
	})

	switch kind {
	case "record":
		fields, _ := obj["fields"].([]interface{})
		renderedFields := make([]string, len(fields))
		for i, field := range fields {
			fieldMap, ok := field.(map[string]interface{})
			if !ok {
				return "", fmt.Errorf("record %q field %d ought to be an object", n.fullName, i+1)
			}
			fieldName, _ := fieldMap["name"].(string)
			renderedType, err := mb.renderType(fieldMap["type"], childNamespace)
			if err != nil {
				return "", err
			}
			renderedFields[i] = "{" +
				strconv.Quote("name") + ":" + strconv.Quote(fieldName) + "," +
				strconv.Quote("type") + ":" + renderedType + "}"
		}
		pairs = append(pairs, stringPair{
			A: "fields",
			B: strconv.Quote("fields") + ":[" + strings.Join(renderedFields, ",") + "]",
		})
	case "enum":
		symbols, _ := obj["symbols"].([]interface{})
		renderedSymbols := make([]string, len(symbols))
		for i, symbol := range symbols {
			text, _ := symbol.(string)
			renderedSymbols[i] = strconv.Quote(text)
		}
		pairs = append(pairs, stringPair{
			A: "symbols",
			B: strconv.Quote("symbols") + ":[" + strings.Join(renderedSymbols, ",") + "]",
		})
	case "fixed":
		size, err := renderSize(obj["size"])
		if err != nil {
			return "", err
		}
		pairs = append(pairs, stringPair{
			A: "size",
			B: strconv.Quote("size") + ":" + size,
		})
	}

	sort.Sort(byAvroFieldOrder(pairs))
	return "{" + strings.Join(pairs.Bs(), ",") + "}", nil
}

// qualifyToken fully qualifies a named type reference, leaving primitives,
// keywords, and already-qualified names alone.
func (mb *moduleBuild) qualifyToken(token, namespace string) string {
	if primitiveTypeNames[token] || token == "array" || token == "map" {
		return token
	}
	if def, ok := mb.resolveName(token, namespace); ok {
		return def.fullName
	}
	return token
}
