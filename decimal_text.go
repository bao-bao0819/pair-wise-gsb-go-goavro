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
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// DecimalConverter translates decimal values between their exact decimal text
// representation and the raw logical bytes of an Avro "bytes" or "fixed"
// decimal logical type.
//
// Unlike the general Codec, which represents decoded decimals as *big.Rat,
// DecimalConverter works exclusively with decimal text such as "12.34" and
// never routes values through float32 or float64, so monetary amounts cannot
// lose precision in binary floating point.
//
// The raw logical bytes are the two's complement unscaled value defined by
// the Avro specification, without the "bytes" type's variable length prefix.
// For "bytes" schemas the shortest legal representation is returned; for
// "fixed" schemas the value is sign extended or truncated to the fixed size.
//
// A DecimalConverter is safe for concurrent use.
type DecimalConverter struct {
	precision int
	scale     int
	fixedSize int // 0 for "bytes" decimals, otherwise the fixed type size
}

// NewDecimalConverter returns a DecimalConverter for an Avro schema
// describing a decimal logical type. schemaSpecification is the JSON object
// for the type, for example:
//
//	{"type":"bytes","logicalType":"decimal","precision":10,"scale":2}
//	{"type":"fixed","size":8,"logicalType":"decimal","precision":18,"scale":4}
//
// An error is returned unless logicalType is exactly "decimal", the backing
// type is "bytes" or "fixed", precision and scale are valid Avro decimal
// properties, and the fixed size is large enough to hold every value allowed
// by the precision. Unknown logical types are never treated as decimals.
func NewDecimalConverter(schemaSpecification string) (*DecimalConverter, error) {
	var parsed interface{}
	if err := json.Unmarshal([]byte(schemaSpecification), &parsed); err != nil {
		return nil, fmt.Errorf("cannot parse decimal schema: %s", err)
	}
	schemaMap, _ := parsed.(map[string]interface{})
	if schemaMap == nil {
		return nil, errors.New("cannot create decimal converter from non-object schema")
	}

	logicalType, _ := schemaMap["logicalType"].(string)
	if logicalType == "" {
		if _, present := schemaMap["logicalType"]; present {
			return nil, fmt.Errorf("cannot create decimal converter: logicalType must be a JSON string, received %T", schemaMap["logicalType"])
		}
		return nil, errors.New(`cannot create decimal converter: missing logicalType "decimal"`)
	}
	if logicalType != "decimal" {
		return nil, fmt.Errorf("cannot create decimal converter: unsupported logicalType %q (only %q is supported)", logicalType, "decimal")
	}

	precision, scale, err := precisionAndScaleFromSchemaMap(schemaMap)
	if err != nil {
		return nil, err
	}

	converter := &DecimalConverter{precision: precision, scale: scale}

	backingType, _ := schemaMap["type"].(string)
	switch backingType {
	case "bytes":
		// fixedSize stays zero
	case "fixed":
		size, err := sizeFromSchemaMap(&name{fullName: "fixed.decimal"}, schemaMap)
		if err != nil {
			return nil, err
		}
		if required := bytesNeededForPrecision(precision); size < required {
			return nil, fmt.Errorf("cannot create decimal converter: fixed size %d is too small for precision %d (requires at least %d bytes)", size, precision, required)
		}
		converter.fixedSize = int(size)
	default:
		return nil, fmt.Errorf("cannot create decimal converter for backing type %q: decimal logical type requires %q or %q", backingType, "bytes", "fixed")
	}

	return converter, nil
}

// Precision returns the converter's decimal precision.
func (dc *DecimalConverter) Precision() int { return dc.precision }

// Scale returns the converter's decimal scale.
func (dc *DecimalConverter) Scale() int { return dc.scale }

// IsFixed reports whether the converter encodes a fixed decimal rather than a
// bytes decimal.
func (dc *DecimalConverter) IsFixed() bool { return dc.fixedSize > 0 }

// FixedSize returns the fixed type size in bytes, or 0 for a bytes decimal.
func (dc *DecimalConverter) FixedSize() int { return dc.fixedSize }

