// Package canonicaljson defines the bounded Fused canonical JSON wire format.
// Object names use decoded UTF-8 order; strings escape JSON syntax, controls,
// HTML-sensitive runes, U+2028, and U+2029 as the language-neutral contract
// specifies. Duplicate decoded names are rejected because accepting them would
// make identity depend on a parser's member-selection policy.
package canonicaljson

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	Version               = "fused-canonical-json-v1"
	MaxInputBytes         = 1 << 20
	MaxDepth              = 64
	MaxValues             = 65536
	MaxNumberDigits       = 4096
	MaxAbsDecimalExponent = 16383
)

type valueKind uint8

const (
	kindNull valueKind = iota
	kindBoolean
	kindString
	kindNumber
	kindArray
	kindObject
)

type value struct {
	kind    valueKind
	text    string
	boolean bool
	items   []value
	members []member
}

type member struct {
	name  string
	value value
}

type parser struct {
	decoder *json.Decoder
	values  int
}

// Canonicalize returns one deterministic representation of exactly one JSON
// value while enforcing the same resource limits at every trust boundary.
func Canonicalize(raw []byte) ([]byte, error) {
	if err := validateInput(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	p := parser{decoder: decoder}
	root, err := p.parseValue(0)
	if err != nil {
		return nil, err
	}
	if err := requireEnd(decoder); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeValue(&output, root); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// SHA256 returns the binary digest of the canonical representation.
func SHA256(raw []byte) ([sha256.Size]byte, error) {
	canonical, err := Canonicalize(raw)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// HexSHA256 returns the lowercase digest used by schema contracts.
func HexSHA256(raw []byte) (string, error) {
	digest, err := SHA256(raw)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest[:]), nil
}

// Equal compares JSON values by their bounded canonical representation.
func Equal(left, right []byte) (bool, error) {
	leftCanonical, err := Canonicalize(left)
	if err != nil {
		return false, err
	}
	rightCanonical, err := Canonicalize(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftCanonical, rightCanonical), nil
}

func validateInput(raw []byte) error {
	if len(raw) > MaxInputBytes {
		return fmt.Errorf("canonical JSON exceeds maximum input size %d", MaxInputBytes)
	}
	if !utf8.Valid(raw) {
		return fmt.Errorf("canonical JSON must be UTF-8")
	}
	return validateUnicodeEscapes(raw)
}

func (p *parser) parseValue(depth int) (value, error) {
	if p.values >= MaxValues {
		return value{}, fmt.Errorf("canonical JSON exceeds maximum value count %d", MaxValues)
	}
	p.values++
	token, err := p.decoder.Token()
	if err != nil {
		return value{}, fmt.Errorf("decode canonical JSON: %w", err)
	}
	return p.valueFromToken(token, depth)
}

func (p *parser) valueFromToken(token json.Token, depth int) (value, error) {
	switch typed := token.(type) {
	case nil:
		return value{kind: kindNull}, nil
	case bool:
		return value{kind: kindBoolean, boolean: typed}, nil
	case string:
		return value{kind: kindString, text: typed}, nil
	case json.Number:
		return numberValue(string(typed))
	case json.Delim:
		return p.containerValue(typed, depth)
	default:
		return value{}, fmt.Errorf("decode canonical JSON: unsupported token")
	}
}

func (p *parser) containerValue(delimiter json.Delim, depth int) (value, error) {
	if depth >= MaxDepth {
		return value{}, fmt.Errorf("canonical JSON exceeds maximum depth %d", MaxDepth)
	}
	switch delimiter {
	case '[':
		return p.parseArray(depth + 1)
	case '{':
		return p.parseObject(depth + 1)
	default:
		return value{}, fmt.Errorf("decode canonical JSON: unexpected delimiter %q", delimiter)
	}
}

func (p *parser) parseArray(depth int) (value, error) {
	items := make([]value, 0)
	for p.decoder.More() {
		item, err := p.parseValue(depth)
		if err != nil {
			return value{}, err
		}
		items = append(items, item)
	}
	if err := p.consumeDelimiter(']'); err != nil {
		return value{}, err
	}
	return value{kind: kindArray, items: items}, nil
}

func (p *parser) parseObject(depth int) (value, error) {
	members := make([]member, 0)
	seen := make(map[string]struct{})
	for p.decoder.More() {
		name, err := p.parseMemberName(seen)
		if err != nil {
			return value{}, err
		}
		child, err := p.parseValue(depth)
		if err != nil {
			return value{}, err
		}
		members = append(members, member{name: name, value: child})
	}
	if err := p.consumeDelimiter('}'); err != nil {
		return value{}, err
	}
	sort.Slice(members, func(i, j int) bool { return members[i].name < members[j].name })
	return value{kind: kindObject, members: members}, nil
}

func (p *parser) parseMemberName(seen map[string]struct{}) (string, error) {
	token, err := p.decoder.Token()
	if err != nil {
		return "", fmt.Errorf("decode canonical JSON object name: %w", err)
	}
	name, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("decode canonical JSON: object name is not a string")
	}
	if _, duplicate := seen[name]; duplicate {
		return "", fmt.Errorf("canonical JSON contains duplicate object name")
	}
	seen[name] = struct{}{}
	return name, nil
}

func (p *parser) consumeDelimiter(want json.Delim) error {
	token, err := p.decoder.Token()
	if err != nil {
		return fmt.Errorf("decode canonical JSON: %w", err)
	}
	if token != want {
		return fmt.Errorf("decode canonical JSON: expected delimiter %q", want)
	}
	return nil
}

func requireEnd(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode canonical JSON: multiple values")
		}
		return fmt.Errorf("decode canonical JSON: trailing data: %w", err)
	}
	return nil
}

