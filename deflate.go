package main

// An altered Go reimplementation of the zlib default-level bitstream rules.
// The algorithm is based on Node 22's zlib 1.3.1 deflate.c and trees.c, Copyright
// (C) 1995-2024 Jean-loup Gailly and Mark Adler. See ZLIB-NOTICE.txt.
// Go's compress/flate makes different lazy-match and Huffman tie decisions;
// reproducing these decisions is necessary for the reference's pack bytes.

import (
	"encoding/binary"
	"hash/adler32"
	"math/bits"
	"sync"
)

type deflateToken struct{ literal, length, distance int }
type deflateBits struct {
	data []byte
	bits uint64
	n    uint
}

func (w *deflateBits) put(v uint32, n uint) {
	w.bits |= uint64(v) << w.n
	w.n += n
	for w.n >= 8 {
		w.data = append(w.data, byte(w.bits))
		w.bits >>= 8
		w.n -= 8
	}
}
func (w *deflateBits) align() {
	if w.n > 0 {
		w.data = append(w.data, byte(w.bits))
		w.bits = 0
		w.n = 0
	}
}

type deflateNode struct{ frequency, parent, length, code int }
type deflateTree struct {
	nodes               [573]deflateNode
	heap, depth         [573]int
	heapLength, maxCode int
}

func (t *deflateTree) smaller(a, b int) bool {
	return t.nodes[a].frequency < t.nodes[b].frequency || t.nodes[a].frequency == t.nodes[b].frequency && t.depth[a] <= t.depth[b]
}
func (t *deflateTree) down(k int) {
	v := t.heap[k]
	for j := k * 2; j <= t.heapLength; j = k * 2 {
		if j < t.heapLength && t.smaller(t.heap[j+1], t.heap[j]) {
			j++
		}
		if t.smaller(v, t.heap[j]) {
			break
		}
		t.heap[k] = t.heap[j]
		k = j
	}
	t.heap[k] = v
}
func (t *deflateTree) build(frequency []int, maxBits int) {
	clear(t.nodes[:])
	clear(t.depth[:])
	t.heapLength = 0
	t.maxCode = -1
	for n, f := range frequency {
		t.nodes[n].frequency = f
		if f != 0 {
			t.heapLength++
			t.heap[t.heapLength] = n
			t.maxCode = n
		}
	}
	for t.heapLength < 2 {
		n := 0
		if t.maxCode < 2 {
			t.maxCode++
			n = t.maxCode
		}
		t.heapLength++
		t.heap[t.heapLength] = n
		t.nodes[n].frequency = 1
	}
	for n := t.heapLength / 2; n >= 1; n-- {
		t.down(n)
	}
	heapMax, next := len(t.heap), len(frequency)
	for {
		n := t.heap[1]
		t.heap[1] = t.heap[t.heapLength]
		t.heapLength--
		t.down(1)
		m := t.heap[1]
		heapMax--
		t.heap[heapMax] = n
		heapMax--
		t.heap[heapMax] = m
		t.nodes[next].frequency = t.nodes[n].frequency + t.nodes[m].frequency
		t.depth[next] = max(t.depth[n], t.depth[m]) + 1
		t.nodes[n].parent = next
		t.nodes[m].parent = next
		t.heap[1] = next
		next++
		t.down(1)
		if t.heapLength < 2 {
			break
		}
	}
	heapMax--
	t.heap[heapMax] = t.heap[1]
	t.nodes[t.heap[heapMax]].length = 0
	var counts [16]int
	overflow := 0
	for h := heapMax + 1; h < len(t.heap); h++ {
		n := t.heap[h]
		length := t.nodes[t.nodes[n].parent].length + 1
		if length > maxBits {
			length = maxBits
			overflow++
		}
		t.nodes[n].length = length
		if n <= t.maxCode {
			counts[length]++
		}
	}
	if overflow > 0 {
		for {
			length := maxBits - 1
			for counts[length] == 0 {
				length--
			}
			counts[length]--
			counts[length+1] += 2
			counts[maxBits]--
			overflow -= 2
			if overflow <= 0 {
				break
			}
		}
		h := len(t.heap)
		for length := maxBits; length > 0; length-- {
			n := counts[length]
			for n > 0 {
				h--
				m := t.heap[h]
				if m > t.maxCode {
					continue
				}
				t.nodes[m].length = length
				n--
			}
		}
	}
	var nextCode [16]int
	code := 0
	for length := 1; length <= 15; length++ {
		code = (code + counts[length-1]) << 1
		nextCode[length] = code
	}
	for n := 0; n <= t.maxCode; n++ {
		length := t.nodes[n].length
		if length > 0 {
			t.nodes[n].code = int(bits.Reverse16(uint16(nextCode[length])) >> uint(16-length))
			nextCode[length]++
		}
	}
}
func (t *deflateTree) send(w *deflateBits, symbol int) {
	n := t.nodes[symbol]
	w.put(uint32(n.code), uint(n.length))
}

type lengthRun struct {
	symbol, value int
	extra         uint
}

var deflateCodeOrder = [19]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