// Encode converts exact decimal text to the raw big-endian two's complement
// logical bytes of the unscaled value.
//
// decimalText must be a base-10 numeral with an optional sign and an optional
// decimal point (for example "1", "-0.00", "40.20"). It is parsed exactly;
// float32/float64 are never used.
//
// The value is scaled by 10^scale. An error is returned when scaling requires
// rounding (the input has more fractional digits than scale), when the scaled
// value needs more than precision decimal digits, or when it cannot fit in the
// fixed type width. No value is silently truncated.
func (dc *DecimalConverter) Encode(decimalText string) ([]byte, error) {
	unscaled, err := scaleDecimalText(decimalText, dc.scale)
	if err != nil {
		return nil, err
	}
	if err := checkUnscaledPrecision(unscaled, dc.precision); err != nil {
		return nil, err
	}
	if dc.fixedSize > 0 {
		return signedFixedBytes(unscaled, dc.fixedSize)
	}
	return minimalSignedBytes(unscaled), nil
}

// Decode converts raw big-endian two's complement logical bytes back to exact
// canonical decimal text.
//
// For fixed decimals the data must be exactly FixedSize bytes; for bytes
// decimals the data must be non-empty. An error is returned when the data
// length is wrong or when the unscaled value exceeds the schema precision.
//
// The returned text is canonical for the schema's scale:
//
//   - scale 0 produces an integer with no decimal point ("12", "-5", "0").
//   - scale greater than zero always emits exactly scale digits after the
//     decimal point, padding with trailing zeros when necessary ("40.20",
//     "0.000").
//   - Zero is always the unsigned form "0" (or "0.00" with scale); a negative
//     zero datum decodes to the same zero as a positive zero datum.
//
// Because of these rules the output can differ textually from the value that
// was originally encoded even though its numeric value is preserved: trailing
// fractional zeros and the sign of zero are normalized, and values encoded
// without all scale fractional digits are rendered with them. Encode(Decode(v))
// reproduces the same bytes for every legal datum.
func (dc *DecimalConverter) Decode(data []byte) (string, error) {
	if dc.fixedSize > 0 {
		if len(data) != dc.fixedSize {
			return "", fmt.Errorf("cannot decode fixed decimal: datum size ought to equal schema size: %d != %d", len(data), dc.fixedSize)
		}
	} else if len(data) == 0 {
		return "", errors.New("cannot decode bytes decimal: empty two's complement representation")
	}

	unscaled := new(big.Int)
	fromSignedBytes(unscaled, data)
	if err := checkUnscaledPrecision(unscaled, dc.precision); err != nil {
		return "", err
	}
	return formatScaledDecimal(unscaled, dc.scale), nil
}

// bytesNeededForPrecision returns the minimum fixed width in bytes that can
// hold every two's complement value with at most precision decimal digits.
func bytesNeededForPrecision(precision int) uint {
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision)), nil)
	// Smallest k with 2^(8*k-1) >= 10^p: max magnitude is 10^p - 1 and the
	// negative minimum -10^p+1 requires the same width.
	width := uint(1)
	threshold := big.NewInt(0x80) // 2^7
	for threshold.Cmp(limit) < 0 {
		width++
		threshold.Lsh(threshold, 8)
	}
	return width
}

// checkUnscaledPrecision rejects unscaled values whose decimal representation
// exceeds precision digits.
func checkUnscaledPrecision(unscaled *big.Int, precision int) error {
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision)), nil)
	abs := new(big.Int).Abs(unscaled)
	if abs.Cmp(limit) >= 0 {
		return fmt.Errorf("decimal value %s exceeds precision %d (unscaled magnitude must be smaller than %s)", unscaled, precision, limit.String())
	}
	return nil
}

