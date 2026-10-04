package goavro

import (
	"bytes"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

const (
	testDecimalBytesSchema = `{"type":"bytes","logicalType":"decimal","precision":10,"scale":0}`
	testDecimalMoneySchema = `{"type":"bytes","logicalType":"decimal","precision":10,"scale":2}`
	testDecimalFixedSchema = `{"type":"fixed","name":"amount","size":4,"logicalType":"decimal","precision":9,"scale":2}`
)

// oracleTwosComplement is an independent oracle computing the shortest
// big-endian two's complement representation of a small integer.
func oracleTwosComplement(v int64) []byte {
	var buf [8]byte
	u := uint64(v)
	for i := 7; i >= 0; i-- {
		buf[i] = byte(u)
		u >>= 8
	}
	out := buf[:]
	for len(out) > 1 {
		if v >= 0 && out[0] == 0x00 && out[1]&0x80 == 0 {
			out = out[1:]
		} else if v < 0 && out[0] == 0xff && out[1]&0x80 != 0 {
			out = out[1:]
		} else {
			break
		}
	}
	trimmed := make([]byte, len(out))
	copy(trimmed, out)
	return trimmed
}

func TestDecimalSmallIntegerOracle(t *testing.T) {
	for i := int64(-1000); i <= 1000; i++ {
		text := strconv.FormatInt(i, 10)
		encoded, err := DecimalBinaryFromText(testDecimalBytesSchema, text)
		if err != nil {
			t.Fatalf("DecimalBinaryFromText(%q): %s", text, err)
		}
		if want := oracleTwosComplement(i); !bytes.Equal(encoded, want) {
			t.Errorf("DecimalBinaryFromText(%q): got % x, want % x", text, encoded, want)
		}
		decoded, err := DecimalTextFromBinary(testDecimalBytesSchema, encoded)
		if err != nil {
			t.Fatalf("DecimalTextFromBinary(% x): %s", encoded, err)
		}
		if decoded != text {
			t.Errorf("round trip of %q: got %q", text, decoded)
		}
	}
}

func TestDecimalBytesBoundarySamples(t *testing.T) {
	cases := []struct {
		text  string
		bytes []byte
	}{
		{"0", []byte{0x00}},
		{"1", []byte{0x01}},
		{"127", []byte{0x7f}},
		{"128", []byte{0x00, 0x80}}, // most significant bit set: needs leading zero byte
		{"255", []byte{0x00, 0xff}},
		{"256", []byte{0x01, 0x00}},
		{"32768", []byte{0x00, 0x80, 0x00}},
		{"-1", []byte{0xff}},
		{"-127", []byte{0x81}},
		{"-128", []byte{0x80}},       // shortest negative form, no 0xff prefix
		{"-129", []byte{0xff, 0x7f}}, // sign extension byte required
		{"-256", []byte{0xff, 0x00}},
		{"-32768", []byte{0x80, 0x00}},
		{"-32769", []byte{0xff, 0x7f, 0xff}},
	}
	for _, c := range cases {
		encoded, err := DecimalBinaryFromText(testDecimalBytesSchema, c.text)
		if err != nil {
			t.Errorf("DecimalBinaryFromText(%q): %s", c.text, err)
			continue
		}
		if !bytes.Equal(encoded, c.bytes) {
			t.Errorf("DecimalBinaryFromText(%q): got % x, want % x", c.text, encoded, c.bytes)
		}
		decoded, err := DecimalTextFromBinary(testDecimalBytesSchema, c.bytes)
		if err != nil {
			t.Errorf("DecimalTextFromBinary(% x): %s", c.bytes, err)
			continue
		}
		if decoded != c.text {
			t.Errorf("DecimalTextFromBinary(% x): got %q, want %q", c.bytes, decoded, c.text)
		}
	}
}

func TestDecimalScaleExceedsInputFractionDigits(t *testing.T) {
	schema := `{"type":"bytes","logicalType":"decimal","precision":10,"scale":4}`
	// "1.5" has one fractional digit; scaling to 4 multiplies by 10^3.
	encoded, err := DecimalBinaryFromText(schema, "1.5")
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x3a, 0x98}; !bytes.Equal(encoded, want) { // 15000
		t.Fatalf("got % x, want % x", encoded, want)
	}
	decoded, err := DecimalTextFromBinary(schema, encoded)
	if err != nil {
		t.Fatal(err)
	}
	// Normalization: exactly scale fractional digits are emitted.
	if want := "1.5000"; decoded != want {
		t.Errorf("got %q, want %q", decoded, want)
	}
}

func TestDecimalInexactScalingRejected(t *testing.T) {
	if _, err := DecimalBinaryFromText(testDecimalMoneySchema, "1.234"); err == nil {
		t.Error("expected error when rounding would be required")
	} else if !strings.Contains(err.Error(), "without rounding") {
		t.Errorf("unexpected error: %s", err)
	}
	if _, err := DecimalBinaryFromText(testDecimalMoneySchema, "0.001"); err == nil {
		t.Error("expected error for sub-scale quantum")
	}
	// Excess fractional digits that are all zeros are exact, not rounding.
	encoded, err := DecimalBinaryFromText(testDecimalMoneySchema, "1.2300")
	if err != nil {
		t.Fatalf("trailing zeros beyond scale ought to be accepted: %s", err)
	}
	decoded, err := DecimalTextFromBinary(testDecimalMoneySchema, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1.23"; decoded != want {
		t.Errorf("got %q, want %q", decoded, want)
	}
}

