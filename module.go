// Copyright [2026] LinkedIn Corp. Licensed under the Apache License, Version
// 2.0 (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.

package goavro

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// A Module is one in-memory schema document taking part in a modular build.
// Modules are read only from the Schema string supplied by the caller; the
// modular builder never performs file system reads or network fetches.
//
// A module document describes a single top-level named type: a record, enum,
// or fixed. Additional named types may be nested inside it.
type Module struct {
	// Source identifies where Schema came from, for example a file path or an
	// import URI. It is only used in diagnostics; it is never dereferenced.
	Source string

	// Schema is the JSON text of the module document.
	Schema string
}

// DuplicateNamePolicy determines how the modular builder reacts when the same
// fully qualified name is defined by more than one source module.
type DuplicateNamePolicy int

const (
	// DuplicateNameReject fails the build when a fully qualified name is
	// defined more than once, even when the definitions are textually
	// identical. This is the zero value and the default.
	DuplicateNameReject DuplicateNamePolicy = iota

	// DuplicateNameAcceptFirst keeps one definition, chosen deterministically
	// by source order rather than by input slice position, and reports every
	// defining source as a diagnostic.
	DuplicateNameAcceptFirst
)

// ModuleBuildOptions configures a modular codec build.
type ModuleBuildOptions struct {
	// DuplicateNames selects what happens when one fully qualified name is
	// defined by multiple modules. Defaults to DuplicateNameReject.
	DuplicateNames DuplicateNamePolicy

	// ErrorOnUnreferencedModules, when false (the default), downgrades errors
	// found in modules not reachable from the root type to non-fatal
	// diagnostics. When true, any malformed module fails the build whether or
	// not the root references it.
	//
	// Errors inside the transitive closure reachable from the root are always
	// fatal, in particular references to unknown named types.
	ErrorOnUnreferencedModules bool

	// CodecOption customizes the generated codec. When nil, the defaults from
	// DefaultCodecOption are used.
	CodecOption *CodecOption
}

// DiagnosticSeverity labels a Diagnostic.
type DiagnosticSeverity int

const (
	// DiagnosticError describes a defect. Whether it aborts the build depends
	// on the build options and on reachability from the root type.
	DiagnosticError DiagnosticSeverity = iota

	// DiagnosticInfo reports a condition tolerated under the selected policy,
	// for example a duplicate definition that was accepted.
	DiagnosticInfo
)

// Diagnostic describes a problem found while linking modules.
type Diagnostic struct {
	// Severity is DiagnosticError or DiagnosticInfo.
	Severity DiagnosticSeverity

	// Source is the identifier of the module where the problem was found, or
	// the empty string for problems that span modules.
	Source string

	// JSONPath is a dotted/bracketed path to the offending schema node, for
	// example "fields[0].type". It may be empty.
	JSONPath string

	// Message is the human readable description.
	Message string

	err     error
	owner   string // full name of the definition containing the defect
	parseOK bool   // true for JSON/shape errors of a whole module
	unknown *unknownRef
}

type unknownRef struct {
	ref       string
	namespace string
	owner     string
}

func (d Diagnostic) Error() string {
	var b strings.Builder
	if d.Source != "" {
		fmt.Fprintf(&b, "module %q", d.Source)
		if d.JSONPath != "" {
			fmt.Fprintf(&b, " at %s", d.JSONPath)
		}
		b.WriteString(": ")
	} else if d.JSONPath != "" {
		fmt.Fprintf(&b, "%s: ", d.JSONPath)
	}
	b.WriteString(d.Message)
	return b.String()
}

// Unwrap exposes the underlying error so errors.Is and errors.As keep working.
func (d Diagnostic) Unwrap() error { return d.err }

// ErrDuplicateName is reported when a fully qualified type name is defined in
// more than one module.
type ErrDuplicateName struct {
	// FullName is the duplicated fully qualified name.
	FullName string
	// Sources lists every module defining FullName, in deterministic order.
	Sources []string
}

func (e *ErrDuplicateName) Error() string {
	return fmt.Sprintf("duplicate definition of %q in sources: %s", e.FullName, strings.Join(e.Sources, ", "))
}

