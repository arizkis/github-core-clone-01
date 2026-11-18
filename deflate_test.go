package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestZlibByteParity(t *testing.T) {
	fixtures := [][]byte{nil, []byte("hello\n"), []byte("abcabcabcabc"), bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("abc"), 200), []byte("tree 0123456789abcdef0123456789abcdef01234567\nauthor Makefun Forge <forge@example.invalid> 1735689600 +0000\ncommitter Makefun Forge <forge@example.invalid> 1735689600 +0000\n\nupdate file\n")}
	rng := rand.New(rand.NewSource(33944905))
	for _, n := range []int{1, 2, 3, 10, 100, 1000, 16383, 16384, 32768, 65274, 65536, 100000, 300000} {
		for _, alphabet := range []int{2, 16, 256} {
			data := make([]byte, n)
			for i := range data {
				data[i] = byte(rng.Intn(alphabet))
			}
			fixtures = append(fixtures, data)
		}
	}
	for range 10 {
		data := make([]byte, rng.Intn(3000))
		for i := range data {
			data[i] = byte('a' + rng.Intn(26))
		}
		fixtures = append(fixtures, data)
	}
	encoded := make([]string, len(fixtures))
	for i, data := range fixtures {
		encoded[i] = base64.StdEncoding.EncodeToString(data)
	}
	input, _ := json.Marshal(encoded)
	cmd := exec.Command("node", "--input-type=module", "-e", `import{deflateSync}from'node:zlib';let s='';for await(const c of process.stdin)s+=c;process.stdout.write(JSON.stringify(JSON.parse(s).map(x=>deflateSync(Buffer.from(x,'base64')).toString('hex'))));`)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var expected []string
	if err = json.Unmarshal(output, &expected); err != nil {
		t.Fatal(err)
	}
	for i, data := range fixtures {
		got := deflateObject(data)
		want, _ := hex.DecodeString(expected[i])
		if !bytes.Equal(got, want) {
			at := 0
			for at < min(len(got), len(want)) && got[at] == want[at] {
				at++
			}
			t.Errorf("fixture %d length %d: compressed %d/%d first difference %d; got %x want %x", i, len(data), len(got), len(want), at, got[:min(len(got), 40)], want[:min(len(want), 40)])
			a, b := traceTokens(got), traceTokens(want)
			raw := 0
			for j := 0; j < min(len(a), len(b)); j++ {
				if a[j] != b[j] {
					t.Logf("token %d raw offset %d got %+v want %+v; next got %+v want %+v", j, raw, a[j], b[j], a[j:min(len(a), j+5)], b[j:min(len(b), j+5)])
					break
				}
				raw += max(1, a[j].length)
			}
		}
		r, err := zlib.NewReader(bytes.NewReader(got))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(decoded, data) {
			t.Fatalf("fixture %d invalid compression: %v", i, err)
		}
		var scratch inflateScratch
		inflated, consumed := scratch.decode(want, float64(len(data)), nil)
		if consumed != len(want) || !bytes.Equal(inflated, data) {
			t.Fatalf("fixture %d custom inflate changed bytes or stream boundary", i)
		}
	}
}

func traceTokens(data []byte) []deflateToken {
	r := inflateBits{data: data, at: 2}
	s := inflateScratch{}
	var out []deflateToken
	for final := false; !final; {
		final = r.get(1) != 0
		kind := r.get(2)
		if kind == 0 {
			r.align()
			n := r.get(16)
			r.get(16)
			for range n {
				out = append(out, deflateToken{literal: int(r.get(8))})
			}
			continue
		}
		if kind == 1 {
			for i := range 288 {
				s.lengths[i] = uint8(fixedLiteralLengths[i])
			}
			s.lit.build(s.lengths[:288], "literal/length")
			for i := range 32 {
				s.lengths[i] = 5
			}
			s.dist.build(s.lengths[:32], "distance")
		} else {
			nlit, ndist, ncode := int(r.get(5))+257, int(r.get(5))+1, int(r.get(4))+4
			clear(s.lengths[:])
			for i := 0; i < ncode; i++ {
				s.lengths[deflateCodeOrder[i]] = uint8(r.get(3))
			}
			s.code.build(s.lengths[:19], "code length")
			for at := 0; at < nlit+ndist; {
				symbol := s.code.symbol(&r, "code")
				if symbol < 16 {
					s.lengths[at] = uint8(symbol)
					at++
					continue
				}
				var n int
				var value uint8
				switch symbol {
				case 16:
					n = int(r.get(2)) + 3
					value = s.lengths[at-1]
				case 17:
					n = int(r.get(3)) + 3
				case 18:
					n = int(r.get(7)) + 11
				}
				for range n {
					s.lengths[at] = value
					at++
				}
			}
			s.lit.build(s.lengths[:nlit], "literal/length")
			s.dist.build(s.lengths[nlit:nlit+ndist], "distance")
		}
		for {
			symbol := s.lit.symbol(&r, "lit")
			if symbol < 256 {
				out = append(out, deflateToken{literal: symbol})
				continue
			}
			if symbol == 256 {
				break
			}
			length := lengthBase[symbol-257] + int(r.get(lengthExtra[symbol-257]))
			distance := s.dist.symbol(&r, "dist")
			distance = distanceBase[distance] + int(r.get(distanceExtra[distance]))
			out = append(out, deflateToken{length: length, distance: distance})
		}
	}
	return out
}

func TestPacketBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 65515, 65516, 131030} {
		data := []byte(strings.Repeat("x", n))
		wire := appendSideband(nil, 1, data)
		var got []byte
		for _, p := range decodePktLines(wire) {
			if p.Kind != 'p' || p.Payload[0] != 1 {
				t.Fatal("invalid sideband")
			}
			got = append(got, p.Payload[1:]...)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("sideband changed bytes")
		}
	}
}
