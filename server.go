package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const bodyLimit = 2_000_000
const maxLineLength = 65_536
const jsonType = "application/json; charset=utf-8"

type ForgeRequest struct {
	Method, URL, HTTPVersion string
	Headers                  map[string]string
	Body                     []byte
	BodyError                bool
}
type header struct{ Name, Value string }
type ForgeResponse struct {
	Status  int
	Headers []header
	Body    []byte
}

func sendJSON(status int, value any, requestID string, headers ...header) *ForgeResponse {
	var data []byte
	if raw, ok := value.(rawJSON); ok {
		data = raw
	} else {
		data = append(jsonBytes(value), '\n')
	}
	h := []header{{"content-type", jsonType}, {"content-length", strconv.Itoa(len(data))}, {"x-github-request-id", requestID}}
	return &ForgeResponse{status, append(h, headers...), data}
}
func fail(status int, message, requestID string) *ForgeResponse {
	return sendJSON(status, obj("message", message), requestID)
}

type ForgeServer struct {
	listener net.Listener
	wg       sync.WaitGroup
}

func startForgeServer() (*ForgeServer, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &ForgeServer{listener: listener}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); serveConnection(c) }()
		}
	}()
	return s, nil
}
func (s *ForgeServer) Close() error { err := s.listener.Close(); s.wg.Wait(); return err }
func (s *ForgeServer) Port() int    { return s.listener.Addr().(*net.TCPAddr).Port }

var requestLinePattern = regexp.MustCompile("^([!#$%&'*+\\-.^_`|~0-9A-Za-z]+) ([^\\x00-\\x20\\x7f]+) HTTP/(1\\.[01])$")

func latin1(data []byte) string {
	for _, c := range data {
		if c >= 128 {
			r := make([]rune, len(data))
			for i, b := range data {
				r[i] = rune(b)
			}
			return string(r)
		}
	}
	return string(data)
}
func latin1Bytes(s string) []byte {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		b = append(b, byte(r))
	}
	return b
}
func isHeaderToken(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) >= 2 && line[len(line)-2] == '\r' && line[len(line)-1] == '\n' {
			if len(line)-2 > maxLineLength {
				return nil, errors.New("HTTP line too long")
			}
			return line[:len(line)-2], nil
		}
		n := len(line)
		if n > 0 && line[n-1] == '\r' {
			n--
		}
		if n > maxLineLength {
			return nil, errors.New("HTTP line too long")
		}
		if err == io.EOF {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, errors.New("Incomplete HTTP line")
		}
		if err != nil && err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}