// ErrUnknownType is reported for a name reference that cannot be resolved.
type ErrUnknownType struct {
	// Reference is the name as it appeared in the schema.
	Reference string
	// Namespace is the enclosing namespace used while resolving Reference.
	Namespace string
	// ReferencedBy is the fully qualified name that held the dangling
	// reference; it is empty for an unresolved root.
	ReferencedBy string
}

func (e *ErrUnknownType) Error() string {
	var b strings.Builder
	if e.Namespace == "" {
		fmt.Fprintf(&b, "unknown type name: %q", e.Reference)
	} else {
		fmt.Fprintf(&b, "unknown type name: %q in namespace %q", e.Reference, e.Namespace)
	}
	if e.ReferencedBy != "" {
		fmt.Fprintf(&b, " (referenced from %q)", e.ReferencedBy)
	}
	return b.String()
}

// ErrAmbiguousRoot is reported when a short root name matches more than one
// fully qualified name in different namespaces.
type ErrAmbiguousRoot struct {
	RootName string
	Matches  []string
}

func (e *ErrAmbiguousRoot) Error() string {
	return fmt.Sprintf("root name %q is ambiguous; matches: %s", e.RootName, strings.Join(e.Matches, ", "))
}

// ModuleBuildResult is the result of BuildCodecFromModules.
type ModuleBuildResult struct {
	// Codec is the codec for the root type. It is nil when the build failed.
	Codec *Codec

	// Diagnostics lists every problem found, in deterministic order. Fatal
	// problems appear both here and as the returned error.
	Diagnostics []Diagnostic
}

// moduleDef is a named definition harvested from a module document.
type moduleDef struct {
	fullName  string
	namespace string
	source    string
	jsonPath  string
	node      map[string]interface{}
	module    *parsedModule
}

// parsedModule is the result of harvesting one Module document.
type parsedModule struct {
	source    string
	original  string
	root      interface{}
	defs      []*moduleDef
	defByNode map[uintptr]*moduleDef
}

// schemaBearingKeys enumerates object keys whose values are Avro schemas.
// Other string values (enum symbols, defaults, docs) must never be treated as
// type references.
var schemaBearingKeys = map[string]bool{
	"type":   true,
	"items":  true,
	"values": true,
}

// NewCodecFromModules compiles a set of in-memory schema documents that share
// named record, enum, and fixed types and returns a Codec for rootName.
//
// Fully qualified names from every module are registered before any
// reference is resolved, so records may recurse across modules and short
// names resolve against their enclosing namespace rather than against module
// input order.
//
// No file system or network access is performed: only the schema strings
// supplied in modules are read. Use BuildCodecFromModules to inspect
// diagnostics and configure duplicate name and unreferenced module handling.
func NewCodecFromModules(modules []Module, rootName string) (*Codec, error) {
	return NewCodecFromModulesWithOptions(modules, rootName, nil)
}

// NewCodecFromModulesWithOptions is NewCodecFromModules with explicit options.
func NewCodecFromModulesWithOptions(modules []Module, rootName string, opts *ModuleBuildOptions) (*Codec, error) {
	result, err := BuildCodecFromModules(modules, rootName, opts)
	if err != nil {
		return nil, err
	}
	return result.Codec, nil
}

