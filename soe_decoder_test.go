package goavro

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

const (
	soeTestSchemaUnion = `{"type":"record","name":"Envelope","fields":[{"name":"body","type":["null","string"]}]}`
	soeTestSchemaLong  = `{"type":"record","name":"Counter","fields":[{"name":"n","type":"long"}]}`
)

func newSOETestDecoder(t *testing.T, resolver SchemaResolver, capacity int) *SOEDecoder {
	t.Helper()
	cache, err := NewCodecCache(capacity)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	decoder, err := NewSOEDecoder(resolver, cache)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	return decoder
}

func encodeSOETestMessage(t *testing.T, schema string, datum interface{}) []byte {
	t.Helper()
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	buf, err := codec.SingleFromNative(nil, datum)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	return buf
}

func soeTestDatumEqual(a, b interface{}) bool {
	for _, schema := range []string{soeTestSchemaUnion, soeTestSchemaLong} {
		codec, err := NewCodec(schema)
		if err != nil {
			continue
		}
		ab, errA := codec.BinaryFromNative(nil, a)
		bb, errB := codec.BinaryFromNative(nil, b)
		if errA == nil && errB == nil {
			return bytes.Equal(ab, bb)
		}
	}
	return false
}

func TestSOEDecoderInterleavedMessagesWithTrailingBytes(t *testing.T) {
	resolver, err := NewMemoryResolver(soeTestSchemaUnion, soeTestSchemaLong)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	decoder := newSOETestDecoder(t, resolver, 8)

	unionDatum := map[string]interface{}{"body": Union("string", "hello")}
	longDatum := map[string]interface{}{"n": int64(42)}

	msg1 := encodeSOETestMessage(t, soeTestSchemaUnion, unionDatum)
	msg2 := encodeSOETestMessage(t, soeTestSchemaLong, longDatum)
	msg3 := encodeSOETestMessage(t, soeTestSchemaUnion, map[string]interface{}{"body": nil})
	tail := []byte{0xde, 0xad, 0xbe, 0xef}

	stream := append(append(append(append([]byte{}, msg1...), msg2...), msg3...), tail...)

	datum1, rest, err := decoder.NativeFromSingle(context.Background(), stream)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if !soeTestDatumEqual(datum1, unionDatum) {
		t.Errorf("GOT: %#v; WANT: %#v", datum1, unionDatum)
	}
	if want := len(msg2) + len(msg3) + len(tail); len(rest) != want {
		t.Fatalf("GOT: %d remaining bytes; WANT: %d", len(rest), want)
	}

	datum2, rest, err := decoder.NativeFromSingle(context.Background(), rest)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if !soeTestDatumEqual(datum2, longDatum) {
		t.Errorf("GOT: %#v; WANT: %#v", datum2, longDatum)
	}
	if want := len(msg3) + len(tail); len(rest) != want {
		t.Fatalf("GOT: %d remaining bytes; WANT: %d", len(rest), want)
	}

	_, rest, err = decoder.NativeFromSingle(context.Background(), rest)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if !bytes.Equal(rest, tail) {
		t.Errorf("GOT: remaining %#v; WANT: %#v", rest, tail)
	}

	// Each schema resolved exactly once despite interleaving.
	if got, want := resolver.Calls(), 2; got != want {
		t.Errorf("GOT: %d resolver calls; WANT: %d", got, want)
	}
}

func TestSOEDecoderResolvesUnionMessageExactlyOnce(t *testing.T) {
	calls := 0
	resolver := SchemaResolverFunc(func(_ context.Context, fingerprint uint64) (string, error) {
		calls++
		return soeTestSchemaUnion, nil
	})
	decoder := newSOETestDecoder(t, resolver, 8)

	// A union payload whose first branch does not match forces the codec to
	// attempt subsequent branches; this must not re-resolve the schema.
	msg := encodeSOETestMessage(t, soeTestSchemaUnion, map[string]interface{}{"body": Union("string", "x")})
	if _, _, err := decoder.NativeFromSingle(context.Background(), msg); err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if got, want := calls, 1; got != want {
		t.Errorf("GOT: %d resolver calls; WANT: %d", got, want)
	}
}

