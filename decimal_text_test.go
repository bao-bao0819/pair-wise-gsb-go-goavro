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
	"math/big"
	"strconv"
	"strings"
	"testing"
)

func newConverterOrFail(t *testing.T, schema string) *DecimalConverter {
	t.Helper()
	converter, err := NewDecimalConverter(schema)
	if err != nil {
		t.Fatalf("NewDecimalConverter(%s): %s", schema, err)
	}
	return converter
}

func TestNewDecimalConverterAccessors(t *testing.T) {
	bytesConverter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":10,"scale":2}`)
	if bytesConverter.Precision() != 10 || bytesConverter.Scale() != 2 {
		t.Errorf("bytes converter precision/scale: got %d/%d, want 10/2",
			bytesConverter.Precision(), bytesConverter.Scale())
	}
	if bytesConverter.IsFixed() || bytesConverter.FixedSize() != 0 {
		t.Errorf("bytes converter reported fixed: %v (%d)", bytesConverter.IsFixed(), bytesConverter.FixedSize())
	}

	fixedConverter := newConverterOrFail(t,
		`{"type":"fixed","size":8,"logicalType":"decimal","precision":18,"scale":4}`)
	if !fixedConverter.IsFixed() || fixedConverter.FixedSize() != 8 {
		t.Errorf("fixed converter reported not fixed or wrong size: %v (%d)",
			fixedConverter.IsFixed(), fixedConverter.FixedSize())
	}

	// precision 1 scale 0 only needs a single byte
	singleByte := newConverterOrFail(t,
		`{"type":"fixed","size":1,"logicalType":"decimal","precision":1,"scale":0}`)
	if singleByte.FixedSize() != 1 {
		t.Errorf("single byte fixed size: got %d, want 1", singleByte.FixedSize())
	}
}

func TestNewDecimalConverterInvalidSchemas(t *testing.T) {
	testCases := []struct {
		name   string
		schema string
		err    string
	}{
		{"empty", "", "cannot parse decimal schema"},
		{"not json", "{not json", "cannot parse decimal schema"},
		{"non object", `[1,2,3]`, "non-object schema"},
		{"missing logical type", `{"type":"bytes","precision":4}`, `missing logicalType "decimal"`},
		{"unknown logical type", `{"type":"bytes","logicalType":"date","precision":4}`, `unsupported logicalType "date"`},
		{"uuid logical type", `{"type":"string","logicalType":"uuid"}`, `unsupported logicalType "uuid"`},
		{"wrong logical type type", `{"type":"bytes","logicalType":123,"precision":4}`, "logicalType must be a JSON string"},
		{"missing precision", `{"type":"bytes","logicalType":"decimal"}`, "without precision"},
		{"zero precision", `{"type":"bytes","logicalType":"decimal","precision":0}`, "precision is less than one"},
		{"negative scale", `{"type":"bytes","logicalType":"decimal","precision":4,"scale":-1}`, "scale is less than zero"},
		{"scale above precision", `{"type":"bytes","logicalType":"decimal","precision":4,"scale":5}`, "scale is larger than precision"},
		{"string backing type", `{"type":"string","logicalType":"decimal","precision":4}`, `backing type "string"`},
		{"int backing type", `{"type":"int","logicalType":"decimal","precision":4}`, `backing type "int"`},
		{"fixed missing size", `{"type":"fixed","logicalType":"decimal","precision":4}`, "ought to have size key"},
		{"fixed zero size", `{"type":"fixed","size":0,"logicalType":"decimal","precision":4}`, "greater than zero"},
		{"fixed too narrow", `{"type":"fixed","size":1,"logicalType":"decimal","precision":4}`, "too small for precision 4"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDecimalConverter(tc.schema)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.err)
			}
			if !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.err)
			}
		})
	}
}