// BuildCodecFromModules links modules with full diagnostics. The result
// carries every diagnostic collected; a non-nil error indicates the build was
// aborted and ModuleBuildResult.Codec is nil.
//
// The returned Codec's Schema and CanonicalSchema are both the Parsing
// Canonical Form of the linked root type, so they are identical regardless
// of module input order.
func BuildCodecFromModules(modules []Module, rootName string, opts *ModuleBuildOptions) (*ModuleBuildResult, error) {
	if opts == nil {
		opts = &ModuleBuildOptions{}
	}
	result := &ModuleBuildResult{}
	addDiag := func(d Diagnostic) { result.Diagnostics = append(result.Diagnostics, d) }
	fail := func(d Diagnostic) (*ModuleBuildResult, error) {
		addDiag(d)
		return result, d
	}

	if len(modules) == 0 {
		return fail(Diagnostic{Severity: DiagnosticError, Message: "no schema modules supplied"})
	}
	if strings.TrimSpace(rootName) == "" {
		return fail(Diagnostic{Severity: DiagnosticError, Message: "root type name ought to be non-empty"})
	}

	// Deterministic processing order. Linking never depends on this order;
	// messages and duplicate selection merely stay stable across input
	// permutations.
	ordered := make([]Module, len(modules))
	copy(ordered, modules)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Source != ordered[j].Source {
			return ordered[i].Source < ordered[j].Source
		}
		return ordered[i].Schema < ordered[j].Schema
	})

	var globals []*moduleDef
	groups := map[string][]*moduleDef{}
	parsed := make([]*parsedModule, 0, len(ordered))

	// Phase 1: parse every document and collect every named definition.
	for _, mod := range ordered {
		pm, diags := parseModule(mod)
		result.Diagnostics = append(result.Diagnostics, diags...)
		if pm == nil {
			continue
		}
		parsed = append(parsed, pm)
		for _, def := range pm.defs {
			globals = append(globals, def)
			groups[def.fullName] = append(groups[def.fullName], def)
		}
	}

	sort.Slice(globals, func(i, j int) bool { return globals[i].fullName < globals[j].fullName })

	// Duplicate full names follow an explicit policy and every defining source
	// is reported, even when the bodies are identical.
	registry := map[string]*moduleDef{}
	seen := map[string]bool{}
	for _, def := range globals {
		if seen[def.fullName] {
			continue
		}
		seen[def.fullName] = true
		same := groups[def.fullName]
		if len(same) == 1 {
			registry[def.fullName] = same[0]
			continue
		}
		sources := make([]string, len(same))
		for i, d := range same {
			sources[i] = d.source
		}
		dupErr := &ErrDuplicateName{FullName: def.fullName, Sources: sources}
		switch opts.DuplicateNames {
		case DuplicateNameAcceptFirst:
			addDiag(Diagnostic{Severity: DiagnosticInfo, Message: dupErr.Error(), err: dupErr})
			registry[def.fullName] = same[0]
		default:
			return fail(Diagnostic{Severity: DiagnosticError, Message: dupErr.Error(), err: dupErr})
		}
	}

	// Phase 2a: rewrite short references to full names using the enclosing
	// namespace of each reference, never the order modules were supplied.
	// Qualification runs globally in one pass, so the same reference resolves
	// identically no matter which module mentions it first.
	result.Diagnostics = append(result.Diagnostics, qualifyAllNames(parsed, registry)...)

	// Phase 2b: resolve the root and compute the reachable closure.
	rootDef, rootDiag := resolveRootDef(rootName, registry)
	if rootDiag != nil {
		return fail(*rootDiag)
	}

	reachableDefs, reachableSources := reachableClosure(rootDef, registry)

	// Fatal errors inside the reachable closure always reject the build.
	reachableNameSet := map[string]bool{}
	for d := range reachableDefs {
		reachableNameSet[d] = true
	}
	for _, d := range result.Diagnostics {
		if d.Severity != DiagnosticError {
			continue
		}
		if d.unknown != nil && reachableNameSet[d.unknown.owner] {
			return fail(d)
		}
		if d.parseOK && (reachableSources[d.Source] || opts.ErrorOnUnreferencedModules) {
			return fail(d)
		}
	}

	// Unreachable modules may still carry unknown references or compile
	// errors. They are diagnostics unless the caller opted into strictness.
	if opts.ErrorOnUnreferencedModules {
		for _, d := range result.Diagnostics {
			if d.Severity == DiagnosticError {
				return fail(d)
			}
		}
	}

	// Phase 3: pre-register a stub codec for every name, then compile the
	// root. Cross-file mutual recursion works because all names are bound up
	// front, and a name mid-compilation resolves to its own stub.
	option := opts.CodecOption
	if option == nil {
		option = DefaultCodecOption()
	}
	st := newSymbolTable()
	defByName := map[string]*moduleDef{}
	for _, def := range globals {
		chosen, ok := registry[def.fullName]
		if !ok || chosen != def || defByName[def.fullName] != nil {
			continue
		}
		defByName[def.fullName] = def
		n, err := newName(def.fullName, nullNamespace, nullNamespace)
		if err != nil {
			d := Diagnostic{Severity: DiagnosticError, Source: def.source, JSONPath: def.jsonPath, Message: err.Error(), err: err}
			if reachableNameSet[def.fullName] {
				return fail(d)
			}
			addDiag(d)
			continue
		}
		st[def.fullName] = &Codec{typeName: n, prefillStub: true}
	}

	ms := &modularState{defByName: defByName, compiling: map[string]bool{}}
	cb := &codecBuilder{
		mapBuilder: buildCodecForTypeDescribedByMap,
		stringBuilder: func(st map[string]*Codec, enclosingNamespace string, typeName string, schemaMap map[string]interface{}, inner *codecBuilder) (*Codec, error) {
			return ms.resolveString(st, enclosingNamespace, typeName, schemaMap, inner)
		},
		sliceBuilder: buildCodecForTypeDescribedBySlice,
		option:       option,
	}

	// Compile every reachable definition in deterministic name order. The
	// first compilation pulls in its dependencies lazily; later iterations
	// reuse filled codecs. Compiling per definition attributes errors to the
	// originating source rather than to whichever root reached them.
	reachableNames := make([]string, 0, len(reachableNameSet))
	for fullName := range reachableNameSet {
		reachableNames = append(reachableNames, fullName)
	}
	sort.Strings(reachableNames)
	for _, fullName := range reachableNames {
		if cd, ok := st[fullName]; ok && !cd.prefillStub {
			continue
		}
		def := registry[fullName]
		if _, err := cb.mapBuilder(st, def.namespace, def.node, cb); err != nil {
			return fail(Diagnostic{
				Severity: DiagnosticError,
				Source:   def.source,
				JSONPath: def.jsonPath,
				Message:  err.Error(),
				err:      err,
			})
		}
	}

	rootCodec := st[rootDef.fullName]

	canonical, err := parsingCanonicalForm(rootDef.node, "", map[string]string{})
	if err != nil {
		return fail(Diagnostic{Severity: DiagnosticError, Source: rootDef.source, Message: err.Error(), err: err})
	}

	rootCodec.schemaCanonical = canonical
	rootCodec.Rabin = rabin([]byte(canonical))
	rootCodec.soeHeader = []byte{0xC3, 0x01, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint64(rootCodec.soeHeader[2:], rootCodec.Rabin)
	rootCodec.schemaOriginal = canonical

	result.Codec = rootCodec
	return result, nil
}

