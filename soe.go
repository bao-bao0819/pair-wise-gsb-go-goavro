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
	"strconv"
	"sync"
)

// SchemaResolver returns the Avro schema, in JSON text form, that corresponds
// to the provided 64-bit Rabin fingerprint read from a single-object encoded
// message. Implementations might consult an in-memory set of known schemas, a
// schema registry, or any other schema source. The provided context is
// forwarded to the implementation so callers may observe and control
// cancellation and deadlines, and so implementations may attach request
// scoped values. Because the resolver is supplied by the caller, its error
// values and invocation counts remain observable to the caller.
type SchemaResolver interface {
	ResolveSchema(ctx context.Context, fingerprint uint64) (string, error)
}

// SchemaResolverFunc adapts a function to the SchemaResolver interface.
type SchemaResolverFunc func(ctx context.Context, fingerprint uint64) (string, error)

// ResolveSchema invokes the wrapped function.
func (f SchemaResolverFunc) ResolveSchema(ctx context.Context, fingerprint uint64) (string, error) {
	return f(ctx, fingerprint)
}

// ErrUnknownSchemaFingerprint is returned by a SchemaResolver when no schema
// is known for the requested fingerprint.
type ErrUnknownSchemaFingerprint uint64

func (e ErrUnknownSchemaFingerprint) Error() string {
	return "no schema known for fingerprint: " + strconv.FormatUint(uint64(e), 10)
}

// ErrSOETooShort is returned when a buffer does not contain enough bytes to
// hold a single-object encoding header. Its value is the number of bytes
// available.
type ErrSOETooShort int

func (e ErrSOETooShort) Error() string {
	return fmt.Sprintf("cannot decode single-object encoded buffer: have %d bytes; need at least %d", int(e), soeHeaderLen)
}

// ErrSOEBadMagic is returned when a buffer does not start with the recognized
// single-object encoding magic prefix. Its value is the offending prefix.
type ErrSOEBadMagic []byte

func (e ErrSOEBadMagic) Error() string {
	return fmt.Sprintf("cannot decode single-object encoded buffer: unknown magic prefix: %#x", []byte(e))
}

// ErrSOEFingerprintMismatch is returned when the schema provided by a
// SchemaResolver does not have the Rabin fingerprint read from the message.
// Decoding is aborted rather than trusting the mismatched schema.
type ErrSOEFingerprintMismatch struct {
	// Message is the fingerprint read from the message header.
	Message uint64
	// Schema is the fingerprint computed from the resolved schema.
	Schema uint64
}

func (e ErrSOEFingerprintMismatch) Error() string {
	return fmt.Sprintf("resolved schema fingerprint %d does not match message fingerprint %d", e.Schema, e.Message)
}

// InMemorySchemaResolver is a SchemaResolver backed by a fixed set of schemas
// held in memory. It never contacts a schema registry. The zero value is not
// usable; create one with NewInMemorySchemaResolver.
type InMemorySchemaResolver struct {
	schemas map[uint64]string
}