func TestDecimalEncodeBytesKnownSamples(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":4,"scale":2}`)

	testCases := []struct {
		text     string
		expected []byte
	}{
		{"0", []byte{0x00}},
		{"-0", []byte{0x00}},
		{"0.00", []byte{0x00}},
		{"-0.00", []byte{0x00}},
		{"12.34", []byte{0x04, 0xd2}},
		{"-12.34", []byte{0xfb, 0x2e}},
		{"99.99", []byte{0x27, 0x0f}},
		{"-99.99", []byte{0xd8, 0xf1}},
		// top bit of the leading byte set forces a 0x00 pad for positives
		{"12.80", []byte{0x05, 0x00}},
		{"-12.80", []byte{0xfb, 0x00}},
		// 128 is 0x80: the sign extension byte is required
		{"1.28", []byte{0x00, 0x80}},
		{"-1.28", []byte{0x80}},
		// scale larger than the input's number of fractional digits: exact zero pad
		{"12.3", []byte{0x04, 0xce}},
		{"12", []byte{0x04, 0xb0}},
		// a trailing fractional zero does not require rounding
		{"12.340", []byte{0x04, 0xd2}},
		{"+12.34", []byte{0x04, 0xd2}},
	}

	for _, tc := range testCases {
		got, err := converter.Encode(tc.text)
		if err != nil {
			t.Errorf("Encode(%q): %s", tc.text, err)
			continue
		}
		if !bytesEqual(got, tc.expected) {
			t.Errorf("Encode(%q) = % x, want % x", tc.text, got, tc.expected)
		}
	}
}

func TestDecimalEncodeFixedKnownSamples(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"fixed","size":12,"logicalType":"decimal","precision":4,"scale":2}`)

	testCases := []struct {
		text     string
		expected []byte
	}{
		{"0", make([]byte, 12)},
		{"12.34", append(make([]byte, 10), 0x04, 0xd2)},
		{"-12.34", bytesRepeat(0xff, 12)},
	}
	neg := testCases[2].expected
	copy(neg[10:], []byte{0xfb, 0x2e})

	for _, tc := range testCases {
		got, err := converter.Encode(tc.text)
		if err != nil {
			t.Errorf("Encode(%q): %s", tc.text, err)
			continue
		}
		if !bytesEqual(got, tc.expected) {
			t.Errorf("Encode(%q) = % x, want % x", tc.text, got, tc.expected)
		}
		if len(got) != 12 {
			t.Errorf("Encode(%q) length = %d, want 12", tc.text, len(got))
		}
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDecimalDecodeKnownSamples(t *testing.T) {
	bytesConverter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":4,"scale":2}`)

	testCases := []struct {
		data     []byte
		expected string
	}{
		{[]byte{0x00}, "0.00"},
		{[]byte{0x04, 0xd2}, "12.34"},
		{[]byte{0xfb, 0x2e}, "-12.34"},
		{[]byte{0x27, 0x0f}, "99.99"},
		{[]byte{0xd8, 0xf1}, "-99.99"},
		// redundant sign extension / leading zero is legal for bytes decimals
		{[]byte{0x00, 0x04, 0xd2}, "12.34"},
		{[]byte{0xff, 0xfb, 0x2e}, "-12.34"},
	}

	for _, tc := range testCases {
		got, err := bytesConverter.Decode(tc.data)
		if err != nil {
			t.Errorf("Decode(% x): %s", tc.data, err)
			continue
		}
		if got != tc.expected {
			t.Errorf("Decode(% x) = %q, want %q", tc.data, got, tc.expected)
		}
	}

	integerConverter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":4,"scale":0}`)
	got, err := integerConverter.Decode([]byte{0x0c})
	if err != nil {
		t.Fatalf("Decode scale 0: %s", err)
	}
	if got != "12" {
		t.Errorf("Decode scale 0 = %q, want %q (no decimal point)", got, "12")
	}
}