// modularState lazily compiles stub codecs while preserving cycles.
type modularState struct {
	defByName map[string]*moduleDef
	compiling map[string]bool
}

func (ms *modularState) resolveString(st map[string]*Codec, enclosingNamespace, typeName string, schemaMap map[string]interface{}, cb *codecBuilder) (*Codec, error) {
	if cd, ok := st[typeName]; ok && cd.prefillStub {
		// A cycle: return the in-progress stub; the caller's closure will
		// fill it in on the way back up.
		if ms.compiling[typeName] {
			return cd, nil
		}
		def := ms.defByName[typeName]
		if def == nil {
			return cd, nil
		}
		ms.compiling[typeName] = true
		defer delete(ms.compiling, typeName)
		if _, err := cb.mapBuilder(st, def.namespace, def.node, cb); err != nil {
			return nil, err
		}
		return st[typeName], nil
	}
	return buildCodecForTypeDescribedByString(st, enclosingNamespace, typeName, schemaMap, cb)
}

// parseModule parses one module document and harvests its named definitions.
// A nil module with error diagnostics means the document could not be used at
// all.
func parseModule(mod Module) (*parsedModule, []Diagnostic) {
	diag := func(message string, err error) Diagnostic {
		return Diagnostic{Severity: DiagnosticError, Source: mod.Source, Message: message, err: err, parseOK: true}
	}

	var root interface{}
	if err := json.Unmarshal([]byte(mod.Schema), &root); err != nil {
		return nil, []Diagnostic{diag(fmt.Sprintf("cannot unmarshal schema JSON: %s", err), err)}
	}
	rootMap, ok := root.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("module schema ought to be a JSON object describing a record, enum, or fixed type; received: %T", root)
		return nil, []Diagnostic{diag(err.Error(), err)}
	}
	t, _ := rootMap["type"].(string)
	if t != "record" && t != "enum" && t != "fixed" {
		err := fmt.Errorf("module top-level schema ought to be a named record, enum, or fixed type; received type: %v", rootMap["type"])
		return nil, []Diagnostic{diag(err.Error(), err)}
	}

	pm := &parsedModule{source: mod.Source, original: mod.Schema, root: root, defByNode: map[uintptr]*moduleDef{}}

	var diags []Diagnostic
	var harvest func(node interface{}, path, ens string, owner *moduleDef) *moduleDef
	harvest = func(node interface{}, path, ens string, owner *moduleDef) *moduleDef {
		switch v := node.(type) {
		case map[string]interface{}:
			ns := effectiveNamespace(v, ens)
			if typeName, ok := v["type"].(string); ok && (typeName == "record" || typeName == "enum" || typeName == "fixed") {
				def, err := defFromNode(v, ns, path, mod.Source, pm)
				if err != nil {
					diags = append(diags, Diagnostic{
						Severity: DiagnosticError,
						Source:   mod.Source,
						JSONPath: path,
						Message:  err.Error(),
						err:      err,
					})
					return owner
				}
				pm.defs = append(pm.defs, def)
				pm.defByNode[reflect.ValueOf(v).Pointer()] = def
				owner = def
				ns = def.namespace
			}
			forEachSchemaChild(v, func(key string, child interface{}, childPath string) {
				harvest(child, path+"."+childPath, ns, owner)
			}, path)
		case []interface{}:
			for i, child := range v {
				harvest(child, fmt.Sprintf("%s[%d]", path, i), ens, owner)
			}
		}
		return owner
	}

	harvest(rootMap, "$", "", nil)
	return pm, diags
}

