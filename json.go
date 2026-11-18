package main

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Response objects are ordered, including nested objects. Input maps are used
// only for property lookup; they are never marshaled onto the wire.
type field struct {
	name  string
	value any
}
type object []field

func obj(pairs ...any) object {
	o := make(object, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		o = append(o, field{pairs[i].(string), pairs[i+1]})
	}
	return o
}

type rawJSON []byte
type jsUndefined struct{}
type jsFunction struct{}

var undefinedValue any = jsUndefined{}

func isNullish(v any) bool {
	if v == nil {
		return true
	}
	_, ok := v.(jsUndefined)
	return ok
}
func isOmitted(v any) bool {
	switch v.(type) {
	case jsUndefined, jsFunction:
		return true
	}
	return false
}

type inputObject struct {
	values body
	keys   []string
}

func newInputObject() *inputObject { return &inputObject{values: make(body)} }
func (o *inputObject) set(k string, v any) {
	if _, ok := o.values[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.values[k] = v
}
func (o *inputObject) ordered() object {
	out := make(object, 0, len(o.keys))
	for _, k := range o.keys {
		out = append(out, field{k, o.values[k]})
	}
	return out
}
func setObjectField(o object, key string, value any) object {
	if key == "__proto__" {
		return o
	}
	for i := range o {
		if o[i].name == key {
			o[i].value = value
			return o
		}
	}
	return append(o, field{key, value})
}
func objectIndex(key string) (uint64, bool) {
	if key == "" || key[0] < '0' || key[0] > '9' || len(key) > 10 || len(key) > 1 && key[0] == '0' {
		return 0, false
	}
	n, err := strconv.ParseUint(key, 10, 32)
	return n, err == nil && n < 1<<32-1 && strconv.FormatUint(n, 10) == key
}
func jsonFieldOrder(o object) object {
	found := false
	for _, f := range o {
		if _, ok := objectIndex(f.name); ok {
			found = true
			break
		}
	}
	if !found {
		return o
	}
	out := slices.Clone(o)
	slices.SortStableFunc(out, func(a, b field) int {
		an, ai := objectIndex(a.name)
		bn, bi := objectIndex(b.name)
		if ai && bi {
			if an < bn {
				return -1
			}
			if an > bn {
				return 1
			}
		} else if ai {
			return -1
		} else if bi {
			return 1
		}
		return 0
	})
	return out
}

func jsonBytes(value any) []byte { return appendJSON(make([]byte, 0, 256), value) }
func appendJSON(b []byte, value any) []byte {
	switch v := value.(type) {
	case nil:
		return append(b, "null"...)
	case jsUndefined, jsFunction:
		return append(b, "null"...)
	case *inputObject:
		return appendJSON(b, v.ordered())
	case graphValue:
		return appendJSON(b, v.serialized())
	case rawJSON:
		return append(b, v...)
	case string:
		return appendJSONString(b, v)
	case bool:
		return strconv.AppendBool(b, v)
	case int:
		return strconv.AppendInt(b, int64(v), 10)
	case int64:
		return strconv.AppendInt(b, v, 10)
	case uint64:
		return strconv.AppendUint(b, v, 10)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return append(b, "null"...)
		}
		return append(b, jsNumberString(v)...)
	case object:
		b = append(b, '{')
		first := true
		for _, f := range jsonFieldOrder(v) {
			if isOmitted(f.value) {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = appendJSONString(b, f.name)
			b = append(b, ':')
			b = appendJSON(b, f.value)
		}
		return append(b, '}')
	case []any:
		b = append(b, '[')
		for i, x := range v {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSON(b, x)
		}
		return append(b, ']')
	case []string:
		b = append(b, '[')
		for i, x := range v {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSONString(b, x)
		}
		return append(b, ']')
	default:
		panic(fmt.Sprintf("unsupported response JSON type %T", value))
	}
}

const hexDigits = "0123456789abcdef"

func jsUTF16(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == 0xed && i+2 < len(s) && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2]&0xc0 == 0x80 {
			out = append(out, uint16(s[i]&15)<<12|uint16(s[i+1]&63)<<6|uint16(s[i+2]&63))
			i += 3
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r > 0xffff {
			a, b := utf16.EncodeRune(r)
			out = append(out, uint16(a), uint16(b))
		} else {
			out = append(out, uint16(r))
		}
	}
	return out
}

