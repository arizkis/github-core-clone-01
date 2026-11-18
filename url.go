package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

type queryPair struct{ Key, Value string }
type forgeURL struct {
	Origin, Pathname, Search string
	Query                    []queryPair
}

func (u *forgeURL) Get(key string, fallback string) string {
	for _, p := range u.Query {
		if p.Key == key {
			return p.Value
		}
	}
	return fallback
}
func (u *forgeURL) Has(key string) bool {
	for _, p := range u.Query {
		if p.Key == key {
			return true
		}
	}
	return false
}
func querySet(query []queryPair, key, value string) []queryPair {
	out := make([]queryPair, 0, len(query)+1)
	found := false
	for _, p := range query {
		if p.Key == key {
			if !found {
				out = append(out, queryPair{key, value})
				found = true
			}
		} else {
			out = append(out, p)
		}
	}
	if !found {
		out = append(out, queryPair{key, value})
	}
	return out
}
func queryEscape(s string) string {
	b := make([]byte, 0, len(s))
	for _, c := range []byte(jsUTF8(s)) {
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '*' || c == '-' || c == '.' || c == '_':
			b = append(b, c)
		case c == ' ':
			b = append(b, '+')
		default:
			b = append(b, '%', "0123456789ABCDEF"[c>>4], "0123456789ABCDEF"[c&15])
		}
	}
	return string(b)
}
func encodeQuery(query []queryPair) string {
	var b strings.Builder
	for i, p := range query {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(queryEscape(p.Key))
		b.WriteByte('=')
		b.WriteString(queryEscape(p.Value))
	}
	return b.String()
}
func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
func queryUnescape(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '+' {
			b = append(b, ' ')
			continue
		}
		if s[i] == '%' && i+2 < len(s) {
			a, ok := unhex(s[i+1])
			c, ok2 := unhex(s[i+2])
			if ok && ok2 {
				b = append(b, a*16+c)
				i += 2
				continue
			}
		}
		b = append(b, s[i])
	}
	return utf8Text(b)
}
func decodeSegment(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	v, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(v) {
		panic("URI malformed")
	}
	return v
}
func jsUTF8(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0xed && i+2 < len(s) && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2]&0xc0 == 0x80 {
			b.WriteRune('\ufffd')
			i += 3
		} else {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}
