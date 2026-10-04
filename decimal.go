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

// This file provides a checked conversion layer between decimal text strings
// and the Avro decimal logical type stored in `bytes` or `fixed` values. It
// is intended for services that exchange monetary amounts as decimal text and
// must never route those values through float64.
//
// The layer is strict by design:
//
//   - The schema must be an Avro `bytes` or `fixed` type whose logicalType is
//     exactly "decimal". Any other logicalType, or a missing logicalType, is
//     rejected so unknown logical types are never silently treated as decimal.
//   - Values are scaled exactly to the schema scale. Input carrying more
//     fractional digits than the scale permits is rejected rather than
//     rounded or truncated.
//   - Values whose unscaled integer needs more digits than the schema
//     precision, or more bytes than a fixed size provides, are rejected.
//   - Negative numbers are encoded as two's complement with correct sign
//     extension: `bytes` uses the shortest legal representation, `fixed` is
//     padded to the schema size with the sign byte.
//
// Text normalization rules (stable output):
//
//   - DecimalTextFromBinary always emits exactly `scale` fractional digits,
//     so trailing fractional zeros are kept: unscaled 150 with scale 2 prints
//     as "1.50".
//   - Zero and negative zero ("-0", "-0.00") encode identically; decoding
//     always yields the non-negative form "0" (scale 0) or "0.00" etc.
//   - Leading zeros and a leading "+" are accepted on input but never
//     emitted: "007.5" with scale 2 round-trips to "7.50".
//   - Fractional digits beyond the schema scale are accepted only when they
//     are all zeros ("1.230" with scale 2 is exactly "1.23"); any other
//     excess digit requires rounding and is rejected with an error.
//
// A text -> binary -> text round trip therefore preserves the numeric value
// but not necessarily the lexical form of the input string.

// decimalSpec describes a validated Avro decimal schema.
type decimalSpec struct {
	precision int
	scale     int
	size      uint // fixed size in bytes; 0 when the schema type is bytes
	fixed     bool
}

// DecimalBinaryFromText converts a decimal text string such as "-1234.50"
// into the two's complement big-endian byte representation of an Avro decimal
// logical type described by schema.
//
// The schema must be a JSON Avro schema of type "bytes" or "fixed" with
// logicalType "decimal", for example:
//
//	{"type":"bytes","logicalType":"decimal","precision":9,"scale":2}
//	{"type":"fixed","name":"amount","size":4,"logicalType":"decimal","precision":9,"scale":2}
//
// For a "bytes" schema the shortest legal two's complement representation is
// returned; when embedding it into an Avro stream as `bytes`, prepend the
// Avro long-encoded length. For a "fixed" schema the result is padded with
// the sign byte to exactly the schema size.
//
// An error is returned when the text is not a plain decimal string, when
// scaling to the schema scale would require rounding, when the value exceeds
// the schema precision, or when it does not fit the fixed size.
func DecimalBinaryFromText(schema, text string) ([]byte, error) {
	spec, err := decimalSpecFromSchema(schema)
	if err != nil {
		return nil, err
	}
	unscaled, err := decimalUnscaledFromText(text, spec.precision, spec.scale)
	if err != nil {
		return nil, err
	}
	return spec.encode(unscaled)
}

// DecimalTextFromBinary converts the two's complement big-endian byte
// representation of an Avro decimal logical type back into a decimal text
// string, using the normalization rules documented above: exactly `scale`
// fractional digits, no negative zero, no superfluous leading zeros.
//
// For a "bytes" schema data must be the decimal content bytes (without the
// Avro length prefix); an empty slice decodes as zero. For a "fixed" schema
// data must be exactly the schema size.
func DecimalTextFromBinary(schema string, data []byte) (string, error) {
	spec, err := decimalSpecFromSchema(schema)
	if err != nil {
		return "", err
	}
	unscaled, err := spec.decode(data)
	if err != nil {
		return "", err
	}
	return decimalTextFromUnscaled(unscaled, spec.scale), nil
}

// decimalSpecFromSchema parses and validates a JSON Avro schema, requiring a
// bytes or fixed type with logicalType "decimal".
func decimalSpecFromSchema(schema string) (*decimalSpec, error) {
	var schemaMap map[string]interface{}
	if err := json.Unmarshal([]byte(schema), &schemaMap); err != nil {
		return nil, fmt.Errorf("cannot parse decimal schema: %s", err)
	}
	typeName, ok := schemaMap["type"].(string)
	if !ok || (typeName != "bytes" && typeName != "fixed") {
		return nil, fmt.Errorf("cannot use decimal conversion with schema type %v: expected \"bytes\" or \"fixed\"", schemaMap["type"])
	}
	logicalType, ok := schemaMap["logicalType"]
	if !ok {
		return nil, errors.New("cannot use decimal conversion without a logicalType: expected \"decimal\"")
	}
	if logicalType != "decimal" {
		return nil, fmt.Errorf("cannot use decimal conversion with logicalType %v: expected \"decimal\"", logicalType)
	}
	precision, scale, err := precisionAndScaleFromSchemaMap(schemaMap)
	if err != nil {
		return nil, err
	}
	spec := &decimalSpec{precision: precision, scale: scale}
	if typeName == "bytes" {
		return spec, nil
	}
	spec.fixed = true
	size, err := decimalFixedSizeFromSchemaMap(schemaMap)
	if err != nil {
		return nil, err
	}
	spec.size = size
	if maxPrecision := maxDecimalPrecisionForFixedSize(size); precision > maxPrecision {
		return nil, fmt.Errorf("cannot use decimal conversion when precision %d exceeds the maximum precision %d supported by a fixed size of %d bytes", precision, maxPrecision, size)
	}
	return spec, nil
}