// minimalSignedBytes returns the shortest legal big-endian two's complement
// representation of n. Zero is encoded as a single 0x00 byte.
func minimalSignedBytes(n *big.Int) []byte {
	switch n.Sign() {
	case 0:
		return []byte{0x00}
	case 1:
		b := n.Bytes()
		if b[0]&0x80 != 0 {
			return append([]byte{0x00}, b...)
		}
		return b
	default:
		// Smallest width k whose two's complement range reaches n: a k byte
		// value is negative exactly when its magnitude is at most 2^(8*k-1),
		// so -128 fits in one byte (0x80) but -129 needs two (0xFF7F).
		mag := new(big.Int).Neg(n)
		width := 1
		threshold := big.NewInt(0x80) // 2^7
		for mag.Cmp(threshold) > 0 {
			width++
			threshold.Lsh(threshold, 8)
		}
		mod := new(big.Int).Lsh(one, uint(width*8))
		return new(big.Int).Sub(mod, mag).Bytes()
	}
}

// signedFixedBytes returns the big-endian two's complement representation of n
// padded or sign extended to exactly size bytes. It returns an error instead
// of truncating when the value cannot fit.
func signedFixedBytes(n *big.Int, size int) ([]byte, error) {
	raw := minimalSignedBytes(n)
	if len(raw) > size {
		return nil, fmt.Errorf("cannot encode fixed decimal: value needs %d bytes but fixed size is %d", len(raw), size)
	}
	out := make([]byte, size)
	if n.Sign() < 0 {
		for i := range out {
			out[i] = 0xff
		}
	}
	copy(out[size-len(raw):], raw)
	return out, nil
}

// formatScaledDecimal renders an unscaled integer and scale as canonical
// decimal text.
func formatScaledDecimal(unscaled *big.Int, scale int) string {
	if scale == 0 {
		return unscaled.String()
	}

	negative := unscaled.Sign() < 0
	digits := new(big.Int).Abs(unscaled).String()
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale+1-len(digits)) + digits
	}
	integerPart := digits[:len(digits)-scale]
	fractionPart := digits[len(digits)-scale:]
	if negative {
		return "-" + integerPart + "." + fractionPart
	}
	return integerPart + "." + fractionPart
}

// scaleDecimalText parses exact decimal text and multiplies it by 10^scale,
// returning an error when the result would not be integral.
func scaleDecimalText(text string, scale int) (*big.Int, error) {
	num, den, err := parseDecimalText(text)
	if err != nil {
		return nil, err
	}

	scaling := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	num.Mul(num, scaling)

	remainder := new(big.Int)
	quotient, remainder := new(big.Int).QuoRem(num, den, remainder)
	if remainder.Sign() != 0 {
		return nil, fmt.Errorf("cannot exactly scale decimal %q to scale %d: rounding would be required", text, scale)
	}
	return quotient, nil
}

// parseDecimalText parses a signed base-10 numeral with an optional decimal
// point into an exact fraction num/den (den is a power of ten). float64 is
// never used.
func parseDecimalText(text string) (*big.Int, *big.Int, error) {
	if text == "" {
		return nil, nil, errors.New("cannot parse empty decimal text")
	}

	i := 0
	negative := false
	switch text[i] {
	case '+':
		i++
	case '-':
		negative = true
		i++
	}
	if i >= len(text) {
		return nil, nil, fmt.Errorf("cannot parse decimal text %q: missing digits", text)
	}

	var integerDigits, fractionDigits []byte
	sawDot := false
	for ; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= '0' && c <= '9':
			if sawDot {
				fractionDigits = append(fractionDigits, c)
			} else {
				integerDigits = append(integerDigits, c)
			}
		case c == '.' && !sawDot:
			sawDot = true
		default:
			return nil, nil, fmt.Errorf("cannot parse decimal text %q: unexpected character %q", text, string(c))
		}
	}

	if len(integerDigits)+len(fractionDigits) == 0 {
		return nil, nil, fmt.Errorf("cannot parse decimal text %q: missing digits", text)
	}
	if sawDot && (len(integerDigits) == 0 || len(fractionDigits) == 0) {
		return nil, nil, fmt.Errorf("cannot parse decimal text %q: digits are required on both sides of the decimal point", text)
	}

	allDigits := string(integerDigits) + string(fractionDigits)
	mantissa, ok := new(big.Int).SetString(allDigits, 10)
	if !ok {
		return nil, nil, fmt.Errorf("cannot parse decimal text %q", text)
	}

	denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fractionDigits))), nil)
	if negative {
		mantissa.Neg(mantissa)
	}
	return mantissa, denom, nil
}
