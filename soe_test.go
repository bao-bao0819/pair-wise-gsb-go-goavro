package goavro

import (
	"bytes"
	"context"
	"testing"
)

const (
	soeTestIntSchema  = `"int"`
	soeTestUserSchema = `{
  "type": "record",
  "name": "User",
  "fields": [
    {"name": "name", "type": "string"},
    {"name": "age", "type": ["null", "int"], "default": null}
  ]
}`
)

// countingResolver wraps a SchemaResolver and records every call so tests can
// observe the resolver's context, errors, and invocation count.
type countingResolver struct {
	next      SchemaResolver
	calls     int
	lastCtx   context.Context
	lastError error
	override  func(fingerprint uint64) (string, error)
}

func (r *countingResolver) ResolveSchema(ctx context.Context, fingerprint uint64) (string, error) {
	r.calls++
	r.lastCtx = ctx
	var schema string
	var err error
	if r.override != nil {
		schema, err = r.override(fingerprint)
	} else {
		schema, err = r.next.ResolveSchema(ctx, fingerprint)
	}
	r.lastError = err
	return schema, err
}

func TestSOEDecoderInterleavedMessages(t *testing.T) {
	intCodec, err := NewCodec(soeTestIntSchema)
	ensureError(t, err)
	userCodec, err := NewCodec(soeTestUserSchema)
	ensureError(t, err)

	resolver, err := NewInMemorySchemaResolver(soeTestIntSchema, soeTestUserSchema)
	ensureError(t, err)
	counting := &countingResolver{next: resolver}
	cache := NewSOECache(8)
	decoder := NewSOEDecoder(counting, cache)

	userDatum := map[string]interface{}{
		"name": "alice",
		"age":  map[string]interface{}{"int": int32(42)},
	}

	// Interleave messages from both schemas, each followed by arbitrary
	// trailing bytes that must be reported as unconsumed.
	intMsg, err := intCodec.SingleFromNative(nil, 3)
	ensureError(t, err)
	userMsg, err := userCodec.SingleFromNative(nil, userDatum)
	ensureError(t, err)
	intMsg2, err := intCodec.SingleFromNative(nil, -7)
	ensureError(t, err)

	stream := append(append(append(intMsg, userMsg...), intMsg2...), 0xDE, 0xAD, 0xBE, 0xEF)

	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("request"), "observable")

	datum, rest, err := decoder.NativeFromSingle(ctx, stream)
	ensureError(t, err)
	if got, want := datum, int32(3); got != want {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}
	if got, want := len(rest), len(stream)-len(intMsg); got != want {
		t.Fatalf("consumed length GOT: %d; WANT: %d", len(stream)-got, want)
	}

	datum, rest, err = decoder.NativeFromSingle(ctx, rest)
	ensureError(t, err)
	m, ok := datum.(map[string]interface{})
	if !ok {
		t.Fatalf("GOT: %T; WANT: map[string]interface{}", datum)
	}
	if got, want := m["name"], "alice"; got != want {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}
	if got, want := m["age"], map[string]interface{}{"int": int32(42)}; !mapsEqual(got.(map[string]interface{}), want) {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}

	datum, rest, err = decoder.NativeFromSingle(ctx, rest)
	ensureError(t, err)
	if got, want := datum, int32(-7); got != want {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}

	// The arbitrary trailing bytes must remain untouched.
	if got, want := rest, []byte{0xDE, 0xAD, 0xBE, 0xEF}; !bytes.Equal(got, want) {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}

	// One resolver call per distinct fingerprint, even though the user schema
	// contains a union: a single message never triggers repeated resolution.
	if got, want := counting.calls, 2; got != want {
		t.Errorf("resolver calls GOT: %d; WANT: %d", got, want)
	}
	if got, want := counting.lastCtx.Value(ctxKey("request")), "observable"; got != want {
		t.Errorf("resolver context GOT: %v; WANT: %v", got, want)
	}
	hits, misses := cache.Stats()
	if got, want := hits, uint64(1); got != want {
		t.Errorf("cache hits GOT: %d; WANT: %d", got, want)
	}
	if got, want := misses, uint64(2); got != want {
		t.Errorf("cache misses GOT: %d; WANT: %d", got, want)
	}
}