func lengthRuns(out []lengthRun, t *deflateTree) []lengthRun {
	previous, next, count, maxCount, minCount := -1, t.nodes[0].length, 0, 7, 4
	if next == 0 {
		maxCount, minCount = 138, 3
	}
	for n := 0; n <= t.maxCode; n++ {
		current := next
		next = 65535
		if n < t.maxCode {
			next = t.nodes[n+1].length
		}
		count++
		if count < maxCount && current == next {
			continue
		}
		if count < minCount {
			for range count {
				out = append(out, lengthRun{symbol: current})
			}
		} else if current != 0 {
			if current != previous {
				out = append(out, lengthRun{symbol: current})
				count--
			}
			out = append(out, lengthRun{16, count - 3, 2})
		} else if count <= 10 {
			out = append(out, lengthRun{17, count - 3, 3})
		} else {
			out = append(out, lengthRun{18, count - 11, 7})
		}
		count = 0
		previous = current
		if next == 0 {
			maxCount, minCount = 138, 3
		} else if current == next {
			maxCount, minCount = 6, 3
		} else {
			maxCount, minCount = 7, 4
		}
	}
	return out
}
func lengthCode(length int) int {
	if length == 258 {
		return 28
	}
	code := 0
	for code < 28 && length >= lengthBase[code+1] {
		code++
	}
	return code
}
func distanceCode(distance int) int {
	code := 0
	for code < 29 && distance >= distanceBase[code+1] {
		code++
	}
	return code
}

var fixedLiteralLengths = func() []int {
	out := make([]int, 288)
	for i := range out {
		out[i] = 8
		if i >= 144 && i <= 255 {
			out[i] = 9
		} else if i >= 256 && i <= 279 {
			out[i] = 7
		}
	}
	return out
}()
var fixedLiteralCodes = func() []uint32 {
	out := make([]uint32, 288)
	var counts, next [16]int
	for _, n := range fixedLiteralLengths {
		counts[n]++
	}
	code := 0
	for n := 1; n <= 15; n++ {
		code = (code + counts[n-1]) << 1
		next[n] = code
	}
	for i, n := range fixedLiteralLengths {
		out[i] = uint32(bits.Reverse16(uint16(next[n])) >> uint(16-n))
		next[n]++
	}
	return out
}()

type deflateScratch struct {
	head, previous    [32768]int
	tokens            []deflateToken
	literalFrequency  [286]int
	distanceFrequency [30]int
	codeFrequency     [19]int
	lit, dist, code   deflateTree
	runs              []lengthRun
}

var deflateScratchPool = sync.Pool{New: func() any {
	return &deflateScratch{tokens: make([]deflateToken, 0, 16383), runs: make([]lengthRun, 0, 320)}
}}