func parseForgeURL(raw, host string) (u *forgeURL, valid bool) {
	// WHATWG special URLs fold backslashes, strip ASCII tabs/newlines, percent
	// encode path text, and remove dot segments before routing.
	raw = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, strings.Trim(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "))
	baseOrigin, ok := urlOrigin("http", host)
	if !ok {
		return nil, false
	}
	origin := baseOrigin
	beforeQuery, q, hasQ := strings.Cut(raw, "?")
	if at := strings.IndexByte(beforeQuery, '#'); at >= 0 {
		beforeQuery = beforeQuery[:at]
		hasQ = false
		q = ""
	} else if at := strings.IndexByte(q, '#'); at >= 0 {
		q = q[:at]
	}
	path := beforeQuery
	scheme, explicit := "http", false
	if at := strings.IndexByte(path, ':'); at > 0 && validScheme(path[:at]) {
		scheme = strings.ToLower(path[:at])
		path = path[at+1:]
		explicit = true
	}
	special := scheme == "http" || scheme == "https" || scheme == "ws" || scheme == "wss" || scheme == "ftp" || scheme == "file"
	if special {
		path = strings.ReplaceAll(path, "\\", "/")
	}
	opaque := false
	if explicit && scheme != "http" {
		if !special {
			origin = "null"
			if strings.HasPrefix(path, "//") {
				authority, remaining, found := strings.Cut(path[2:], "/")
				if strings.ContainsAny(authority, " <>^|\\") {
					return nil, false
				}
				path = ""
				if found {
					path = "/" + remaining
				}
			} else {
				opaque = !strings.HasPrefix(path, "/")
			}
		} else if scheme == "file" {
			origin = "null"
			if strings.HasPrefix(path, "//") {
				authority, remaining, found := strings.Cut(path[2:], "/")
				if authority != "" && authority != "localhost" {
					if _, ok = urlOrigin("http", authority); !ok {
						return nil, false
					}
				}
				path = "/"
				if found {
					path += remaining
				}
			}
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
		} else {
			rest := strings.TrimLeft(path, "/")
			authority, remaining, found := strings.Cut(rest, "/")
			path = "/"
			if found {
				path += remaining
			}
			origin, ok = urlOrigin(scheme, authority)
			if !ok {
				return nil, false
			}
		}
	} else if strings.HasPrefix(path, "//") {
		rest := strings.TrimLeft(path, "/")
		authority, remaining, found := strings.Cut(rest, "/")
		path = "/"
		if found {
			path += remaining
		}
		origin, ok = urlOrigin("http", authority)
		if !ok {
			return nil, false
		}
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if path == "" && special {
		path = "/"
	}
	bits := strings.Split(path, "/")
	normalized := make([]string, 0, len(bits))
	for i, bit := range bits {
		if opaque {
			normalized = append(normalized, bit)
			continue
		}
		dot := strings.ReplaceAll(strings.ToLower(bit), "%2e", ".")
		if i > 0 && dot == "." {
			if i == len(bits)-1 {
				normalized = append(normalized, "")
			}
			continue
		}
		if i > 0 && dot == ".." {
			if len(normalized) > 1 {
				normalized = normalized[:len(normalized)-1]
			}
			if i == len(bits)-1 {
				normalized = append(normalized, "")
			}
			continue
		}
		normalized = append(normalized, bit)
	}
	path = encodeURLText(strings.Join(normalized, "/"), false)
	u = &forgeURL{Origin: origin, Pathname: path, Query: make([]queryPair, 0)}
	if hasQ && q != "" {
		encoded := encodeURLText(q, true)
		if !special {
			encoded = strings.ReplaceAll(encoded, "%27", "'")
		}
		u.Search = "?" + encoded
	}
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		u.Query = append(u.Query, queryPair{queryUnescape(k), queryUnescape(v)})
	}
	return u, true
}
func validScheme(s string) bool {
	for i, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			continue
		}
		return false
	}
	return s != ""
}
func urlOrigin(scheme, authority string) (string, bool) {
	// Parse just the authority, preserving WHATWG's default-port elision.
	authority = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, authority)
	if at := strings.IndexAny(authority, "/\\?#"); at >= 0 {
		authority = authority[:at]
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	decoded, e := url.PathUnescape(authority)
	if e != nil {
		return "", false
	}
	authority = decoded
	if authority == "" || strings.ContainsAny(authority, " \x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f<>^|%") {
		return "", false
	}
	hostname, port := authority, ""
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", false
		}
		ip := net.ParseIP(authority[1:end])
		if ip == nil {
			return "", false
		}
		hostname = "[" + formatIPv6(ip.To16()) + "]"
		if end+1 < len(authority) {
			if authority[end+1] != ':' {
				return "", false
			}
			port = authority[end+2:]
		}
	} else if i := strings.LastIndexByte(authority, ':'); i >= 0 {
		hostname = authority[:i]
		port = authority[i+1:]
		if strings.Contains(hostname, ":") {
			return "", false
		}
	}
	if hostname == "" {
		return "", false
	}
	hostname = strings.ToLower(hostname)
	if !strings.HasPrefix(hostname, "[") {
		if strings.ContainsAny(hostname, "[]") {
			return "", false
		}
		if strings.IndexFunc(hostname, func(r rune) bool { return r >= 128 }) >= 0 || strings.Contains(hostname, "xn--") {
			var valid bool
			hostname, valid = idnaHost(hostname)
			if !valid {
				return "", false
			}
		}
		var valid bool
		hostname, valid = canonicalIPv4(hostname)
		if !valid {
			return "", false
		}
	}
	if port != "" {
		for _, r := range port {
			if r < '0' || r > '9' {
				return "", false
			}
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", false
		}
		port = strconv.Itoa(int(n))
		if (scheme == "http" || scheme == "ws") && n == 80 || (scheme == "https" || scheme == "wss") && n == 443 || scheme == "ftp" && n == 21 {
			port = ""
		}
	}
	if port != "" {
		hostname += ":" + port
	}
	return scheme + "://" + hostname, true
}