func TestDecimalPrecisionExceeded(t *testing.T) {
	schema := `{"type":"bytes","logicalType":"decimal","precision":4,"scale":2}`
	if _, err := DecimalBinaryFromText(schema, "99.99"); err != nil {
		t.Errorf("99.99 ought to fit precision 4 scale 2: %s", err)
	}
	for _, text := range []string{"100.00", "1234.56", "-100.00", "10000"} {
		if _, err := DecimalBinaryFromText(schema, text); err == nil {
			t.Errorf("expected precision error for %q", text)
		} else if !strings.Contains(err.Error(), "precision") {
			t.Errorf("unexpected error for %q: %s", text, err)
		}
	}
}

func TestDecimalFixedPaddingAndSignExtension(t *testing.T) {
	cases := []struct {
		text  string
		bytes []byte
	}{
		{"0.00", []byte{0x00, 0x00, 0x00, 0x00}},
		{"1.00", []byte{0x00, 0x00, 0x00, 0x64}},   // unscaled 100
		{"99.99", []byte{0x00, 0x00, 0x27, 0x0f}},  // unscaled 9999
		{"-0.01", []byte{0xff, 0xff, 0xff, 0xff}},  // unscaled -1
		{"-1.00", []byte{0xff, 0xff, 0xff, 0x9c}},  // unscaled -100
		{"-99.99", []byte{0xff, 0xff, 0xd8, 0xf1}}, // unscaled -9999
		{"128.00", []byte{0x00, 0x00, 0x32, 0x00}}, // unscaled 12800
	}
	for _, c := range cases {
		encoded, err := DecimalBinaryFromText(testDecimalFixedSchema, c.text)
		if err != nil {
			t.Errorf("DecimalBinaryFromText(%q): %s", c.text, err)
			continue
		}
		if !bytes.Equal(encoded, c.bytes) {
			t.Errorf("DecimalBinaryFromText(%q): got % x, want % x", c.text, encoded, c.bytes)
		}
		decoded, err := DecimalTextFromBinary(testDecimalFixedSchema, c.bytes)
		if err != nil {
			t.Errorf("DecimalTextFromBinary(% x): %s", c.bytes, err)
			continue
		}
		if decoded != c.text {
			t.Errorf("DecimalTextFromBinary(% x): got %q, want %q", c.bytes, decoded, c.text)
		}
	}
}

func TestDecimalFixedSizeInsufficient(t *testing.T) {
	// Schema level: a 2-byte fixed holds at most 4 decimal digits.
	schema := `{"type":"fixed","name":"d","size":2,"logicalType":"decimal","precision":5,"scale":0}`
	if _, err := DecimalBinaryFromText(schema, "1"); err == nil {
		t.Error("expected error when precision exceeds fixed capacity")
	}
	// Value level: defensive check when a spec is constructed directly.
	spec := &decimalSpec{precision: 20, scale: 0, size: 1, fixed: true}
	if _, err := spec.encode(big.NewInt(128)); err == nil {
		t.Error("expected error when value does not fit fixed size")
	}
	if _, err := spec.encode(big.NewInt(-129)); err == nil {
		t.Error("expected error when negative value does not fit fixed size")
	}
}

func TestDecimalFixedDecodeWrongSize(t *testing.T) {
	if _, err := DecimalTextFromBinary(testDecimalFixedSchema, []byte{0x00, 0x64}); err == nil {
		t.Error("expected error when datum size differs from fixed size")
	}
}