func defFromNode(node map[string]interface{}, ns, path, source string, pm *parsedModule) (*moduleDef, error) {
	n, err := newNameFromSchemaMap(ns, node)
	if err != nil {
		return nil, err
	}
	return &moduleDef{
		fullName:  n.fullName,
		namespace: n.namespace,
		source:    source,
		jsonPath:  path,
		node:      node,
		module:    pm,
	}, nil
}

// effectiveNamespace returns the namespace in effect at node. An explicitly
// empty namespace resets the enclosing namespace, matching Avro semantics.
func effectiveNamespace(node map[string]interface{}, ens string) string {
	if raw, ok := node["namespace"]; ok {
		if ns, ok := raw.(string); ok {
			return ns
		}
	}
	return ens
}

// forEachSchemaChild invokes fn for every sub-schema carried by node. key is
// the object key (or "" for array members); path is the JSON path fragment.
func forEachSchemaChild(node map[string]interface{}, fn func(key string, child interface{}, path string), parentPath string) {
	for key := range schemaBearingKeys {
		child, ok := node[key]
		if !ok {
			continue
		}
		switch v := child.(type) {
		case map[string]interface{}, string:
			fn(key, child, key)
		case []interface{}:
			for i := range v {
				fn(key, v[i], fmt.Sprintf("%s[%d]", key, i))
			}
		}
	}
	if fields, ok := node["fields"].([]interface{}); ok {
		for i, field := range fields {
			fn("fields", field, fmt.Sprintf("fields[%d]", i))
		}
	}
}

