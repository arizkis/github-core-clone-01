package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"hash/adler32"
	"math"
	"strconv"
	"strings"
	"sync"
)

const packLimit = 65520

func appendPktLine(out, payload []byte) []byte {
	n := len(payload) + 4
	if n > packLimit {
		panic("Pkt-line exceeds maximum length")
	}
	out = append(out, hexDigits[n>>12&15], hexDigits[n>>8&15], hexDigits[n>>4&15], hexDigits[n&15])
	return append(out, payload...)
}
func pktLine(text string) []byte                   { return appendPktLine(nil, []byte(jsUTF8(text))) }
func appendPktText(out []byte, text string) []byte { return appendPktLine(out, []byte(jsUTF8(text))) }
func pktLines(lines ...string) []byte {
	var out []byte
	for _, line := range lines {
		switch line {
		case "flush":
			out = append(out, "0000"...)
		case "delim":
			out = append(out, "0001"...)
		default:
			out = appendPktText(out, line)
		}
	}
	return out
}
func appendSideband(out []byte, band byte, data []byte) []byte {
	for offset := 0; offset < len(data) || offset == 0; offset += packLimit - 5 {
		part := data[offset:min(len(data), offset+packLimit-5)]
		n := len(part) + 5
		out = append(out, hexDigits[n>>12&15], hexDigits[n>>8&15], hexDigits[n>>4&15], hexDigits[n&15], band)
		out = append(out, part...)
		if len(data) == 0 {
			break
		}
	}
	return out
}

type gitPacket struct {
	Kind         byte
	Payload, Raw []byte
}

func hexPacketLength(data []byte) (int, bool) {
	n := 0
	for _, b := range data {
		v, ok := unhex(b & 127)
		if !ok {
			return 0, false
		}
		n = n*16 + int(v)
	}
	return n, true
}
func decodePktLines(data []byte) []gitPacket {
	out := make([]gitPacket, 0)
	for offset := 0; offset < len(data); {
		if offset+4 > len(data) {
			panic("Truncated pkt-line header")
		}
		n, ok := hexPacketLength(data[offset : offset+4])
		if !ok {
			panic("Invalid pkt-line header")
		}
		offset += 4
		if n == 0 {
			out = append(out, gitPacket{Kind: 'f'})
			continue
		}
		if n == 1 {
			out = append(out, gitPacket{Kind: 'd'})
			continue
		}
		if n < 4 || n > packLimit || n-4 > len(data)-offset {
			panic("Invalid pkt-line length")
		}
		out = append(out, gitPacket{'p', data[offset : offset+n-4], data[offset-4 : offset+n-4]})
		offset += n - 4
	}
	return out
}
func parseServiceRequest(data []byte) []string {
	packets := decodePktLines(data)
	out := make([]string, 0, len(packets))
	for _, p := range packets {
		if p.Kind == 'p' {
			out = append(out, strings.TrimSuffix(utf8Text(p.Payload), "\n"))
		}
	}
	return out
}