func TestDecimalZeroAndNegativeZero(t *testing.T) {
	// Zero, negative zero, and trailing fractional zeros all encode to the
	// same single zero byte and decode to the stable non-negative form.
	for _, text := range []string{"0", "-0", "+0", "0.00", "-0.00", "000.000"} {
		encoded, err := DecimalBinaryFromText(testDecimalMoneySchema, text)
		if err != nil {
			t.Errorf("DecimalBinaryFromText(%q): %s", text, err)
			continue
		}
		if want := []byte{0x00}; !bytes.Equal(encoded, want) {
			t.Errorf("DecimalBinaryFromText(%q): got % x, want % x", text, encoded, want)
		}
	}
	decoded, err := DecimalTextFromBinary(testDecimalMoneySchema, []byte{0x00})
	if err != nil {
		t.Fatal(err)
	}
	if want := "0.00"; decoded != want {
		t.Errorf("got %q, want %q", decoded, want)
	}
	// Empty bytes content also decodes as zero.
	decoded, err = DecimalTextFromBinary(testDecimalMoneySchema, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "0.00"; decoded != want {
		t.Errorf("got %q, want %q", decoded, want)
	}
	// Scale 0 renders zero without a decimal point.
	decoded, err = DecimalTextFromBinary(testDecimalBytesSchema, []byte{0x00})
	if err != nil {
		t.Fatal(err)
	}
	if want := "0"; decoded != want {
		t.Errorf("got %q, want %q", decoded, want)
	}
}

func TestDecimalRejectsNonDecimalSchemas(t *testing.T) {
	schemas := []string{
		`{"type":"bytes","logicalType":"uuid","precision":4,"scale":2}`, // unknown logicalType
		`{"type":"bytes","logicalType":"date","precision":4,"scale":2}`, // known but not decimal
		`{"type":"bytes","precision":4,"scale":2}`,                      // missing logicalType
		`{"type":"string","logicalType":"decimal","precision":4,"scale":2}`,
		`{"type":"int","logicalType":"decimal","precision":4,"scale":2}`,
		`{"type":"fixed","name":"d","logicalType":"decimal","precision":4,"scale":2}`, // no size
		`{"type":"bytes","logicalType":"decimal","scale":2}`,                          // no precision
		`{"type":"bytes","logicalType":"decimal","precision":2,"scale":4}`,            // scale > precision
		`not json`,
	}
	for _, schema := range schemas {
		if _, err := DecimalBinaryFromText(schema, "1.00"); err == nil {
			t.Errorf("DecimalBinaryFromText ought to reject schema %s", schema)
		}
		if _, err := DecimalTextFromBinary(schema, []byte{0x00}); err == nil {
			t.Errorf("DecimalTextFromBinary ought to reject schema %s", schema)
		}
	}
}

func TestDecimalRejectsMalformedText(t *testing.T) {
	for _, text := range []string{"", ".", "-", "+", ".5", "5.", "1.2.3", "1e3", "1,00", "abc", " 1", "1 ", "--1", "NaN"} {
		if _, err := DecimalBinaryFromText(testDecimalMoneySchema, text); err == nil {
			t.Errorf("expected error for malformed decimal %q", text)
		}
	}
}

func TestDecimalRoundTripNormalization(t *testing.T) {
	// Round trips preserve the numeric value; the lexical form is normalized
	// to exactly scale fractional digits without a leading "+" or superfluous
	// leading zeros.
	cases := []struct{ in, out string }{
		{"1.5", "1.50"},
		{"007.50", "7.50"},
		{"+2", "2.00"},
		{"-0.00", "0.00"},
		{"1.230", "1.23"},
		{"-1234.56", "-1234.56"},
	}
	for _, c := range cases {
		encoded, err := DecimalBinaryFromText(testDecimalMoneySchema, c.in)
		if err != nil {
			t.Errorf("DecimalBinaryFromText(%q): %s", c.in, err)
			continue
		}
		decoded, err := DecimalTextFromBinary(testDecimalMoneySchema, encoded)
		if err != nil {
			t.Errorf("DecimalTextFromBinary: %s", err)
			continue
		}
		if decoded != c.out {
			t.Errorf("round trip of %q: got %q, want %q", c.in, decoded, c.out)
		}
	}
}

// TestDecimalCodecCompatibility verifies the checked text conversion layer
// agrees with the generic Codec's spec-compliant decimal representation,
// which remains *big.Rat based and unchanged.
func TestDecimalCodecCompatibility(t *testing.T) {
	option := DefaultCodecOption()
	option.EnableDecimalBinarySpecCompliantEncoding = true

	codec, err := NewCodecWithOptions(testDecimalMoneySchema, option)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := DecimalBinaryFromText(testDecimalMoneySchema, "40.20")
	if err != nil {
		t.Fatal(err)
	}
	// The generic codec prepends the Avro long-encoded bytes length.
	binary, err := codec.BinaryFromNative(nil, big.NewRat(4020, 100))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(binary[1:], encoded) {
		t.Errorf("codec content % x does not match text layer % x", binary[1:], encoded)
	}
	// Decoding the text-layer bytes through the generic codec yields the
	// same numeric value as the text layer.
	native, _, err := codec.NativeFromBinary(binary)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecimalTextFromBinary(testDecimalMoneySchema, encoded)
	if err != nil {
		t.Fatal(err)
	}
	rat, ok := new(big.Rat).SetString(decoded)
	if !ok {
		t.Fatalf("cannot parse %q as big.Rat", decoded)
	}
	if native.(*big.Rat).Cmp(rat) != 0 {
		t.Errorf("codec native %v does not match text layer %q", native, decoded)
	}

	// Fixed decimals: the codec binary is exactly the text-layer output.
	fixedCodec, err := NewCodecWithOptions(testDecimalFixedSchema, option)
	if err != nil {
		t.Fatal(err)
	}
	fixedEncoded, err := DecimalBinaryFromText(testDecimalFixedSchema, "-1")
	if err != nil {
		t.Fatal(err)
	}
	fixedBinary, err := fixedCodec.BinaryFromNative(nil, big.NewRat(-100, 100))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fixedBinary, fixedEncoded) {
		t.Errorf("codec fixed % x does not match text layer % x", fixedBinary, fixedEncoded)
	}
}