func TestDecimalDecodeFixedLengthEnforced(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"fixed","size":12,"logicalType":"decimal","precision":4,"scale":2}`)

	data := bytesRepeat(0x00, 12)
	data[11] = 0xd2
	data[10] = 0x04
	if got, err := converter.Decode(data); err != nil || got != "12.34" {
		t.Errorf("Decode fixed = %q, %v; want 12.34", got, err)
	}

	if _, err := converter.Decode(data[:11]); err == nil {
		t.Error("expected error decoding short fixed datum, got nil")
	}
	if _, err := converter.Decode(append(data, 0x00)); err == nil {
		t.Error("expected error decoding long fixed datum, got nil")
	}

	bytesConverter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":4,"scale":2}`)
	if _, err := bytesConverter.Decode(nil); err == nil {
		t.Error("expected error decoding empty bytes datum, got nil")
	}
}

func TestDecimalEncodePrecisionOverflow(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":4,"scale":2}`)

	for _, text := range []string{"100.00", "-100.00", "999.99", "-999.99"} {
		if _, err := converter.Encode(text); err == nil {
			t.Errorf("Encode(%q): expected precision error, got nil", text)
		}
	}
}

func TestDecimalDecodePrecisionOverflow(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":4,"scale":2}`)

	// 10000 and -10000 are five-digit unscaled values
	for _, data := range [][]byte{{0x27, 0x10}, {0xd8, 0xf0}} {
		if _, err := converter.Decode(data); err == nil {
			t.Errorf("Decode(% x): expected precision error, got nil", data)
		}
	}
}

func TestDecimalEncodeRoundingRequired(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":10,"scale":2}`)

	for _, text := range []string{"12.345", "-12.345", "0.001", "1.005"} {
		if _, err := converter.Encode(text); err == nil {
			t.Errorf("Encode(%q): expected rounding error, got nil", text)
		} else if !strings.Contains(err.Error(), "rounding would be required") {
			t.Errorf("Encode(%q): error %q does not mention rounding", text, err)
		}
	}
}

func TestDecimalEncodeInvalidText(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":10,"scale":2}`)

	for _, text := range []string{
		"", " ", ".5", "1.", "1e2", "12.3.4", "0x10", " 12.34", "12.34 ",
		"1/2", "NaN", "inf", "--1", "++1", "+", "-",
	} {
		if _, err := converter.Encode(text); err == nil {
			t.Errorf("Encode(%q): expected parse error, got nil", text)
		}
	}
}

// TestDecimalSmallIntegerOracle cross-checks Encode/Decode against an
// independent reference built from a byte-level two's complement algorithm and
// math/big.Rat.FloatString, spanning positive values, negative values and the
// highest-bit-set boundaries where sign extension matters.
func TestDecimalSmallIntegerOracle(t *testing.T) {
	const precision = 5

	for scale := 0; scale <= 3; scale++ {
		schema := `{"type":"bytes","logicalType":"decimal","precision":5,"scale":` +
			strconv.Itoa(scale) + `}`
		converter := newConverterOrFail(t, schema)
		denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)

		for n := -12000; n <= 12000; n += 37 {
			unscaled := big.NewInt(int64(n))
			rat := new(big.Rat).SetFrac(unscaled, denom)
			input := rat.FloatString(scale)

			encoded, err := converter.Encode(input)
			if err != nil {
				t.Fatalf("Encode(%q) scale %d: %s", input, scale, err)
			}
			wantEncoded := oracleMinimalTwosComplement(n)
			if !bytesEqual(encoded, wantEncoded) {
				t.Fatalf("Encode(%q) scale %d = % x, oracle % x",
					input, scale, encoded, wantEncoded)
			}

			decoded, err := converter.Decode(encoded)
			if err != nil {
				t.Fatalf("Decode(% x) scale %d: %s", encoded, scale, err)
			}
			if decoded != input {
				t.Fatalf("round trip scale %d: got %q, want %q", scale, decoded, input)
			}
		}
	}
}