func TestSOEDecoderUnknownFingerprintNotCached(t *testing.T) {
	resolver, err := NewMemoryResolver(soeTestSchemaLong)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	cache, err := NewCodecCache(8)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	decoder, err := NewSOEDecoder(resolver, cache)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}

	// Message encoded with a schema the resolver does not know.
	msg := encodeSOETestMessage(t, soeTestSchemaUnion, map[string]interface{}{"body": nil})

	for i := 0; i < 2; i++ {
		_, _, err := decoder.NativeFromSingle(context.Background(), msg)
		var resolutionErr ErrSchemaResolution
		if !errors.As(err, &resolutionErr) {
			t.Fatalf("GOT: %T %v; WANT: ErrSchemaResolution", err, err)
		}
		var unknown ErrUnknownSchemaFingerprint
		if !errors.As(err, &unknown) {
			t.Errorf("GOT: %v; WANT: wrapped ErrUnknownSchemaFingerprint", err)
		}
	}
	// Unknown fingerprint must not be cached as absent: every attempt asks
	// the resolver again, and nothing is stored.
	if got, want := resolver.Calls(), 2; got != want {
		t.Errorf("GOT: %d resolver calls; WANT: %d", got, want)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("GOT: %d cached codecs; WANT: 0", got)
	}
}

func TestSOEDecoderResolverReturnsWrongSchema(t *testing.T) {
	// Resolver answers with a schema whose fingerprint does not match the
	// message fingerprint; decoding must not proceed.
	resolver := SchemaResolverFunc(func(_ context.Context, fingerprint uint64) (string, error) {
		return soeTestSchemaLong, nil
	})
	cache, err := NewCodecCache(8)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	decoder, err := NewSOEDecoder(resolver, cache)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}

	msg := encodeSOETestMessage(t, soeTestSchemaUnion, map[string]interface{}{"body": Union("string", "x")})
	_, _, err = decoder.NativeFromSingle(context.Background(), msg)
	var mismatch ErrSchemaFingerprintMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("GOT: %T %v; WANT: ErrSchemaFingerprintMismatch", err, err)
	}
	if got := cache.Len(); got != 0 {
		t.Errorf("GOT: %d cached codecs; WANT: 0", got)
	}
}

func TestSOEDecoderDistinctDiagnostics(t *testing.T) {
	resolver, err := NewMemoryResolver(soeTestSchemaLong)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	decoder := newSOETestDecoder(t, resolver, 8)
	ctx := context.Background()

	// Buffer shorter than the SOE header.
	_, _, err = decoder.NativeFromSingle(ctx, []byte{0xC3, 0x01, 0x00})
	var shortBuffer ErrSOEShortBuffer
	if !errors.As(err, &shortBuffer) {
		t.Errorf("GOT: %T %v; WANT: ErrSOEShortBuffer", err, err)
	}

	// Full-length buffer with wrong magic bytes.
	badMagic := encodeSOETestMessage(t, soeTestSchemaLong, map[string]interface{}{"n": int64(1)})
	badMagic[0] = 0x00
	_, _, err = decoder.NativeFromSingle(ctx, badMagic)
	var magic ErrSOEBadMagic
	if !errors.As(err, &magic) {
		t.Errorf("GOT: %T %v; WANT: ErrSOEBadMagic", err, err)
	}

	// Valid header, but payload is truncated.
	msg := encodeSOETestMessage(t, soeTestSchemaLong, map[string]interface{}{"n": int64(1)})
	_, _, err = decoder.NativeFromSingle(ctx, msg[:len(msg)-1])
	var payload ErrSOEPayload
	if !errors.As(err, &payload) {
		t.Errorf("GOT: %T %v; WANT: ErrSOEPayload", err, err)
	}
}