// qualifyAllNames rewrites every resolvable short name reference into its
// full name across every parsed module and returns diagnostics for references
// that cannot resolve. One global pass guarantees order-independent results.
func qualifyAllNames(parsed []*parsedModule, registry map[string]*moduleDef) []Diagnostic {
	var diags []Diagnostic
	defByNode := map[uintptr]*moduleDef{}
	for _, pm := range parsed {
		for node, def := range pm.defByNode {
			defByNode[node] = def
		}
	}

	var qualify func(node interface{}, ens string, owner *moduleDef)

	// qualifySlice walks a schema array (a union), rewriting string members
	// in place via set.
	var qualifySlice func(arr []interface{}, ns string, owner *moduleDef, set func(int, string))

	qualifyRef := func(ref, ns string, owner *moduleDef, replace func(string)) {
		if isPrimitiveOrContainer(ref) {
			return
		}
		if full, ok := resolveRef(ref, ns, registry); ok {
			replace(full)
			return
		}
		ownerName := ""
		if owner != nil {
			ownerName = owner.fullName
		}
		err := &ErrUnknownType{Reference: ref, Namespace: ns, ReferencedBy: ownerName}
		diags = append(diags, Diagnostic{
			Severity: DiagnosticError,
			Source:   ownerSource(owner),
			Message:  err.Error(),
			err:      err,
			owner:    ownerName,
			unknown:  &unknownRef{ref: ref, namespace: ns, owner: ownerName},
		})
	}

	qualifySlice = func(arr []interface{}, ns string, owner *moduleDef, set func(int, string)) {
		for i, member := range arr {
			index := i
			switch m := member.(type) {
			case string:
				qualifyRef(m, ns, owner, func(full string) { set(index, full) })
			case map[string]interface{}:
				qualify(m, ns, owner)
			case []interface{}:
				qualifySlice(m, ns, owner, func(j int, full string) { m[j] = full })
			}
		}
	}

	qualify = func(node interface{}, ens string, owner *moduleDef) {
		switch v := node.(type) {
		case map[string]interface{}:
			ns := effectiveNamespace(v, ens)
			if typeName, ok := v["type"].(string); ok && (typeName == "record" || typeName == "enum" || typeName == "fixed") {
				if def, ok := defByNode[reflect.ValueOf(v).Pointer()]; ok {
					owner = def
				}
			}
			switch t := v["type"].(type) {
			case string:
				qualifyRef(t, ns, owner, func(full string) { v["type"] = full })
			case []interface{}:
				qualifySlice(t, ns, owner, func(i int, full string) { t[i] = full })
			case map[string]interface{}:
				qualify(t, ns, owner)
			}
			switch items := v["items"].(type) {
			case string:
				qualifyRef(items, ns, owner, func(full string) { v["items"] = full })
			case []interface{}:
				qualifySlice(items, ns, owner, func(i int, full string) { items[i] = full })
			case map[string]interface{}:
				qualify(items, ns, owner)
			}
			switch values := v["values"].(type) {
			case string:
				qualifyRef(values, ns, owner, func(full string) { v["values"] = full })
			case []interface{}:
				qualifySlice(values, ns, owner, func(i int, full string) { values[i] = full })
			case map[string]interface{}:
				qualify(values, ns, owner)
			}
			if fields, ok := v["fields"].([]interface{}); ok {
				for _, field := range fields {
					fm, ok := field.(map[string]interface{})
					if !ok {
						continue
					}
					switch ft := fm["type"].(type) {
					case string:
						qualifyRef(ft, ns, owner, func(full string) { fm["type"] = full })
					case []interface{}:
						qualifySlice(ft, ns, owner, func(i int, full string) { ft[i] = full })
					case map[string]interface{}:
						qualify(ft, ns, owner)
					}
				}
			}
		case []interface{}:
			qualifySlice(v, ens, owner, func(i int, full string) { v[i] = full })
		}
	}

	for _, pm := range parsed {
		qualify(pm.root, "", nil)
	}
	return diags
}

func ownerSource(owner *moduleDef) string {
	if owner == nil {
		return ""
	}
	return owner.source
}

func fullNameOf(node map[string]interface{}, ens string) string {
	n, err := newNameFromSchemaMap(ens, node)
	if err != nil {
		return ""
	}
	return n.fullName
}