func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
			i++
		case '\b':
			b = append(b, '\\', 'b')
			i++
		case '\f':
			b = append(b, '\\', 'f')
			i++
		case '\n':
			b = append(b, '\\', 'n')
			i++
		case '\r':
			b = append(b, '\\', 'r')
			i++
		case '\t':
			b = append(b, '\\', 't')
			i++
		default:
			if c < 32 {
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&15])
				i++
				continue
			}
			// WTF-8 keeps lone JavaScript UTF-16 surrogates lossless internally.
			if c == 0xed && i+2 < len(s) && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2]&0xc0 == 0x80 {
				u := uint16(c&15)<<12 | uint16(s[i+1]&63)<<6 | uint16(s[i+2]&63)
				b = append(b, '\\', 'u', hexDigits[u>>12], hexDigits[u>>8&15], hexDigits[u>>4&15], hexDigits[u&15])
				i += 3
			} else {
				b = append(b, c)
				i++
			}
		}
	}
	return append(b, '"')
}
func jsNumberString(v float64) string {
	if v == 0 {
		return "0"
	}
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 1) {
		return "Infinity"
	}
	if math.IsInf(v, -1) {
		return "-Infinity"
	}
	fmtByte := byte('f')
	if math.Abs(v) < 1e-6 || math.Abs(v) >= 1e21 {
		fmtByte = 'e'
	}
	s := strconv.FormatFloat(v, fmtByte, -1, 64)
	if at := strings.IndexByte(s, 'e'); at >= 0 {
		sign := s[at+1 : at+2]
		exponent := strings.TrimLeft(s[at+2:], "0")
		if exponent == "" {
			exponent = "0"
		}
		s = s[:at+1] + sign + exponent
	}
	return s
}
func jsString(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case jsUndefined:
		return "undefined"
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case float64:
		return jsNumberString(v)
	case []any:
		a := make([]string, len(v))
		for i, x := range v {
			if !isNullish(x) {
				a[i] = jsString(x)
			}
		}
		return strings.Join(a, ",")
	case body:
		if _, exists := v["toString"]; exists {
			panic("Cannot convert object to primitive value")
		}
		return "[object Object]"
	case *inputObject:
		return jsString(v.values)
	default:
		return "[object Object]"
	}
}
func jsTruthy(v any) bool {
	if isNullish(v) {
		return false
	}
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	}
	return true
}
func jsNumber(v any) float64 {
	if _, ok := v.(jsUndefined); ok {
		return math.NaN()
	}
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	}
	s := strings.TrimFunc(jsString(v), jsSpace)
	if s == "" {
		return 0
	}
	if s == "Infinity" || s == "+Infinity" {
		return math.Inf(1)
	}
	if s == "-Infinity" {
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			n, e := strconv.ParseUint(s[2:], base, 64)
			if e == nil {
				return float64(n)
			}
			return math.NaN()
		}
	}
	if s == "NaN" || strings.ContainsAny(s, "xXpPiInN_") {
		return math.NaN()
	}
	n, e := strconv.ParseFloat(s, 64)
	if e != nil && !math.IsInf(n, 0) {
		return math.NaN()
	}
	return n
}

type body map[string]any

func (b body) str(key, fallback string) string {
	if isNullish(b[key]) {
		return fallback
	}
	return jsString(b[key])
}
func (b body) stringValue(key string) (string, bool) { v, ok := b[key].(string); return v, ok }

// A small JSON parser reproduces JSON.parse coercions and V8 diagnostics,
// including positions measured in UTF-16 and lone surrogate escape handling.
type jsonParser struct {
	data []byte
	at   int
}