var lastIDNAInput, lastIDNAOutput string
var lastIDNAValid bool

func idnaHost(host string) (string, bool) {
	if host == lastIDNAInput {
		return lastIDNAOutput, lastIDNAValid
	}
	// net/http applies its standard-library UTS-46 lookup profile before
	// invoking DialContext. Intercept that call and return an error: this is
	// entirely local, performs no DNS lookup, and never opens a connection.
	// A mapped-away soft hyphen also makes the library validate ASCII A-labels.
	input := "\u00ad" + host
	canonical := make(chan string, 1)
	t := http.Transport{DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
		select {
		case canonical <- address:
		default:
		}
		return nil, errors.New("local IDNA normalization")
	}}
	req := &http.Request{Method: "GET", URL: &url.URL{Scheme: "http", Host: input, Path: "/"}, Header: make(http.Header)}
	_, _ = t.RoundTrip(req)
	t.CloseIdleConnections()
	result, valid := "", false
	select {
	case address := <-canonical:
		var err error
		result, _, err = net.SplitHostPort(address)
		valid = err == nil && result != "" && strings.IndexFunc(result, func(r rune) bool { return r >= 128 }) < 0
	default:
	}
	lastIDNAInput, lastIDNAOutput, lastIDNAValid = host, result, valid
	return result, valid
}
func formatIPv6(ip net.IP) string {
	parts := make([]string, 8)
	values := make([]uint16, 8)
	for i := range 8 {
		values[i] = uint16(ip[2*i])<<8 | uint16(ip[2*i+1])
		parts[i] = strconv.FormatUint(uint64(values[i]), 16)
	}
	bestStart, bestSize := -1, 1
	for i := 0; i < 8; {
		if values[i] != 0 {
			i++
			continue
		}
		end := i
		for end < 8 && values[end] == 0 {
			end++
		}
		if end-i > bestSize {
			bestStart, bestSize = i, end-i
		}
		i = end
	}
	if bestStart >= 0 {
		return strings.Join(parts[:bestStart], ":") + "::" + strings.Join(parts[bestStart+bestSize:], ":")
	}
	return strings.Join(parts, ":")
}
func ipv4Number(s string) (uint64, bool) {
	base := 10
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
		base = 16
	} else if len(s) >= 2 && s[0] == '0' {
		s = s[1:]
		base = 8
	}
	if s == "" {
		return 0, true
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' && int(c-'0') < base || base == 16 && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F')) {
			return 0, false
		}
	}
	n, e := strconv.ParseUint(s, base, 64)
	return n, e == nil
}
func canonicalIPv4(host string) (string, bool) {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return host, true
	}
	last := parts[len(parts)-1]
	_, number := ipv4Number(last)
	if !number {
		number = last != ""
		for _, c := range last {
			if c < '0' || c > '9' {
				number = false
				break
			}
		}
	}
	if !number {
		return host, true
	}
	if len(parts) > 4 {
		return "", false
	}
	var result uint64
	for i, p := range parts {
		if p == "" {
			return "", false
		}
		n, ok := ipv4Number(p)
		if !ok {
			return "", false
		}
		if i < len(parts)-1 {
			if n > 255 {
				return "", false
			}
			result += n << uint(24-i*8)
		} else {
			if n >= uint64(1)<<uint(8*(5-len(parts))) {
				return "", false
			}
			result += n
		}
	}
	return fmtIPv4(result), true
}
func fmtIPv4(n uint64) string {
	return strconv.FormatUint(n>>24, 10) + "." + strconv.FormatUint(n>>16&255, 10) + "." + strconv.FormatUint(n>>8&255, 10) + "." + strconv.FormatUint(n&255, 10)
}
func encodeURLText(s string, query bool) string {
	b := make([]byte, 0, len(s))
	for _, c := range []byte(jsUTF8(s)) {
		escape := c <= 32 || c >= 127 || c == '"' || c == '<' || c == '>' || !query && (c == '`' || c == '{' || c == '}') || query && c == '\''
		if escape {
			b = append(b, '%', "0123456789ABCDEF"[c>>4], "0123456789ABCDEF"[c&15])
		} else {
			b = append(b, c)
		}
	}
	return string(b)
}