func parseHeaderLine(line []byte) (string, string, error) {
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return "", "", errors.New("Malformed HTTP line")
	}
	for _, c := range line[:colon] {
		if !isHeaderToken(c) {
			return "", "", errors.New("Malformed HTTP line")
		}
	}
	value := bytes.Trim(line[colon+1:], " \t")
	for _, c := range value {
		if c < 32 && c != 9 || c == 127 {
			return "", "", errors.New("Malformed HTTP line")
		}
	}
	return strings.ToLower(string(line[:colon])), latin1(value), nil
}
func readRequest(r *bufio.Reader, c net.Conn) (*ForgeRequest, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	m := requestLinePattern.FindStringSubmatch(latin1(line))
	if m == nil {
		return nil, errors.New("Malformed HTTP line")
	}
	req := &ForgeRequest{Method: m[1], URL: m[2], HTTPVersion: m[3], Headers: make(map[string]string)}
	for {
		line, err = readLine(r)
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			break
		}
		k, v, e := parseHeaderLine(line)
		if e != nil {
			return nil, e
		}
		if old, ok := req.Headers[k]; ok {
			req.Headers[k] = old + ", " + v
		} else {
			req.Headers[k] = v
		}
	}
	if hasToken(req.Headers["expect"], "100-continue") {
		if _, err = c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return nil, err
		}
	}
	err = readBody(r, req)
	return req, err
}
func hasToken(value, token string) bool {
	for _, s := range strings.Split(strings.ToLower(value), ",") {
		if strings.TrimFunc(s, jsSpace) == token {
			return true
		}
	}
	return false
}
func readBody(r *bufio.Reader, req *ForgeRequest) error {
	te, hasTE := req.Headers["transfer-encoding"]
	cl, hasCL := req.Headers["content-length"]
	if !hasTE && (!hasCL || cl == "0") {
		return nil
	}
	var body []byte
	consume := func(n uint64) error {
		if n > uint64(bodyLimit-len(body)) {
			req.BodyError = true
			body = nil
		}
		if req.BodyError {
			_, err := io.CopyN(io.Discard, r, int64(n))
			return err
		}
		start := len(body)
		body = append(body, make([]byte, int(n))...)
		_, err := io.ReadFull(r, body[start:])
		return err
	}
	if hasTE {
		codings := strings.Split(strings.ToLower(te), ",")
		for i := range codings {
			codings[i] = strings.TrimFunc(codings[i], jsSpace)
		}
		if hasCL || len(codings) == 0 || codings[len(codings)-1] != "chunked" {
			return errors.New("Invalid request framing")
		}
		for i, coding := range codings {
			if coding == "" || coding == "chunked" && i != len(codings)-1 {
				return errors.New("Invalid request framing")
			}
			for _, c := range []byte(coding) {
				if !isHeaderToken(c) {
					return errors.New("Invalid request framing")
				}
			}
		}
		for {
			line, err := readLine(r)
			if err != nil {
				return err
			}
			raw, _, _ := strings.Cut(string(line), ";")
			if raw == "" {
				return errors.New("Invalid chunk size")
			}
			for _, c := range raw {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
					return errors.New("Invalid chunk size")
				}
			}
			n, e := strconv.ParseUint(raw, 16, 64)
			if e != nil || n > 1<<53-1 {
				return errors.New("Invalid chunk size")
			}
			if n == 0 {
				for {
					trailer, e := readLine(r)
					if e != nil {
						return e
					}
					if len(trailer) == 0 {
						break
					}
					if _, _, e = parseHeaderLine(trailer); e != nil {
						return e
					}
				}
				break
			}
			if e = consume(n); e != nil {
				return e
			}
			var end [2]byte
			if _, e = io.ReadFull(r, end[:]); e != nil {
				return e
			}
			if end != [2]byte{'\r', '\n'} {
				return errors.New("Invalid chunk terminator")
			}
		}
	} else {
		if cl == "" {
			return errors.New("Invalid content length")
		}
		for _, c := range cl {
			if c < '0' || c > '9' {
				return errors.New("Invalid content length")
			}
		}
		n, e := strconv.ParseUint(cl, 10, 64)
		if e != nil || n > 1<<53-1 {
			return errors.New("Invalid content length")
		}
		if e = consume(n); e != nil {
			return e
		}
	}
	req.Body = body
	return nil
}
func serveConnection(c net.Conn) {
	defer c.Close()
	defer func() { _ = recover() }() // Source transport closes silently on parser/dispatcher errors.
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	r := bufio.NewReaderSize(c, 16_384)
	for {
		req, err := readRequest(r, c)
		if err != nil {
			return
		}
		close := hasToken(req.Headers["connection"], "close") || req.HTTPVersion == "1.0" && !hasToken(req.Headers["connection"], "keep-alive")
		res := dispatch(req)
		if res == nil {
			return
		}
		if err = writeResponse(c, res, close, req.Method == "HEAD"); err != nil {
			return
		}
		if close {
			return
		}
	}
}

var reasons = map[int]string{200: "OK", 201: "Created", 202: "Accepted", 204: "No Content", 304: "Not Modified", 400: "Bad Request", 401: "Unauthorized", 404: "Not Found", 405: "Method Not Allowed", 409: "Conflict", 413: "Payload Too Large", 422: "Unprocessable Entity", 500: "Internal Server Error"}

func writeResponse(c net.Conn, res *ForgeResponse, close, headOnly bool) error {
	reason := reasons[res.Status]
	if reason == "" {
		reason = "Unknown"
	}
	b := make([]byte, 0, 256)
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(res.Status), 10)
	b = append(b, ' ')
	b = append(b, reason...)
	b = append(b, '\r', '\n')
	noBody := res.Status == 204 || res.Status == 304
	data := res.Body
	if noBody {
		data = nil
	}
	hasLength, hasConnection := false, false
	for _, h := range res.Headers {
		lower := strings.ToLower(h.Name)
		if noBody && (lower == "content-length" || lower == "transfer-encoding") {
			continue
		}
		if lower == "content-length" {
			hasLength = true
		}
		if lower == "connection" {
			hasConnection = true
			if close {
				b = append(b, latin1Bytes(h.Name+": close\r\n")...)
			}
			continue
		}
		b = append(b, latin1Bytes(h.Name+": "+h.Value+"\r\n")...)
	}
	if !noBody && !hasLength {
		b = append(b, "Content-Length: "...)
		b = strconv.AppendInt(b, int64(len(data)), 10)
		b = append(b, '\r', '\n')
	}
	if close && !hasConnection {
		b = append(b, "Connection: close\r\n"...)
	}
	b = append(b, '\r', '\n')
	if headOnly || len(data) == 0 {
		_, err := c.Write(b)
		return err
	}
	if len(data) < 16_384 {
		b = append(b, data...)
		_, err := c.Write(b)
		return err
	}
	buffers := net.Buffers{b, data}
	_, err := buffers.WriteTo(c)
	return err
}