func parseJSON(data []byte) any {
	if !utf8.Valid(data) {
		data = []byte(utf8Text(data))
	}
	p := jsonParser{data: data}
	p.ws()
	v := p.value()
	p.ws()
	if p.at < len(data) {
		p.fail("Unexpected non-whitespace character after JSON")
	}
	return v
}
func parseBody(data []byte) body {
	if len(data) == 0 {
		return body{}
	}
	v := parseJSON(data)
	if b, ok := v.(*inputObject); ok {
		return b.values
	}
	return body{}
}
func (p *jsonParser) ws() {
	for p.at < len(p.data) && (p.data[p.at] == ' ' || p.data[p.at] == '\t' || p.data[p.at] == '\n' || p.data[p.at] == '\r') {
		p.at++
	}
}
func (p *jsonParser) fail(message string) {
	pos, line, col := 0, 1, 1
	for _, r := range string(p.data[:p.at]) {
		n := 1
		if r > 0xffff {
			n = 2
		}
		pos += n
		if r == '\n' {
			line++
			col = 1
		} else {
			col += n
		}
	}
	suffix := " in JSON"
	if strings.HasSuffix(message, "JSON") {
		suffix = ""
	}
	panic(fmt.Sprintf("%s%s at position %d (line %d column %d)", message, suffix, pos, line, col))
}
func (p *jsonParser) unexpected() {
	if p.at >= len(p.data) {
		panic("Unexpected end of JSON input")
	}
	s := string(p.data)
	if s == "undefined" || s == "NaN" || s == "Infinity" || s == "[object Object]" {
		panic(fmt.Sprintf("\"%s\" is not valid JSON", s))
	}
	units := utf16.Encode([]rune(s))
	pos := len(utf16.Encode([]rune(s[:p.at])))
	if len(units) <= 20 {
		r, _ := utf8.DecodeRune(p.data[p.at:])
		panic(fmt.Sprintf("Unexpected token '%c', \"%s\" is not valid JSON", r, s))
	}
	start, end := max(0, pos-10), min(len(units), pos+10)
	preview := string(utf16.Decode(units[start:end]))
	left, right := "", ""
	if pos >= 10 {
		left = "..."
	}
	if end < len(units) {
		right = "..."
	}
	r, _ := utf8.DecodeRune(p.data[p.at:])
	panic(fmt.Sprintf("Unexpected token '%c', %s\"%s\"%s is not valid JSON", r, left, preview, right))
}
func (p *jsonParser) value() any {
	p.ws()
	if p.at >= len(p.data) {
		p.unexpected()
	}
	switch p.data[p.at] {
	case '{':
		p.at++
		p.ws()
		out := newInputObject()
		if p.at < len(p.data) && p.data[p.at] == '}' {
			p.at++
			return out
		}
		if p.at >= len(p.data) || p.data[p.at] != '"' {
			p.fail("Expected property name or '}'")
		}
		for {
			key := p.string()
			p.ws()
			if p.at >= len(p.data) || p.data[p.at] != ':' {
				p.fail("Expected ':' after property name")
			}
			p.at++
			out.set(key, p.value())
			p.ws()
			if p.at < len(p.data) && p.data[p.at] == '}' {
				p.at++
				return out
			}
			if p.at >= len(p.data) || p.data[p.at] != ',' {
				p.fail("Expected ',' or '}' after property value")
			}
			p.at++
			p.ws()
			if p.at >= len(p.data) || p.data[p.at] != '"' {
				p.fail("Expected double-quoted property name")
			}
		}
	case '[':
		p.at++
		p.ws()
		out := make([]any, 0)
		if p.at < len(p.data) && p.data[p.at] == ']' {
			p.at++
			return out
		}
		for {
			out = append(out, p.value())
			p.ws()
			if p.at < len(p.data) && p.data[p.at] == ']' {
				p.at++
				return out
			}
			if p.at >= len(p.data) || p.data[p.at] != ',' {
				p.fail("Expected ',' or ']' after array element")
			}
			p.at++
		}
	case '"':
		return p.string()
	case 't', 'f', 'n':
		word := "true"
		var v any = true
		if p.data[p.at] == 'f' {
			word = "false"
			v = false
		}
		if p.data[p.at] == 'n' {
			word = "null"
			v = nil
		}
		for i := 0; i < len(word); i++ {
			if p.at >= len(p.data) || p.data[p.at] != word[i] {
				p.unexpected()
			}
			p.at++
		}
		return v
	default:
		if p.data[p.at] == '-' || p.data[p.at] >= '0' && p.data[p.at] <= '9' {
			return p.number()
		}
		p.unexpected()
	}
	return nil
}
func (p *jsonParser) number() float64 {
	start := p.at
	if p.data[p.at] == '-' {
		p.at++
		if p.at >= len(p.data) || p.data[p.at] < '0' || p.data[p.at] > '9' {
			p.fail("No number after minus sign")
		}
	}
	if p.data[p.at] == '0' {
		p.at++
		if p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
			p.fail("Unexpected number")
		}
	} else {
		for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
			p.at++
		}
	}
	if p.at < len(p.data) && p.data[p.at] == '.' {
		p.at++
		if p.at >= len(p.data) || p.data[p.at] < '0' || p.data[p.at] > '9' {
			p.fail("Unterminated fractional number")
		}
		for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
			p.at++
		}
	}
	if p.at < len(p.data) && (p.data[p.at] == 'e' || p.data[p.at] == 'E') {
		p.at++
		if p.at < len(p.data) && (p.data[p.at] == '+' || p.data[p.at] == '-') {
			p.at++
		}
		if p.at >= len(p.data) || p.data[p.at] < '0' || p.data[p.at] > '9' {
			p.fail("Exponent part is missing a number")
		}
		for p.at < len(p.data) && p.data[p.at] >= '0' && p.data[p.at] <= '9' {
			p.at++
		}
	}
	n, _ := strconv.ParseFloat(string(p.data[start:p.at]), 64)
	return n
}
func appendWTF8(b []byte, r uint16) []byte {
	return append(b, byte(0xe0|r>>12), byte(0x80|r>>6&63), byte(0x80|r&63))
}
func (p *jsonParser) string() string {
	p.at++
	start := p.at
	for p.at < len(p.data) && p.data[p.at] != '"' && p.data[p.at] != '\\' && p.data[p.at] >= 32 {
		p.at++
	}
	if p.at < len(p.data) && p.data[p.at] == '"' {
		s := string(p.data[start:p.at])
		p.at++
		return s
	}
	out := append([]byte(nil), p.data[start:p.at]...)
	for p.at < len(p.data) {
		c := p.data[p.at]
		if c == '"' {
			p.at++
			return string(out)
		}
		if c < 32 {
			p.fail("Bad control character in string literal")
		}
		if c != '\\' {
			out = append(out, c)
			p.at++
			continue
		}
		p.at++
		if p.at >= len(p.data) {
			p.unexpected()
		}
		c = p.data[p.at]
		p.at++
		switch c {
		case '"', '\\', '/':
			out = append(out, c)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			u := p.hex4()
			if u >= 0xd800 && u <= 0xdbff && p.at+6 <= len(p.data) && bytes.Equal(p.data[p.at:p.at+2], []byte(`\u`)) {
				lo, e := strconv.ParseUint(string(p.data[p.at+2:p.at+6]), 16, 16)
				if e == nil && lo >= 0xdc00 && lo <= 0xdfff {
					p.at += 6
					out = utf8.AppendRune(out, utf16.DecodeRune(rune(u), rune(lo)))
					continue
				}
			}
			if u >= 0xd800 && u <= 0xdfff {
				out = appendWTF8(out, u)
			} else {
				out = utf8.AppendRune(out, rune(u))
			}
		default:
			p.at--
			p.fail("Bad escaped character")
		}
	}
	p.fail("Unterminated string")
	return ""
}
func (p *jsonParser) hex4() uint16 {
	var u uint16
	for range 4 {
		if p.at >= len(p.data) {
			p.fail("Bad Unicode escape")
		}
		c := p.data[p.at]
		var n byte
		switch {
		case c >= '0' && c <= '9':
			n = c - '0'
		case c >= 'a' && c <= 'f':
			n = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			n = c - 'A' + 10
		default:
			p.fail("Bad Unicode escape")
		}
		u = u*16 + uint16(n)
		p.at++
	}
	return u
}