func reachable(tips []string) *orderedMap[string, bool] {
	seen := newOrderedMap[string, bool]()
	queue := append([]string(nil), tips...)
	for len(queue) > 0 {
		hash := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen.Has(hash) || hash == zeroOID {
			continue
		}
		o := lookupObject(hash)
		if o == nil {
			panic("Missing git object " + hash)
		}
		seen.Set(hash, true)
		switch o.Type {
		case "commit":
			c := parseCommit(o.Data)
			queue = append(queue, c.Tree)
			queue = append(queue, c.Parents...)
		case "tree":
			for _, e := range parseTree(o.Data) {
				if e.Mode != "160000" {
					queue = append(queue, e.Hash)
				}
			}
		case "tag":
			if target := tagTarget(hash); target != "" {
				queue = append(queue, target)
			}
		}
	}
	return seen
}
func reachableDifference(wants, haves []string) []string {
	held := reachable(haves)
	wanted := reachable(wants)
	out := make([]string, 0, len(wanted.keys))
	for _, h := range wanted.keys {
		if !held.Has(h) {
			out = append(out, h)
		}
	}
	return out
}
func depthObjects(tip string, depth float64) (hashes, shallow []string) {
	limit := math.Max(1, math.Trunc(depth))
	seen := newOrderedMap[string, bool]()
	type item struct {
		hash  string
		level int
	}
	queue := []item{{tip, 1}}
	shallow = make([]string, 0)
	for at := 0; at < len(queue); at++ {
		v := queue[at]
		if v.hash == zeroOID || seen.Has(v.hash) {
			continue
		}
		o := lookupObject(v.hash)
		if o == nil {
			panic("Missing git object " + v.hash)
		}
		seen.Set(v.hash, true)
		switch o.Type {
		case "commit":
			c := parseCommit(o.Data)
			queue = append(queue, item{c.Tree, 0})
			if v.level > 0 {
				if float64(v.level) < limit {
					for _, p := range c.Parents {
						queue = append(queue, item{p, v.level + 1})
					}
				} else if len(c.Parents) > 0 {
					shallow = append(shallow, v.hash)
				}
			}
		case "tree":
			for _, e := range parseTree(o.Data) {
				if e.Mode != "160000" {
					queue = append(queue, item{e.Hash, 0})
				}
			}
		}
	}
	return seen.keys, shallow
}
func objectTypeCode(kind string) byte {
	switch kind {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 0
}
func objectTypeName(code byte) string {
	switch code {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}
func appendEntryHeader(out []byte, kind string, size int) []byte {
	first := objectTypeCode(kind)<<4 | byte(size&15)
	size >>= 4
	for size > 0 {
		out = append(out, first|128)
		first = byte(size & 127)
		size >>= 7
	}
	return append(out, first)
}
func buildPack(objects []*GitObject) []byte {
	out := make([]byte, 12, 1024)
	copy(out, "PACK")
	binary.BigEndian.PutUint32(out[4:], 2)
	binary.BigEndian.PutUint32(out[8:], uint32(len(objects)))
	for _, o := range objects {
		if o.Packed != nil {
			out = append(out, o.Packed...)
		} else {
			out = appendEntryHeader(out, o.Type, len(o.Data))
			out = append(out, deflateObject(o.Data)...)
		}
	}
	digest := sha1.Sum(out)
	return append(out, digest[:]...)
}

// Retained validated entries always bypass the compressor, even when another
// pack orders the objects differently. No cache entry is published on failure.
type entryPrefix struct {
	Bytes  [8]byte
	Length uint8
}
type packedObject struct {
	Hash   [20]byte
	Object *GitObject
}

var validatedEntries = make(map[entryPrefix][]packedObject)

func packEntryPrefix(data []byte) entryPrefix {
	var p entryPrefix
	p.Length = uint8(min(len(data), 8))
	copy(p.Bytes[:], data)
	return p
}

type packScratch struct {
	header  [64]byte
	inflate inflateScratch
	digest  hash.Hash
}

var packScratchPool = sync.Pool{New: func() any { return &packScratch{digest: sha1.New()} }}

func parsePack(pack []byte) *orderedMap[[20]byte, *GitObject] {
	if len(pack) < 32 || pack[0]&127 != 'P' || pack[1]&127 != 'A' || pack[2]&127 != 'C' || pack[3]&127 != 'K' {
		panic("Invalid pack header")
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 {
		panic("Unsupported pack version " + strconv.FormatUint(uint64(version), 10))
	}
	count := binary.BigEndian.Uint32(pack[8:])
	end := len(pack) - 20
	digest := sha1.Sum(pack[:end])
	if !bytes.Equal(digest[:], pack[end:]) {
		panic("Pack checksum mismatch")
	}
	scratch := packScratchPool.Get().(*packScratch)
	defer packScratchPool.Put(scratch)
	objects := newOrderedMap[[20]byte, *GitObject]()
	offset := 12
	for i := uint32(0); i < count; i++ {
		if offset >= end {
			panic("Truncated pack entry")
		}
		start := offset
		var known *packedObject
		bucket := validatedEntries[packEntryPrefix(pack[offset:end])]
		for i, entry := range bucket {
			if len(entry.Object.Packed) <= end-offset && bytes.Equal(pack[offset:offset+len(entry.Object.Packed)], entry.Object.Packed) {
				known = &bucket[i]
				break
			}
		}
		if known != nil {
			offset += len(known.Object.Packed)
			objects.Set(known.Hash, known.Object)
			continue
		}
		first := pack[offset]
		offset++
		code := first >> 4 & 7
		if code == 6 || code == 7 {
			panic("Delta objects are unsupported")
		}
		kind := objectTypeName(code)
		if kind == "" {
			panic("Unknown pack object type " + strconv.Itoa(int(code)))
		}
		size := float64(first & 15)
		shift := 4
		b := first
		for b&128 != 0 {
			if offset >= end {
				panic("Truncated pack size")
			}
			b = pack[offset]
			offset++
			size += float64(b&127) * math.Exp2(float64(shift))
			shift += 7
			if shift > 56 {
				panic("Pack size overflow")
			}
		}
		h := scratch.digest
		h.Reset()
		header := scratch.header[:0]
		header = append(header, kind...)
		header = append(header, ' ')
		header = strconv.AppendInt(header, int64(size), 10)
		header = append(header, 0)
		h.Write(header)
		data, consumed := scratch.inflate.decode(pack[offset:end], size, h)
		offset += consumed
		if float64(len(data)) != size {
			panic("Pack object length mismatch")
		}
		var hash [20]byte
		h.Sum(hash[:0])
		objects.Set(hash, &GitObject{Type: kind, Data: data, Packed: bytes.Clone(pack[start:offset])})
	}
	if offset != end {
		panic("Unexpected bytes before pack checksum")
	}
	for _, hash := range objects.keys {
		o := objects.Get(hash)
		key := packEntryPrefix(o.Packed)
		bucket := validatedEntries[key]
		exists := false
		for _, e := range bucket {
			if bytes.Equal(e.Object.Packed, o.Packed) {
				exists = true
				break
			}
		}
		if !exists {
			validatedEntries[key] = append(bucket, packedObject{hash, o})
		}
	}
	return objects
}
func hashHex(hash [20]byte) string { return hex.EncodeToString(hash[:]) }

type inflateBits struct {
	data []byte
	at   int
	bits uint64
	n    uint
}

func (r *inflateBits) get(n uint) uint32 {
	for r.n < n {
		if r.at == len(r.data) {
			panic("unexpected end of file")
		}
		r.bits |= uint64(r.data[r.at]) << r.n
		r.n += 8
		r.at++
	}
	v := uint32(r.bits & ((uint64(1) << n) - 1))
	r.bits >>= n
	r.n -= n
	return v
}
func (r *inflateBits) align() { r.bits = 0; r.n = 0 }

type inflateTree struct {
	counts  [16]int
	symbols [288]int
	max     int
}

func (t *inflateTree) build(lengths []uint8, kind string) {
	*t = inflateTree{}
	for _, n := range lengths {
		t.counts[n]++
		t.max = max(t.max, int(n))
	}
	if t.max == 0 {
		return
	}
	left := 1
	for bits := 1; bits <= 15; bits++ {
		left = left*2 - t.counts[bits]
		if left < 0 {
			panic("invalid " + kind + "s set")
		}
	}
	if left > 0 && (kind == "code length" || t.max != 1) {
		panic("invalid " + kind + "s set")
	}
	var offsets [16]int
	for bits := 1; bits < 15; bits++ {
		offsets[bits+1] = offsets[bits] + t.counts[bits]
	}
	for symbol, n := range lengths {
		if n != 0 {
			t.symbols[offsets[n]] = symbol
			offsets[n]++
		}
	}
}
func (t *inflateTree) symbol(r *inflateBits, message string) int {
	code, first, index := 0, 0, 0
	for length := 1; length <= t.max; length++ {
		code |= int(r.get(1))
		count := t.counts[length]
		if code < first+count {
			return t.symbols[index+code-first]
		}
		index += count
		first = (first + count) << 1
		code <<= 1
	}
	panic(message)
}

var lengthBase = [29]int{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
var lengthExtra = [29]uint{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
var distanceBase = [30]int{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
var distanceExtra = [30]uint{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}

type inflateScratch struct {
	lit, dist, code inflateTree
	lengths         [320]uint8
}

func (s *inflateScratch) decode(data []byte, size float64, digest hash.Hash) ([]byte, int) {
	r := inflateBits{data: data}
	cmf, flg := r.get(8), r.get(8)
	if (cmf<<8|flg)%31 != 0 {
		panic("incorrect header check")
	}
	if cmf&15 != 8 {
		panic("unknown compression method")
	}
	if cmf>>4 > 7 {
		panic("invalid window size")
	}
	if flg&32 != 0 {
		r.get(32)
		panic("Missing dictionary")
	}
	out := make([]byte, 0, int(math.Min(size, 65536)))
	hashed := 0
	flushDigest := func() {
		if digest != nil && len(out)-hashed >= 32768 {
			_, _ = digest.Write(out[hashed:])
			hashed = len(out)
		}
	}
	for final := false; !final; {
		final = r.get(1) != 0
		kind := r.get(2)
		if kind == 0 {
			r.align()
			n, inv := r.get(16), r.get(16)
			if n^inv != 65535 {
				panic("invalid stored block lengths")
			}
			for range n {
				out = append(out, byte(r.get(8)))
			}
			flushDigest()
			continue
		}
		if kind == 3 {
			panic("invalid block type")
		}
		if kind == 1 {
			for i := range 288 {
				n := uint8(8)
				if i >= 144 && i <= 255 {
					n = 9
				} else if i >= 256 && i <= 279 {
					n = 7
				}
				s.lengths[i] = n
			}
			s.lit.build(s.lengths[:288], "literal/length")
			for i := range 32 {
				s.lengths[i] = 5
			}
			s.dist.build(s.lengths[:32], "distance")
		} else {
			nlit, ndist, ncode := int(r.get(5))+257, int(r.get(5))+1, int(r.get(4))+4
			if nlit > 286 || ndist > 30 {
				panic("too many length or distance symbols")
			}
			clear(s.lengths[:])
			order := [19]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
			for i := 0; i < ncode; i++ {
				s.lengths[order[i]] = uint8(r.get(3))
			}
			s.code.build(s.lengths[:19], "code length")
			for at := 0; at < nlit+ndist; {
				sym := s.code.symbol(&r, "invalid code lengths set")
				if sym < 16 {
					s.lengths[at] = uint8(sym)
					at++
					continue
				}
				n, value := 0, uint8(0)
				switch sym {
				case 16:
					if at == 0 {
						panic("invalid bit length repeat")
					}
					value = s.lengths[at-1]
					n = int(r.get(2)) + 3
				case 17:
					n = int(r.get(3)) + 3
				case 18:
					n = int(r.get(7)) + 11
				}
				if at+n > nlit+ndist {
					panic("invalid bit length repeat")
				}
				for range n {
					s.lengths[at] = value
					at++
				}
			}
			if s.lengths[256] == 0 {
				panic("invalid code -- missing end-of-block")
			}
			s.lit.build(s.lengths[:nlit], "literal/length")
			s.dist.build(s.lengths[nlit:nlit+ndist], "distance")
		}
		for {
			sym := s.lit.symbol(&r, "invalid literal/length code")
			if sym < 256 {
				out = append(out, byte(sym))
				flushDigest()
				continue
			}
			if sym == 256 {
				break
			}
			if sym > 285 {
				panic("invalid literal/length code")
			}
			n := lengthBase[sym-257] + int(r.get(lengthExtra[sym-257]))
			d := s.dist.symbol(&r, "invalid distance code")
			if d > 29 {
				panic("invalid distance code")
			}
			distance := distanceBase[d] + int(r.get(distanceExtra[d]))
			if distance > len(out) {
				panic("invalid distance too far back")
			}
			for range n {
				out = append(out, out[len(out)-distance])
			}
			flushDigest()
		}
	}
	r.align()
	var expected uint32
	for range 4 {
		expected = expected<<8 | r.get(8)
	}
	if adler32.Checksum(out) != expected {
		panic("incorrect data check")
	}
	if digest != nil {
		_, _ = digest.Write(out[hashed:])
	}
	return out, r.at
}