func mapsEqual(a, b map[string]interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestSOEDecoderDistinctDiagnostics(t *testing.T) {
	resolver, err := NewInMemorySchemaResolver(soeTestIntSchema)
	ensureError(t, err)
	decoder := NewSOEDecoder(resolver, NewSOECache(4))
	ctx := context.Background()

	t.Run("short buffer", func(t *testing.T) {
		buf := []byte{0xC3, 0x01, 0x8F}
		_, rest, err := decoder.NativeFromSingle(ctx, buf)
		if _, ok := err.(ErrSOETooShort); !ok {
			t.Fatalf("GOT: %T %v; WANT: ErrSOETooShort", err, err)
		}
		if !bytes.Equal(rest, buf) {
			t.Errorf("buffer modified on error")
		}
	})

	t.Run("bad magic", func(t *testing.T) {
		buf := []byte{0x00, 0x01, 0x8F, 0x5C, 0x39, 0x3F, 0x1A, 0xD5, 0x75, 0x72, 0x06}
		_, _, err := decoder.NativeFromSingle(ctx, buf)
		if _, ok := err.(ErrSOEBadMagic); !ok {
			t.Fatalf("GOT: %T %v; WANT: ErrSOEBadMagic", err, err)
		}
	})

	t.Run("fingerprint mismatch", func(t *testing.T) {
		intCodec, err := NewCodec(soeTestIntSchema)
		ensureError(t, err)
		buf, err := intCodec.SingleFromNative(nil, 3)
		ensureError(t, err)

		// Resolver returns a schema that does not match the message
		// fingerprint: decoding must not proceed.
		lying := &countingResolver{override: func(uint64) (string, error) {
			return `"string"`, nil
		}}
		d := NewSOEDecoder(lying, NewSOECache(4))
		_, rest, err := d.NativeFromSingle(ctx, buf)
		mismatch, ok := err.(ErrSOEFingerprintMismatch)
		if !ok {
			t.Fatalf("GOT: %T %v; WANT: ErrSOEFingerprintMismatch", err, err)
		}
		if mismatch.Message != uint64(intCodec.Rabin) {
			t.Errorf("GOT: %d; WANT: %d", mismatch.Message, intCodec.Rabin)
		}
		if !bytes.Equal(rest, buf) {
			t.Errorf("buffer modified on error")
		}
	})

	t.Run("malformed payload", func(t *testing.T) {
		userCodec, err := NewCodec(soeTestUserSchema)
		ensureError(t, err)
		resolver, err := NewInMemorySchemaResolver(soeTestUserSchema)
		ensureError(t, err)
		d := NewSOEDecoder(resolver, NewSOECache(4))

		// Valid header, but the payload is truncated: a long string length
		// with no string bytes following it.
		buf, err := userCodec.SingleFromNative(nil, map[string]interface{}{"name": "alice"})
		ensureError(t, err)
		buf = buf[:len(buf)-3]

		_, _, err = d.NativeFromSingle(ctx, buf)
		if err == nil {
			t.Fatalf("GOT: nil; WANT: payload decode error")
		}
		switch err.(type) {
		case ErrSOETooShort, ErrSOEBadMagic, ErrSOEFingerprintMismatch, ErrUnknownSchemaFingerprint:
			t.Fatalf("payload error misdiagnosed as header error: %T %v", err, err)
		}
		ensureError(t, err, "cannot decode binary")
	})
}

func TestSOEDecoderUnknownFingerprintNotCached(t *testing.T) {
	intCodec, err := NewCodec(soeTestIntSchema)
	ensureError(t, err)
	buf, err := intCodec.SingleFromNative(nil, 3)
	ensureError(t, err)

	resolver, err := NewInMemorySchemaResolver() // knows nothing yet
	ensureError(t, err)
	counting := &countingResolver{next: resolver}
	decoder := NewSOEDecoder(counting, NewSOECache(4))
	ctx := context.Background()

	_, _, err = decoder.NativeFromSingle(ctx, buf)
	if _, ok := err.(ErrUnknownSchemaFingerprint); !ok {
		t.Fatalf("GOT: %T %v; WANT: ErrUnknownSchemaFingerprint", err, err)
	}
	if got, want := counting.lastError, err; got != want {
		t.Errorf("resolver error GOT: %v; WANT: %v", got, want)
	}

	// The unknown fingerprint must not be cached as nonexistent: once the
	// resolver learns the schema, the very next message decodes.
	_, err = resolver.Add(soeTestIntSchema)
	ensureError(t, err)

	datum, _, err := decoder.NativeFromSingle(ctx, buf)
	ensureError(t, err)
	if got, want := datum, int32(3); got != want {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}
	if got, want := counting.calls, 2; got != want {
		t.Errorf("resolver calls GOT: %d; WANT: %d", got, want)
	}
}

func TestSOEDecoderCacheCapacity(t *testing.T) {
	intCodec, err := NewCodec(soeTestIntSchema)
	ensureError(t, err)
	userCodec, err := NewCodec(soeTestUserSchema)
	ensureError(t, err)

	resolver, err := NewInMemorySchemaResolver(soeTestIntSchema, soeTestUserSchema)
	ensureError(t, err)
	counting := &countingResolver{next: resolver}
	decoder := NewSOEDecoder(counting, NewSOECache(1)) // holds only one codec
	ctx := context.Background()

	intMsg, err := intCodec.SingleFromNative(nil, 1)
	ensureError(t, err)
	userMsg, err := userCodec.SingleFromNative(nil, map[string]interface{}{"name": "bob"})
	ensureError(t, err)

	// Alternating fingerprints with capacity 1 evicts on every message, so
	// the resolver is consulted once per message.
	for i := 0; i < 3; i++ {
		if _, _, err = decoder.NativeFromSingle(ctx, intMsg); err != nil {
			t.Fatal(err)
		}
		if _, _, err = decoder.NativeFromSingle(ctx, userMsg); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := counting.calls, 6; got != want {
		t.Errorf("resolver calls GOT: %d; WANT: %d", got, want)
	}
	if got, want := decoder.cache.Len(), 1; got != want {
		t.Errorf("cache length GOT: %d; WANT: %d", got, want)
	}
}

func TestSOEDecoderResolverErrorPropagates(t *testing.T) {
	intCodec, err := NewCodec(soeTestIntSchema)
	ensureError(t, err)
	buf, err := intCodec.SingleFromNative(nil, 3)
	ensureError(t, err)

	counting := &countingResolver{override: func(fingerprint uint64) (string, error) {
		return "", ErrUnknownSchemaFingerprint(fingerprint)
	}}
	decoder := NewSOEDecoder(counting, nil) // no cache at all

	_, rest, err := decoder.NativeFromSingle(context.Background(), buf)
	if _, ok := err.(ErrUnknownSchemaFingerprint); !ok {
		t.Fatalf("GOT: %T %v; WANT: ErrUnknownSchemaFingerprint", err, err)
	}
	if !bytes.Equal(rest, buf) {
		t.Errorf("buffer modified on error")
	}
	if got, want := counting.calls, 1; got != want {
		t.Errorf("resolver calls GOT: %d; WANT: %d", got, want)
	}
}

func TestSOEDecoderHonorsExistingCodecAPI(t *testing.T) {
	// Callers that already hold a Codec keep using the pre-existing API.
	codec, err := NewCodec(soeTestIntSchema)
	ensureError(t, err)

	buf, err := codec.SingleFromNative(nil, 3)
	ensureError(t, err)
	buf = append(buf, 0xDE, 0xAD)

	datum, rest, err := codec.NativeFromSingle(buf)
	ensureError(t, err)
	if got, want := datum, int32(3); got != want {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}
	if got, want := rest, []byte{0xDE, 0xAD}; !bytes.Equal(got, want) {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}

	fingerprint, _, err := FingerprintFromSOE(buf)
	ensureError(t, err)
	if got, want := fingerprint, uint64(codec.Rabin); got != want {
		t.Errorf("GOT: %d; WANT: %d", got, want)
	}
}