func TestSOEDecoderResolverContextObservable(t *testing.T) {
	type ctxKey string
	const key ctxKey = "request-id"

	var seen interface{}
	resolver := SchemaResolverFunc(func(ctx context.Context, fingerprint uint64) (string, error) {
		seen = ctx.Value(key)
		return soeTestSchemaLong, nil
	})
	decoder := newSOETestDecoder(t, resolver, 8)

	msg := encodeSOETestMessage(t, soeTestSchemaLong, map[string]interface{}{"n": int64(7)})
	ctx := context.WithValue(context.Background(), key, "abc-123")
	if _, _, err := decoder.NativeFromSingle(ctx, msg); err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if got, want := seen, "abc-123"; got != want {
		t.Errorf("GOT: %v; WANT: %v", got, want)
	}
}

func TestSOEDecoderCacheCapacityEvicts(t *testing.T) {
	resolver, err := NewMemoryResolver(soeTestSchemaUnion, soeTestSchemaLong)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	decoder := newSOETestDecoder(t, resolver, 1)

	msg1 := encodeSOETestMessage(t, soeTestSchemaUnion, map[string]interface{}{"body": nil})
	msg2 := encodeSOETestMessage(t, soeTestSchemaLong, map[string]interface{}{"n": int64(1)})

	// Capacity of 1 forces eviction on every schema switch.
	for i := 0; i < 2; i++ {
		if _, _, err := decoder.NativeFromSingle(context.Background(), msg1); err != nil {
			t.Fatalf("GOT: %v; WANT: nil", err)
		}
		if _, _, err := decoder.NativeFromSingle(context.Background(), msg2); err != nil {
			t.Fatalf("GOT: %v; WANT: nil", err)
		}
	}
	if got, want := resolver.Calls(), 4; got != want {
		t.Errorf("GOT: %d resolver calls; WANT: %d", got, want)
	}
}

func TestSOEDecoderCacheHitSkipsResolver(t *testing.T) {
	resolver, err := NewMemoryResolver(soeTestSchemaLong)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	decoder := newSOETestDecoder(t, resolver, 8)

	msg := encodeSOETestMessage(t, soeTestSchemaLong, map[string]interface{}{"n": int64(3)})
	for i := 0; i < 3; i++ {
		if _, _, err := decoder.NativeFromSingle(context.Background(), msg); err != nil {
			t.Fatalf("GOT: %v; WANT: nil", err)
		}
	}
	if got, want := resolver.Calls(), 1; got != want {
		t.Errorf("GOT: %d resolver calls; WANT: %d", got, want)
	}
}

func TestSOEDecoderRequiresResolverAndCache(t *testing.T) {
	cache, err := NewCodecCache(1)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if _, err := NewSOEDecoder(nil, cache); err == nil {
		t.Errorf("GOT: nil; WANT: error")
	}
	resolver, err := NewMemoryResolver(soeTestSchemaLong)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if _, err := NewSOEDecoder(resolver, nil); err == nil {
		t.Errorf("GOT: nil; WANT: error")
	}
	if _, err := NewCodecCache(0); err == nil {
		t.Errorf("GOT: nil; WANT: error")
	}
}

func TestCodecNativeFromSingleStillWorks(t *testing.T) {
	codec, err := NewCodec(soeTestSchemaUnion)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	datum := map[string]interface{}{"body": Union("string", "hi")}
	buf, err := codec.SingleFromNative(nil, datum)
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	tail := []byte{0x01, 0x02}
	got, rest, err := codec.NativeFromSingle(append(buf, tail...))
	if err != nil {
		t.Fatalf("GOT: %v; WANT: nil", err)
	}
	if !bytes.Equal(rest, tail) {
		t.Errorf("GOT: remaining %#v; WANT: %#v", rest, tail)
	}
	if !soeTestDatumEqual(got, datum) {
		t.Errorf("GOT: %#v; WANT: %#v", got, datum)
	}
}
