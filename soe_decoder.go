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
	"container/list"
	"context"
	"encoding/binary"
	"fmt"
	"sync"
)

// SchemaResolver returns the Avro schema associated with the given Rabin
// fingerprint of a schema's Parsing Canonical Form. It is used by SOEDecoder
// to fetch the schema for a Single-Object Encoded datum when the local cache
// does not already have a Codec for the datum's schema fingerprint.
//
// The provided context carries the deadline and cancellation of the decode
// operation, and implementations ought to honor it. Implementations must be
// safe for concurrent use.
type SchemaResolver interface {
	ResolveSchema(ctx context.Context, fingerprint uint64) (string, error)
}

// SchemaResolverFunc adapts a function to the SchemaResolver interface.
type SchemaResolverFunc func(ctx context.Context, fingerprint uint64) (string, error)

// ResolveSchema calls f(ctx, fingerprint).
func (f SchemaResolverFunc) ResolveSchema(ctx context.Context, fingerprint uint64) (string, error) {
	return f(ctx, fingerprint)
}

// ErrUnknownSchemaFingerprint is returned by a SchemaResolver when it has no
// schema for the requested fingerprint.
type ErrUnknownSchemaFingerprint uint64

func (e ErrUnknownSchemaFingerprint) Error() string {
	return fmt.Sprintf("unknown schema fingerprint: %#x", uint64(e))
}

// MemoryResolver is an in-memory SchemaResolver built from a fixed set of
// schemas. It computes the Rabin fingerprint of each schema's Parsing
// Canonical Form at construction, and serves lookups without contacting any
// external schema registry. It records how many times it has been invoked so
// callers may observe resolver activity.
type MemoryResolver struct {
	mu      sync.Mutex
	schemas map[uint64]string
	calls   int
}

// NewMemoryResolver returns a MemoryResolver that serves the provided
// schemas. It returns an error when any schema cannot be compiled, or when
// two schemas share the same fingerprint.
func NewMemoryResolver(schemas ...string) (*MemoryResolver, error) {
	r := &MemoryResolver{schemas: make(map[uint64]string, len(schemas))}
	for _, schema := range schemas {
		codec, err := NewCodec(schema)
		if err != nil {
			return nil, fmt.Errorf("cannot create memory resolver: %s", err)
		}
		if _, ok := r.schemas[codec.Rabin]; ok {
			return nil, fmt.Errorf("cannot create memory resolver: duplicate schema fingerprint: %#x", codec.Rabin)
		}
		r.schemas[codec.Rabin] = schema
	}
	return r, nil
}

// ResolveSchema returns the schema for the fingerprint, or
// ErrUnknownSchemaFingerprint when the fingerprint is not known.
func (r *MemoryResolver) ResolveSchema(_ context.Context, fingerprint uint64) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	schema, ok := r.schemas[fingerprint]
	if !ok {
		return "", ErrUnknownSchemaFingerprint(fingerprint)
	}
	return schema, nil
}

// Calls returns how many times ResolveSchema has been invoked.
func (r *MemoryResolver) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// CodecCache is a bounded least-recently-used cache of Codecs keyed by schema
// Rabin fingerprint. It is created and owned by the caller, so the caller
// controls its capacity and lifetime, and may share it between multiple
// SOEDecoders. Only successfully verified Codecs are stored: a schema
// resolution failure is never cached, so an unknown fingerprint does not
// permanently resolve to not-found. A CodecCache is safe for concurrent use.
type CodecCache struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[uint64]*list.Element
}

type codecCacheEntry struct {
	fingerprint uint64
	codec       *Codec
}

// NewCodecCache returns a CodecCache that holds at most capacity Codecs. It
// returns an error when capacity is less than one.
func NewCodecCache(capacity int) (*CodecCache, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("cannot create codec cache: capacity must be at least 1: %d", capacity)
	}
	return &CodecCache{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[uint64]*list.Element),
	}, nil
}

// Get returns the Codec for the fingerprint, marking it most recently used.
func (c *CodecCache) Get(fingerprint uint64) (*Codec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[fingerprint]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*codecCacheEntry).codec, true
	}
	return nil, false
}

// Add stores the Codec for the fingerprint, evicting the least recently used
// entry when the cache is at capacity.
func (c *CodecCache) Add(fingerprint uint64, codec *Codec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[fingerprint]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*codecCacheEntry).codec = codec
		return
	}
	c.items[fingerprint] = c.ll.PushFront(&codecCacheEntry{fingerprint: fingerprint, codec: codec})
	for c.ll.Len() > c.capacity {
		el := c.ll.Back()
		c.ll.Remove(el)
		delete(c.items, el.Value.(*codecCacheEntry).fingerprint)
	}
}