// TestDecimalFixedBoundaryOracle verifies every byte-width boundary from one
// byte through three bytes for both positive and negative values.
func TestDecimalFixedBoundaryOracle(t *testing.T) {
	boundaries := []int{
		-32769, -32768, -129, -128, -1, 0, 1, 127, 128, 129, 255, 256,
		32767, 32768,
	}

	for _, n := range boundaries {
		minimal := oracleMinimalTwosComplement(n)
		got := minimalSignedBytes(big.NewInt(int64(n)))
		if !bytesEqual(got, minimal) {
			t.Errorf("minimalSignedBytes(%d) = % x, oracle % x", n, got, minimal)
		}

		for size := 1; size <= 3; size++ {
			fixed, err := signedFixedBytes(big.NewInt(int64(n)), size)
			if len(minimal) > size {
				if err == nil {
					t.Errorf("signedFixedBytes(%d, %d): expected width error", n, size)
				}
				continue
			}
			if err != nil {
				t.Errorf("signedFixedBytes(%d, %d): %s", n, size, err)
				continue
			}
			want := oracleFixedTwosComplement(n, size)
			if !bytesEqual(fixed, want) {
				t.Errorf("signedFixedBytes(%d, %d) = % x, oracle % x", n, size, fixed, want)
			}
			decoded := new(big.Int)
			fromSignedBytes(decoded, fixed)
			if decoded.Int64() != int64(n) {
				t.Errorf("decode after encode (%d, size %d) = %d", n, size, decoded.Int64())
			}
		}
	}
}

// oracleMinimalTwosComplement is an independent reference that negates the
// padded magnitude byte by byte and then adds one, instead of modular big.Int
// arithmetic.
func oracleMinimalTwosComplement(n int) []byte {
	if n == 0 {
		return []byte{0x00}
	}
	if n > 0 {
		b := big.NewInt(int64(n)).Bytes()
		if b[0]&0x80 != 0 {
			b = append([]byte{0x00}, b...)
		}
		return b
	}

	magnitude := big.NewInt(int64(-n)).Bytes()
	width := 1
	for threshold := int64(0x80); threshold < int64(-n); threshold *= 256 {
		width++
	}
	padded := make([]byte, width)
	copy(padded[width-len(magnitude):], magnitude)

	out := make([]byte, width)
	for i := range padded {
		out[i] = ^padded[i]
	}
	carry := 1
	for i := width - 1; i >= 0 && carry > 0; i-- {
		sum := int(out[i]) + carry
		out[i] = byte(sum)
		carry = sum >> 8
	}
	return out
}

func oracleFixedTwosComplement(n, size int) []byte {
	minimal := oracleMinimalTwosComplement(n)
	out := make([]byte, size)
	if n < 0 {
		for i := range out {
			out[i] = 0xff
		}
	}
	copy(out[size-len(minimal):], minimal)
	return out
}

func TestDecimalCanonicalOutputNormalization(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":6,"scale":2}`)

	testCases := []struct {
		input   string
		encoded []byte
		decoded string
	}{
		{"-0.00", []byte{0x00}, "0.00"},
		{"-0", []byte{0x00}, "0.00"},
		{"12.3", []byte{0x04, 0xce}, "12.30"},
		{"12.340", []byte{0x04, 0xd2}, "12.34"},
		{"1", []byte{0x64}, "1.00"},
	}

	for _, tc := range testCases {
		encoded, err := converter.Encode(tc.input)
		if err != nil {
			t.Fatalf("Encode(%q): %s", tc.input, err)
		}
		if !bytesEqual(encoded, tc.encoded) {
			t.Errorf("Encode(%q) = % x, want % x", tc.input, encoded, tc.encoded)
		}
		decoded, err := converter.Decode(encoded)
		if err != nil {
			t.Fatalf("Decode after %q: %s", tc.input, err)
		}
		if decoded != tc.decoded {
			t.Errorf("Decode after %q = %q, want %q", tc.input, decoded, tc.decoded)
		}
	}
}