func isPrimitiveOrContainer(name string) bool {
	switch name {
	case "null", "boolean", "int", "long", "float", "double", "bytes", "string",
		"record", "enum", "fixed", "array", "map":
		return true
	}
	return false
}

// resolveRef applies Avro name resolution: a dotted name is full, otherwise
// the enclosing namespace is tried before the null namespace.
func resolveRef(ref, ns string, registry map[string]*moduleDef) (string, bool) {
	if strings.ContainsRune(ref, '.') {
		if _, ok := registry[ref]; ok {
			return ref, true
		}
		return "", false
	}
	if ns != "" {
		full := ns + "." + ref
		if _, ok := registry[full]; ok {
			return full, true
		}
	}
	if _, ok := registry[ref]; ok {
		return ref, true
	}
	return "", false
}

// resolveRootDef selects the definition named by rootName, applying the same
// short name rules and rejecting ambiguous short names.
func resolveRootDef(rootName string, registry map[string]*moduleDef) (*moduleDef, *Diagnostic) {
	if strings.ContainsRune(rootName, '.') {
		if def, ok := registry[rootName]; ok {
			return def, nil
		}
		err := &ErrUnknownType{Reference: rootName}
		d := Diagnostic{Severity: DiagnosticError, Message: err.Error(), err: err}
		return nil, &d
	}
	var exact []*moduleDef
	for fullName, def := range registry {
		if fullName == rootName || strings.HasSuffix(fullName, "."+rootName) {
			exact = append(exact, def)
		}
	}
	switch len(exact) {
	case 1:
		return exact[0], nil
	case 0:
		err := &ErrUnknownType{Reference: rootName}
		d := Diagnostic{Severity: DiagnosticError, Message: err.Error(), err: err}
		return nil, &d
	default:
		names := make([]string, len(exact))
		for i, def := range exact {
			names[i] = def.fullName
		}
		sort.Strings(names)
		err := &ErrAmbiguousRoot{RootName: rootName, Matches: names}
		d := Diagnostic{Severity: DiagnosticError, Message: err.Error(), err: err}
		return nil, &d
	}
}

// reachableClosure returns every definition and source module transitively
// referenced from root after phase 2a qualified the references.
func reachableClosure(root *moduleDef, registry map[string]*moduleDef) (map[string]*moduleDef, map[string]bool) {
	defs := map[string]*moduleDef{}
	sources := map[string]bool{}
	var visit func(def *moduleDef)
	visit = func(def *moduleDef) {
		if def == nil {
			return
		}
		if _, ok := defs[def.fullName]; ok {
			return
		}
		defs[def.fullName] = def
		sources[def.source] = true
		for _, ref := range collectRefs(def.node) {
			if target, ok := registry[ref]; ok {
				visit(target)
			}
		}
	}
	visit(root)
	return defs, sources
}

// collectRefs returns every named reference inside node. After qualification
// all resolvable user-type references are full names; unknown references are
// simply absent from the registry by the caller.
func collectRefs(node interface{}) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(s string) {
		if !isPrimitiveOrContainer(s) && !seen[s] {
			seen[s] = true
			refs = append(refs, s)
		}
	}
	var walk func(interface{})
	var walkValue func(interface{})
	walkValue = func(node interface{}) {
		switch v := node.(type) {
		case string:
			add(v)
		case map[string]interface{}:
			walk(v)
		case []interface{}:
			for _, member := range v {
				walkValue(member)
			}
		}
	}
	walk = func(node interface{}) {
		switch v := node.(type) {
		case map[string]interface{}:
			if child, ok := v["type"]; ok {
				walkValue(child)
			}
			if child, ok := v["items"]; ok {
				walkValue(child)
			}
			if child, ok := v["values"]; ok {
				walkValue(child)
			}
			if fields, ok := v["fields"].([]interface{}); ok {
				for _, field := range fields {
					if fm, ok := field.(map[string]interface{}); ok {
						walkValue(fm["type"])
					}
				}
			}
		case []interface{}:
			for _, child := range v {
				walkValue(child)
			}
		}
	}
	walk(node)
	return refs
}