// decimalFixedSizeFromSchemaMap extracts and validates the fixed size.
func decimalFixedSizeFromSchemaMap(schemaMap map[string]interface{}) (uint, error) {
	sizeRaw, ok := schemaMap["size"]
	if !ok {
		return 0, errors.New("cannot use decimal conversion with fixed schema without size")
	}
	size, ok := sizeRaw.(float64)
	if !ok || size != float64(uint(size)) || uint(size) == 0 {
		return 0, fmt.Errorf("cannot use decimal conversion with fixed size %v: expected a positive integer", sizeRaw)
	}
	return uint(size), nil
}

// maxDecimalPrecisionForFixedSize returns the maximum number of decimal
// digits that fit in a two's complement integer stored in size bytes, per the
// Avro specification: floor(log10(2^(8*size-1) - 1)).
func maxDecimalPrecisionForFixedSize(size uint) int {
	// 2^(8*size-1) is one more than the maximum positive value; its decimal
	// digit count minus one is the largest precision whose every value fits.
	maxPlusOne := new(big.Int).Lsh(one, size*8-1)
	return len(maxPlusOne.Text(10)) - 1
}

// decimalUnscaledFromText parses a plain decimal string and returns the
// unscaled integer value scaled to the given scale. It never uses floating
// point arithmetic.
func decimalUnscaledFromText(text string, precision, scale int) (*big.Int, error) {
	s := text
	var neg bool
	if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
		neg = s[0] == '-'
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		return nil, fmt.Errorf("cannot parse decimal %q: expected digits before the decimal point", text)
	}
	if strings.IndexByte(s, '.') >= 0 && fracPart == "" {
		return nil, fmt.Errorf("cannot parse decimal %q: expected digits after the decimal point", text)
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return nil, fmt.Errorf("cannot parse decimal %q: invalid character %#U", text, intPart[i])
		}
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return nil, fmt.Errorf("cannot parse decimal %q: invalid character %#U", text, fracPart[i])
		}
	}
	if len(fracPart) > scale {
		// Fractional digits beyond the schema scale are only permitted when
		// they are all zeros, because dropping them is exact and requires no
		// rounding.
		if extra := fracPart[scale:]; strings.Trim(extra, "0") != "" {
			return nil, fmt.Errorf("cannot convert decimal %q to scale %d without rounding: value has %d fractional digits", text, scale, len(fracPart))
		}
		fracPart = fracPart[:scale]
	}
	digits := strings.TrimLeft(intPart+fracPart, "0")
	unscaled := new(big.Int)
	if digits != "" {
		unscaled.SetString(digits, 10)
	}
	if shift := scale - len(fracPart); shift > 0 {
		unscaled.Mul(unscaled, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(shift)), nil))
	}
	if digitCount := len(unscaled.Text(10)); digitCount > precision {
		return nil, fmt.Errorf("cannot convert decimal %q: unscaled value requires %d digits, exceeding schema precision %d", text, digitCount, precision)
	}
	if neg && unscaled.Sign() != 0 {
		unscaled.Neg(unscaled)
	}
	return unscaled, nil
}

// encode returns the two's complement big-endian representation of the
// unscaled value: the shortest legal representation for bytes, sign-extended
// to the schema size for fixed.
func (spec *decimalSpec) encode(unscaled *big.Int) ([]byte, error) {
	minimal, err := toSignedBytes(unscaled)
	if err != nil {
		return nil, err
	}
	if !spec.fixed {
		return minimal, nil
	}
	if count := uint(len(minimal)); count > spec.size {
		return nil, fmt.Errorf("cannot encode decimal: value requires %d bytes, exceeding fixed size %d", count, spec.size)
	}
	pad := byte(0x00)
	if unscaled.Sign() < 0 {
		pad = 0xff
	}
	out := make([]byte, spec.size)
	for i := range out {
		out[i] = pad
	}
	copy(out[spec.size-uint(len(minimal)):], minimal)
	return out, nil
}

// decode interprets data as a two's complement big-endian unscaled integer.
func (spec *decimalSpec) decode(data []byte) (*big.Int, error) {
	if spec.fixed && uint(len(data)) != spec.size {
		return nil, fmt.Errorf("cannot decode decimal fixed: datum size ought to equal schema size: %d != %d", len(data), spec.size)
	}
	unscaled := new(big.Int)
	fromSignedBytes(unscaled, data)
	return unscaled, nil
}

// decimalTextFromUnscaled formats the unscaled integer as a decimal string
// with exactly scale fractional digits. Zero is always rendered without a
// sign, so negative zero cannot be produced.
func decimalTextFromUnscaled(unscaled *big.Int, scale int) string {
	neg := unscaled.Sign() < 0
	digits := new(big.Int).Abs(unscaled).Text(10)
	if scale > 0 {
		for len(digits) <= scale {
			digits = "0" + digits
		}
		split := len(digits) - scale
		digits = digits[:split] + "." + digits[split:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}