// NewInMemorySchemaResolver returns an InMemorySchemaResolver that resolves
// the Rabin fingerprints of the provided schemas to those schemas. It returns
// an error when any schema cannot be compiled.
func NewInMemorySchemaResolver(schemas ...string) (*InMemorySchemaResolver, error) {
	r := &InMemorySchemaResolver{schemas: make(map[uint64]string, len(schemas))}
	for _, schema := range schemas {
		if _, err := r.Add(schema); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Add compiles the provided schema and registers it under its Rabin
// fingerprint, returning that fingerprint.
func (r *InMemorySchemaResolver) Add(schema string) (uint64, error) {
	codec, err := NewCodec(schema)
	if err != nil {
		return 0, err
	}
	if r.schemas == nil {
		r.schemas = make(map[uint64]string)
	}
	r.schemas[codec.Rabin] = codec.Schema()
	return codec.Rabin, nil
}

// ResolveSchema returns the schema registered for the fingerprint, or
// ErrUnknownSchemaFingerprint when none is registered.
func (r *InMemorySchemaResolver) ResolveSchema(ctx context.Context, fingerprint uint64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	schema, ok := r.schemas[fingerprint]
	if !ok {
		return "", ErrUnknownSchemaFingerprint(fingerprint)
	}
	return schema, nil
}

// SOECache is a bounded cache from schema Rabin fingerprints to compiled
// Codec instances. It is created and owned by the caller, which chooses its
// capacity, and is safe for concurrent use. When the cache is full, inserting
// a new entry evicts the least recently used entry. Only successfully
// verified codecs are ever stored: unknown fingerprints are never cached as
// nonexistent, so a resolver that learns a schema later will be consulted
// again. A capacity of zero or less disables caching.
type SOECache struct {
	lock     sync.Mutex
	capacity int
	ll       *list.List // front is most recently used
	items    map[uint64]*list.Element
	hits     uint64
	misses   uint64
}

type soeCacheEntry struct {
	fingerprint uint64
	codec       *Codec
}

// NewSOECache returns a SOECache that holds at most capacity codecs. A
// capacity of zero or less disables caching entirely.
func NewSOECache(capacity int) *SOECache {
	return &SOECache{capacity: capacity, ll: list.New(), items: make(map[uint64]*list.Element)}
}

// Get returns the Codec cached for the fingerprint, moving it to the front of
// the eviction order.
func (c *SOECache) Get(fingerprint uint64) (*Codec, bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.capacity <= 0 {
		return nil, false
	}
	if el, ok := c.items[fingerprint]; ok {
		c.ll.MoveToFront(el)
		c.hits++
		return el.Value.(soeCacheEntry).codec, true
	}
	c.misses++
	return nil, false
}

// Put stores the Codec under the fingerprint, evicting the least recently
// used entry when the cache is at capacity.
func (c *SOECache) Put(fingerprint uint64, codec *Codec) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.capacity <= 0 {
		return
	}
	if el, ok := c.items[fingerprint]; ok {
		el.Value = soeCacheEntry{fingerprint: fingerprint, codec: codec}
		c.ll.MoveToFront(el)
		return
	}
	for c.ll.Len() >= c.capacity {
		back := c.ll.Back()
		delete(c.items, back.Value.(soeCacheEntry).fingerprint)
		c.ll.Remove(back)
	}
	c.items[fingerprint] = c.ll.PushFront(soeCacheEntry{fingerprint: fingerprint, codec: codec})
}

// Len returns the number of codecs currently cached.
func (c *SOECache) Len() int {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.ll.Len()
}

// Stats returns the number of cache hits and misses observed so far, allowing
// callers to observe how often the resolver was consulted.
func (c *SOECache) Stats() (hits, misses uint64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.hits, c.misses
}

// SOEDecoder decodes single-object encoded messages whose schemas are not
// known in advance. It reads the magic prefix and schema fingerprint from
// each message, obtains the schema from a caller supplied SchemaResolver,
// verifies that the resolved schema actually has the message fingerprint, and
// only then decodes the payload. The resolver is consulted at most once per
// message, and verified codecs are stored in the caller owned SOECache so
// subsequent messages with the same fingerprint do not trigger another
// resolution.
type SOEDecoder struct {
	resolver SchemaResolver
	cache    *SOECache
}

// NewSOEDecoder returns a SOEDecoder that obtains schemas from resolver and
// stores verified codecs in cache. The cache may be nil, in which case every
// message triggers exactly one resolver call.
func NewSOEDecoder(resolver SchemaResolver, cache *SOECache) *SOEDecoder {
	return &SOEDecoder{resolver: resolver, cache: cache}
}

// NativeFromSingle decodes one single-object encoded datum from buf, using
// the resolver to fetch the schema identified by the message fingerprint. On
// success it returns the decoded datum and the unconsumed remainder of buf.
// On any failure it returns the original buf unmodified, with a distinct
// error for each failure class: ErrSOETooShort when the header is truncated,
// ErrSOEBadMagic when the magic prefix is wrong, the resolver's own error
// when the schema cannot be resolved, ErrSOEFingerprintMismatch when the
// resolved schema does not match the message fingerprint, or the codec's
// decode error when the payload is malformed.
func (d *SOEDecoder) NativeFromSingle(ctx context.Context, buf []byte) (interface{}, []byte, error) {
	if len(buf) < soeHeaderLen {
		return nil, buf, ErrSOETooShort(len(buf))
	}
	if buf[0] != 0xC3 || buf[1] != 0x01 {
		return nil, buf, ErrSOEBadMagic(buf[:soeMagicPrefix])
	}
	fingerprint := binary.LittleEndian.Uint64(buf[soeMagicPrefix:soeHeaderLen])
	codec, err := d.codecForFingerprint(ctx, fingerprint)
	if err != nil {
		return nil, buf, err
	}
	value, newBuf, err := codec.NativeFromBinary(buf[soeHeaderLen:])
	if err != nil {
		return nil, buf, err // if error, return original byte slice
	}
	return value, newBuf, nil
}

// codecForFingerprint returns a verified Codec for the fingerprint, asking
// the resolver at most once per call. Resolution failures and fingerprint
// mismatches are never cached.
func (d *SOEDecoder) codecForFingerprint(ctx context.Context, fingerprint uint64) (*Codec, error) {
	if d.cache != nil {
		if codec, ok := d.cache.Get(fingerprint); ok {
			return codec, nil
		}
	}
	schema, err := d.resolver.ResolveSchema(ctx, fingerprint)
	if err != nil {
		return nil, err // miss is not cached; resolver will be consulted again
	}
	codec, err := NewCodec(schema)
	if err != nil {
		return nil, fmt.Errorf("cannot compile schema resolved for fingerprint %d: %v", fingerprint, err)
	}
	if codec.Rabin != fingerprint {
		return nil, ErrSOEFingerprintMismatch{Message: fingerprint, Schema: codec.Rabin}
	}
	if d.cache != nil {
		d.cache.Put(fingerprint, codec)
	}
	return codec, nil
}