func numberValue(raw string) (value, error) {
	normalized, err := normalizeNumber(raw)
	if err != nil {
		return value{}, err
	}
	return value{kind: kindNumber, text: normalized}, nil
}

func normalizeNumber(raw string) (string, error) {
	negative := strings.HasPrefix(raw, "-")
	unsigned := strings.TrimPrefix(raw, "-")
	mantissa, exponentText := splitExponent(unsigned)
	digits, fractionalDigits := coefficient(mantissa)
	if len(digits) > MaxNumberDigits {
		return "", fmt.Errorf("canonical JSON number exceeds maximum digit count %d", MaxNumberDigits)
	}
	exponent, err := boundedExponent(exponentText)
	if err != nil {
		return "", err
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	trailingZeros := len(digits) - len(strings.TrimRight(digits, "0"))
	digits = strings.TrimRight(digits, "0")
	exponent += trailingZeros - fractionalDigits + len(digits) - 1
	if exponent < -MaxAbsDecimalExponent || exponent > MaxAbsDecimalExponent {
		return "", fmt.Errorf("canonical JSON number exceeds maximum decimal exponent %d", MaxAbsDecimalExponent)
	}
	return formatNumber(negative, digits, exponent), nil
}

func boundedExponent(raw string) (int, error) {
	negative := strings.HasPrefix(raw, "-")
	digits := strings.TrimPrefix(raw, "-")
	if digits == "" {
		return 0, fmt.Errorf("canonical JSON number has an invalid exponent")
	}
	exponent := 0
	for _, digit := range digits {
		if digit < '0' || digit > '9' || exponent > MaxAbsDecimalExponent/10 {
			return 0, fmt.Errorf("canonical JSON number exceeds maximum decimal exponent %d", MaxAbsDecimalExponent)
		}
		exponent = exponent*10 + int(digit-'0')
		if exponent > MaxAbsDecimalExponent {
			return 0, fmt.Errorf("canonical JSON number exceeds maximum decimal exponent %d", MaxAbsDecimalExponent)
		}
	}
	if negative {
		return -exponent, nil
	}
	return exponent, nil
}

func splitExponent(raw string) (string, string) {
	index := strings.IndexAny(raw, "eE")
	if index < 0 {
		return raw, "0"
	}
	return raw[:index], strings.TrimPrefix(raw[index+1:], "+")
}

func coefficient(mantissa string) (string, int) {
	index := strings.IndexByte(mantissa, '.')
	if index < 0 {
		return mantissa, 0
	}
	return mantissa[:index] + mantissa[index+1:], len(mantissa) - index - 1
}

func formatNumber(negative bool, digits string, exponent int) string {
	var result strings.Builder
	if negative {
		result.WriteByte('-')
	}
	result.WriteByte(digits[0])
	if len(digits) > 1 {
		result.WriteByte('.')
		result.WriteString(digits[1:])
	}
	if exponent != 0 {
		result.WriteByte('e')
		result.WriteString(strconv.Itoa(exponent))
	}
	return result.String()
}

func writeValue(output *bytes.Buffer, item value) error {
	switch item.kind {
	case kindNull:
		output.WriteString("null")
	case kindBoolean:
		output.WriteString(strconv.FormatBool(item.boolean))
	case kindString:
		return writeString(output, item.text)
	case kindNumber:
		output.WriteString(item.text)
	case kindArray:
		return writeArray(output, item.items)
	case kindObject:
		return writeObject(output, item.members)
	default:
		return fmt.Errorf("canonical JSON contains an unsupported value")
	}
	return nil
}

func writeString(output *bytes.Buffer, value string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	output.Write(encoded)
	return nil
}

func writeArray(output *bytes.Buffer, items []value) error {
	output.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			output.WriteByte(',')
		}
		if err := writeValue(output, item); err != nil {
			return err
		}
	}
	output.WriteByte(']')
	return nil
}