func TestDecimalLargePrecisionNoFloat(t *testing.T) {
	converter := newConverterOrFail(t,
		`{"type":"bytes","logicalType":"decimal","precision":77,"scale":38}`)

	intPart := "123456789012345678901234567890123456789"
	fracPart := "11111111111111111111111111111111111111"
	text := intPart + "." + fracPart

	encoded, err := converter.Encode(text)
	if err != nil {
		t.Fatalf("Encode large decimal: %s", err)
	}
	expected := []byte("\x1b\x4b\x68\x19\x26\x11\xfa\xea\x20\x8f\xca\x21\x62\x7b\xe9\xda\xee\x32\x19\x83\x83\x95\x5d\xe8\x13\x1f\x4b\xf1\xc7\x1c\x71\xc7")
	if !bytesEqual(encoded, expected) {
		t.Fatalf("Encode large decimal = % x, want % x", encoded, expected)
	}

	decoded, err := converter.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode large decimal: %s", err)
	}
	if decoded != text {
		t.Fatalf("large decimal round trip:\n got %q\nwant %q", decoded, text)
	}

	// numeric equivalence via exact rationals proves no float64 involvement
	wantRat, ok := new(big.Rat).SetString(intPart + fracPart + "/1" +
		fracZeroes(38))
	if !ok {
		t.Fatal("cannot build expected rational")
	}
	gotRat, ok := new(big.Rat).SetString(decoded)
	if !ok {
		t.Fatalf("cannot parse decoded text %q", decoded)
	}
	if gotRat.Cmp(wantRat) != 0 {
		t.Errorf("decoded value %s != expected %s", gotRat, wantRat)
	}
}

func fracZeroes(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '0'
	}
	return string(out)
}

// TestDecimalConverterCompatibleWithCodec proves the checked layer exchanges
// the same on-wire logical bytes as the general Codec's *big.Rat representation.
func TestDecimalConverterCompatibleWithCodec(t *testing.T) {
	schema := `{"type":"bytes","logicalType":"decimal","precision":10,"scale":4}`
	codec, err := NewCodec(schema)
	if err != nil {
		t.Fatal(err)
	}
	converter := newConverterOrFail(t, schema)

	text := "123456.7890"
	logicalBytes, err := converter.Encode(text)
	if err != nil {
		t.Fatal(err)
	}

	// Frame the logical bytes with the plain bytes codec's variable length
	// prefix, then decode with the general Codec's *big.Rat representation.
	framed, err := bytesBinaryFromNative(nil, logicalBytes)
	if err != nil {
		t.Fatalf("bytesBinaryFromNative: %s", err)
	}
	native, _, err := codec.NativeFromBinary(framed)
	if err != nil {
		t.Fatalf("codec.NativeFromBinary: %s", err)
	}
	rat, ok := native.(*big.Rat)
	if !ok {
		t.Fatalf("expected *big.Rat, got %T", native)
	}
	if rat.FloatString(4) != text {
		t.Errorf("codec decoded %s, want %s", rat.FloatString(4), text)
	}

	// Conversely, Codec-produced logical bytes must decode through the converter.
	reFramed, err := codec.BinaryFromNative(nil, rat)
	if err != nil {
		t.Fatalf("codec.BinaryFromNative: %s", err)
	}
	nativeBytes, rest, err := bytesNativeFromBinary(reFramed)
	if err != nil {
		t.Fatalf("bytesNativeFromBinary: %s", err)
	}
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: % x", rest)
	}
	logicalFromCodec := nativeBytes.([]byte)
	if !bytesEqual(logicalFromCodec, logicalBytes) {
		t.Fatalf("codec logical bytes % x differ from converter % x",
			logicalFromCodec, logicalBytes)
	}
	roundTrip, err := converter.Decode(logicalFromCodec)
	if err != nil {
		t.Fatalf("converter.Decode: %s", err)
	}
	if roundTrip != text {
		t.Errorf("converter decoded %q, want %q", roundTrip, text)
	}
}