func (s *deflateScratch) resetBlock() {
	s.tokens = s.tokens[:0]
	clear(s.literalFrequency[:])
	clear(s.distanceFrequency[:])
	s.literalFrequency[256] = 1
}
func (s *deflateScratch) tally(token deflateToken) {
	s.tokens = append(s.tokens, token)
	if token.distance == 0 {
		s.literalFrequency[token.literal]++
	} else {
		s.literalFrequency[257+lengthCode(token.length)]++
		s.distanceFrequency[distanceCode(token.distance)]++
	}
}
func (s *deflateScratch) writeBlock(w *deflateBits, raw []byte, final bool) {
	s.lit.build(s.literalFrequency[:], 15)
	s.dist.build(s.distanceFrequency[:], 15)
	s.runs = lengthRuns(s.runs[:0], &s.lit)
	s.runs = lengthRuns(s.runs, &s.dist)
	clear(s.codeFrequency[:])
	for _, r := range s.runs {
		s.codeFrequency[r.symbol]++
	}
	s.code.build(s.codeFrequency[:], 7)
	ncodes := 19
	for ncodes > 4 && s.code.nodes[deflateCodeOrder[ncodes-1]].length == 0 {
		ncodes--
	}
	dynamicCost, fixedCost := 14+3*ncodes, 0
	for symbol, f := range s.literalFrequency {
		extra := 0
		if symbol >= 257 {
			extra = int(lengthExtra[symbol-257])
		}
		dynamicCost += f * (s.lit.nodes[symbol].length + extra)
		fixedCost += f * (fixedLiteralLengths[symbol] + extra)
	}
	for symbol, f := range s.distanceFrequency {
		extra := int(distanceExtra[symbol])
		dynamicCost += f * (s.dist.nodes[symbol].length + extra)
		fixedCost += f * (5 + extra)
	}
	for _, r := range s.runs {
		dynamicCost += s.code.nodes[r.symbol].length + int(r.extra)
	}
	dynamicBytes, fixedBytes := (dynamicCost+10)>>3, (fixedCost+10)>>3
	best := min(dynamicBytes, fixedBytes)
	last := uint32(0)
	if final {
		last = 1
	}
	if len(raw) <= 65535 && len(raw)+4 <= best {
		w.put(last, 3)
		w.align()
		n := uint16(len(raw))
		w.data = append(w.data, byte(n), byte(n>>8), byte(^n), byte(^n>>8))
		w.data = append(w.data, raw...)
	} else {
		fixed := fixedBytes <= dynamicBytes
		if fixed {
			w.put(2|last, 3)
		} else {
			w.put(4|last, 3)
			w.put(uint32(s.lit.maxCode+1-257), 5)
			w.put(uint32(s.dist.maxCode), 5)
			w.put(uint32(ncodes-4), 4)
			for i := 0; i < ncodes; i++ {
				w.put(uint32(s.code.nodes[deflateCodeOrder[i]].length), 3)
			}
			for _, r := range s.runs {
				s.code.send(w, r.symbol)
				w.put(uint32(r.value), r.extra)
			}
		}
		literal := func(symbol int) {
			if fixed {
				w.put(fixedLiteralCodes[symbol], uint(fixedLiteralLengths[symbol]))
			} else {
				s.lit.send(w, symbol)
			}
		}
		for _, token := range s.tokens {
			if token.distance == 0 {
				literal(token.literal)
			} else {
				lc, dc := lengthCode(token.length), distanceCode(token.distance)
				literal(lc + 257)
				w.put(uint32(token.length-lengthBase[lc]), lengthExtra[lc])
				if fixed {
					w.put(uint32(bits.Reverse16(uint16(dc))>>11), 5)
				} else {
					s.dist.send(w, dc)
				}
				w.put(uint32(token.distance-distanceBase[dc]), distanceExtra[dc])
			}
		}
		literal(256)
	}
	if final {
		w.align()
	}
	s.resetBlock()
}
func deflateObject(data []byte) []byte {
	s := deflateScratchPool.Get().(*deflateScratch)
	defer deflateScratchPool.Put(s)
	clear(s.head[:])
	clear(s.previous[:])
	s.resetBlock()
	w := deflateBits{data: make([]byte, 2, 128)}
	w.data[0], w.data[1] = 0x78, 0x9c
	pos, origin, end, blockStart := 0, 0, min(len(data), 65536), 0
	insert := func(pos int) int {
		// Chromium's ANZAC hash uses a little-endian four-byte word.
		value := uint32(data[pos]) | uint32(data[pos+1])<<8 | uint32(data[pos+2])<<16
		if pos+3 < len(data) {
			value |= uint32(data[pos+3]) << 24
		} else if origin > 0 {
			// fill_window retains its high-water mark across slides. The
			// fourth hash byte just beyond EOF is then from the prior window.
			value |= uint32(data[len(data)-32768]) << 24
		}
		h := int((value*66521+66521)>>16) & 32767
		old := s.head[h]
		s.previous[pos&32767] = old
		s.head[h] = pos
		return old
	}
	matchLength, matchStart := 2, 0
	available := false
	for pos < len(data) {
		if end-pos < 262 {
			if pos >= origin+65274 {
				origin += 32768
			}
			end = min(len(data), origin+65536)
		}
		lookahead := end - pos
		head := 0
		if lookahead >= 3 {
			head = insert(pos)
		}
		previousLength, previousMatch := matchLength, matchStart
		matchLength = 2
		if head > origin && previousLength < 16 && pos-head <= 32506 {
			best := previousLength
			limit := max(origin, pos-32506)
			chain := 128
			if best >= 8 {
				chain >>= 2
			}
			nice, maxLength := min(128, lookahead), min(258, lookahead)
			for candidate := head; candidate > origin; {
				length := 0
				for length < maxLength && data[candidate+length] == data[pos+length] {
					length++
				}
				if length > best {
					best = length
					matchStart = candidate
					if length >= nice {
						break
					}
				}
				candidate = s.previous[candidate&32767]
				chain--
				if candidate <= limit || chain == 0 {
					break
				}
			}
			matchLength = min(best, lookahead)
			if matchLength == 3 && pos-matchStart > 4096 {
				matchLength = 2
			}
		}
		if previousLength >= 3 && matchLength <= previousLength {
			s.tally(deflateToken{length: previousLength, distance: pos - 1 - previousMatch})
			maxInsert := pos + lookahead - 3
			for remaining := previousLength - 2; remaining > 0; remaining-- {
				pos++
				if pos <= maxInsert {
					insert(pos)
				}
			}
			pos++
			available = false
			matchLength = 2
			if len(s.tokens) == 16383 {
				s.writeBlock(&w, data[blockStart:pos], false)
				blockStart = pos
			}
		} else if available {
			s.tally(deflateToken{literal: int(data[pos-1])})
			if len(s.tokens) == 16383 {
				s.writeBlock(&w, data[blockStart:pos], false)
				blockStart = pos
			}
			pos++
		} else {
			available = true
			pos++
		}
	}
	if available {
		s.tally(deflateToken{literal: int(data[pos-1])})
	}
	s.writeBlock(&w, data[blockStart:pos], true)
	var trailer [4]byte
	binary.BigEndian.PutUint32(trailer[:], adler32.Checksum(data))
	return append(w.data, trailer[:]...)
}
