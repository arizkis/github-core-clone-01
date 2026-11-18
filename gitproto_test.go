package main

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchSingleFlight(t *testing.T) {
	c := fetchResponses{cached: make(map[fetchKey]*ForgeResponse), flights: make(map[fetchKey]*fetchFlight)}
	key := fetchKey{1, 1, "exact request bytes\x00"}
	var builds atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	responses := make([]*ForgeResponse, 64)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses[i] = c.get(key, func() *ForgeResponse {
				if builds.Add(1) == 1 {
					close(entered)
				}
				<-release
				return frameGitResponse(gitResponse(200, "application/x-git-upload-pack-result", bytes.Repeat([]byte{1, 2, 3}, 30000)))
			})
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	if builds.Load() != 1 {
		t.Fatalf("built %d times", builds.Load())
	}
	for _, r := range responses {
		if r != responses[0] || &r.Wire.KeepAlive[0] != &responses[0].Wire.KeepAlive[0] {
			t.Fatal("waiters did not share the complete immutable response")
		}
	}
	// New generations and exact-byte changes must not reuse a cached response.
	for _, k := range []fetchKey{{1, 2, key.Body}, {1, 1, key.Body + "\x00"}} {
		r := c.get(k, func() *ForgeResponse { builds.Add(1); return gitResponse(200, "x", nil) })
		if r == responses[0] {
			t.Fatal("cache key omitted generation or body bytes")
		}
	}
	if builds.Load() != 3 {
		t.Fatal("cache generation behavior")
	}
}
func TestFetchFailureDoesNotPoisonCache(t *testing.T) {
	c := fetchResponses{cached: make(map[fetchKey]*ForgeResponse), flights: make(map[fetchKey]*fetchFlight)}
	key := fetchKey{1, 1, "bad"}
	func() {
		defer func() {
			if recover() != "failed build" {
				t.Error("lost build failure")
			}
		}()
		c.get(key, func() *ForgeResponse { panic("failed build") })
	}()
	if len(c.cached) != 0 || len(c.flights) != 0 {
		t.Fatal("failed flight retained")
	}
	want := gitResponse(200, "x", nil)
	if c.get(key, func() *ForgeResponse { return want }) != want {
		t.Fatal("retry failed")
	}
}

type countingConn struct {
	writes int
	data   []byte
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.writes++
	c.data = append(c.data, b...)
	return len(b), nil
}
func (c *countingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *countingConn) Close() error                     { return nil }
func (c *countingConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *countingConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *countingConn) SetDeadline(time.Time) error      { return nil }
func (c *countingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *countingConn) SetWriteDeadline(time.Time) error { return nil }
func TestCachedFetchIsOneWrite(t *testing.T) {
	response := frameGitResponse(gitResponse(200, "application/x-git-upload-pack-result", bytes.Repeat([]byte("packet"), 20000)))
	for _, close := range []bool{false, true} {
		conn := new(countingConn)
		if err := writeResponse(conn, response, close, false); err != nil {
			t.Fatal(err)
		}
		if conn.writes != 1 {
			t.Fatal("cached fetch was copied or reframed")
		}
		want := response.Wire.KeepAlive
		if close {
			want = response.Wire.Close
		}
		if !bytes.Equal(conn.data, want) {
			t.Fatal("changed framed bytes")
		}
	}
}