// Len returns the number of Codecs currently in the cache.
func (c *CodecCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// ErrSOEShortBuffer is returned when a buffer is too short to contain a
// Single-Object Encoding header.
type ErrSOEShortBuffer struct {
	Length int
	Needed int
}

func (e ErrSOEShortBuffer) Error() string {
	return fmt.Sprintf("cannot decode single-object encoding: short buffer: have %d bytes, need %d", e.Length, e.Needed)
}

// ErrSOEBadMagic is returned when a buffer does not start with the
// Single-Object Encoding magic bytes.
type ErrSOEBadMagic [2]byte

func (e ErrSOEBadMagic) Error() string {
	return fmt.Sprintf("cannot decode single-object encoding: unknown magic prefix: %#x", []byte(e[:]))
}

// ErrSchemaResolution is returned when the SchemaResolver fails to provide a
// usable schema for the fingerprint, either by returning an error or by
// returning a schema that cannot be compiled.
type ErrSchemaResolution struct {
	Fingerprint uint64
	Err         error
}

func (e ErrSchemaResolution) Error() string {
	return fmt.Sprintf("cannot resolve schema for fingerprint %#x: %s", e.Fingerprint, e.Err)
}

// Unwrap returns the underlying resolver error.
func (e ErrSchemaResolution) Unwrap() error { return e.Err }

// ErrSchemaFingerprintMismatch is returned when the schema provided by the
// SchemaResolver has a Rabin fingerprint that differs from the fingerprint in
// the message header. Decoding is aborted rather than trusting the resolver.
type ErrSchemaFingerprintMismatch struct {
	Message uint64
	Schema  uint64
}

func (e ErrSchemaFingerprintMismatch) Error() string {
	return fmt.Sprintf("schema fingerprint mismatch: message has %#x but resolved schema has %#x", e.Message, e.Schema)
}

// ErrSOEPayload is returned when the payload following a valid Single-Object
// Encoding header cannot be decoded with the resolved schema.
type ErrSOEPayload struct {
	Err error
}

func (e ErrSOEPayload) Error() string {
	return fmt.Sprintf("cannot decode single-object payload: %s", e.Err)
}

// Unwrap returns the underlying decoding error.
func (e ErrSOEPayload) Unwrap() error { return e.Err }

// SOEDecoder decodes Single-Object Encoded datums whose schema is not known
// in advance. For each message it reads the magic bytes and schema
// fingerprint from the header, obtains a Codec for the fingerprint — from the
// caller-provided cache when possible, otherwise by asking the resolver for
// the schema exactly once per message — verifies that the resolved schema's
// fingerprint matches the message fingerprint, and then decodes the payload.
type SOEDecoder struct {
	resolver SchemaResolver
	cache    *CodecCache
}

// NewSOEDecoder returns a SOEDecoder that obtains schemas from resolver and
// stores verified Codecs in the caller-owned cache.
func NewSOEDecoder(resolver SchemaResolver, cache *CodecCache) (*SOEDecoder, error) {
	if resolver == nil {
		return nil, fmt.Errorf("cannot create single-object decoder: resolver must not be nil")
	}
	if cache == nil {
		return nil, fmt.Errorf("cannot create single-object decoder: cache must not be nil")
	}
	return &SOEDecoder{resolver: resolver, cache: cache}, nil
}

// NativeFromSingle decodes one Single-Object Encoded datum from buf,
// returning the decoded datum and the unconsumed bytes that follow the datum.
// The schema for the message fingerprint is resolved at most once per call:
// union branches are attempted against the already-resolved Codec and never
// trigger additional resolver calls. Resolution failures and fingerprint
// mismatches are not cached.
func (d *SOEDecoder) NativeFromSingle(ctx context.Context, buf []byte) (interface{}, []byte, error) {
	if len(buf) < soeHeaderLen {
		return nil, buf, ErrSOEShortBuffer{Length: len(buf), Needed: soeHeaderLen}
	}
	if buf[0] != 0xC3 || buf[1] != 0x01 {
		return nil, buf, ErrSOEBadMagic{buf[0], buf[1]}
	}
	fingerprint := binary.LittleEndian.Uint64(buf[soeMagicPrefix:soeHeaderLen])

	codec, ok := d.cache.Get(fingerprint)
	if !ok {
		var err error
		codec, err = d.resolve(ctx, fingerprint)
		if err != nil {
			return nil, buf, err
		}
		d.cache.Add(fingerprint, codec)
	}

	datum, remaining, err := codec.NativeFromBinary(buf[soeHeaderLen:])
	if err != nil {
		return nil, buf, ErrSOEPayload{Err: err}
	}
	return datum, remaining, nil
}

// resolve asks the resolver for the schema of fingerprint, compiles it, and
// verifies the compiled schema's fingerprint against the message fingerprint
// before any decoding is attempted.
func (d *SOEDecoder) resolve(ctx context.Context, fingerprint uint64) (*Codec, error) {
	schema, err := d.resolver.ResolveSchema(ctx, fingerprint)
	if err != nil {
		return nil, ErrSchemaResolution{Fingerprint: fingerprint, Err: err}
	}
	codec, err := NewCodec(schema)
	if err != nil {
		return nil, ErrSchemaResolution{Fingerprint: fingerprint, Err: err}
	}
	if codec.Rabin != fingerprint {
		return nil, ErrSchemaFingerprintMismatch{Message: fingerprint, Schema: codec.Rabin}
	}
	return codec, nil
}