func writeObject(output *bytes.Buffer, members []member) error {
	output.WriteByte('{')
	for index, item := range members {
		if index > 0 {
			output.WriteByte(',')
		}
		if err := writeString(output, item.name); err != nil {
			return err
		}
		output.WriteByte(':')
		if err := writeValue(output, item.value); err != nil {
			return err
		}
	}
	output.WriteByte('}')
	return nil
}

func validateUnicodeEscapes(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		next, err := scanString(raw, index+1)
		if err != nil {
			return err
		}
		index = next - 1
	}
	return nil
}

func scanString(raw []byte, index int) (int, error) {
	for index < len(raw) {
		switch raw[index] {
		case '"':
			return index + 1, nil
		case '\\':
			next, err := scanEscape(raw, index)
			if err != nil {
				return 0, err
			}
			index = next
		default:
			index++
		}
	}
	return 0, fmt.Errorf("canonical JSON contains an unterminated string")
}

func scanEscape(raw []byte, index int) (int, error) {
	if index+1 >= len(raw) {
		return 0, fmt.Errorf("canonical JSON contains an incomplete escape")
	}
	if raw[index+1] != 'u' {
		return index + 2, nil
	}
	code, next, err := readUnicodeEscape(raw, index)
	if err != nil {
		return 0, err
	}
	if isLowSurrogate(code) {
		return 0, fmt.Errorf("canonical JSON contains a lone low surrogate")
	}
	if !isHighSurrogate(code) {
		return next, nil
	}
	return scanLowSurrogate(raw, next)
}

func scanLowSurrogate(raw []byte, index int) (int, error) {
	low, afterLow, err := readUnicodeEscape(raw, index)
	if err != nil || !isLowSurrogate(low) {
		return 0, fmt.Errorf("canonical JSON contains a lone high surrogate")
	}
	return afterLow, nil
}

func isHighSurrogate(code uint16) bool {
	return code >= 0xd800 && code <= 0xdbff
}

func isLowSurrogate(code uint16) bool {
	return code >= 0xdc00 && code <= 0xdfff
}

func readUnicodeEscape(raw []byte, index int) (uint16, int, error) {
	if index+6 > len(raw) || raw[index] != '\\' || raw[index+1] != 'u' {
		return 0, 0, fmt.Errorf("canonical JSON contains an incomplete Unicode escape")
	}
	value, err := strconv.ParseUint(string(raw[index+2:index+6]), 16, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("canonical JSON contains an invalid Unicode escape")
	}
	return uint16(value), index + 6, nil
}
