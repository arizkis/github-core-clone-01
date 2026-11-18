package main

import (
	"os"
	"strings"
)

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var firstNames = strings.Fields("ada bram cora dmitri elena felix greta hugo iris jonas kira lars mira nico olga pavel quinn rosa sven tess ulla vito wren yara zane")
var lastNames = strings.Fields("anders bauer costa dahl eriksen fischer garcia holm iversen jansen keller lind moreau novak olsen peters quist rossi sato turi ulrich vega weber xu yilmaz zimmer")

func fnv(text string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for _, c := range jsUTF16(text) {
		h = (h ^ uint64(c)) * 0x100000001b3
	}
	return h
}

func mix(value uint64) uint64 {
	v := value + 0x9e3779b97f4a7c15
	v = (v ^ (v >> 30)) * 0xbf58476d1ce4e5b9
	v = (v ^ (v >> 27)) * 0x94d049bb133111eb
	return v ^ (v >> 31)
}

type rng struct{ state uint64 }

func newRng(seed uint64) *rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &rng{seed}
}
func (r *rng) next() uint64 {
	v := r.state
	v ^= v >> 12
	v ^= v << 25
	v ^= v >> 27
	r.state = v
	return v * 0x2545f4914f6cdd1d
}
func (r *rng) nextInt(limit uint64) int {
	ceiling := uint64(1<<32) - uint64(1<<32)%limit
	for {
		v := r.next() >> 32
		if v < ceiling {
			return int(v % limit)
		}
	}
}
func scenarioUsers() []string {
	r := newRng(mix(scenarioSeed ^ fnv("users")))
	used := make(map[string]bool)
	users := make([]string, 0, 12)
	for range 12 {
		base := firstNames[r.nextInt(uint64(len(firstNames)))] + lastNames[r.nextInt(uint64(len(lastNames)))]
		login := base
		for suffix := 2; used[login]; suffix++ {
			login = base + "-" + jsString(suffix)
		}
		used[login] = true
		users = append(users, login)
	}
	return users
}

var generatedUsers = scenarioUsers()
var viewerLogin, strangerLogin = generatedUsers[0], generatedUsers[2]

// Accessed under ForgeStore.mu, just like the source's single event loop.
var lastTokenHeader string
var lastTokenVerdict bool

func tokenIsAuthorized(header string) bool {
	if header == lastTokenHeader {
		return lastTokenVerdict
	}
	verdict := false
	if len(header) > 6 && strings.EqualFold(header[:6], "Bearer") {
		tail := header[6:]
		token := strings.TrimLeftFunc(tail, jsSpace)
		if len(token) < len(tail) && token != "" && strings.IndexFunc(token, jsSpace) < 0 {
			expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
			verdict = expected == "" || token == expected
		}
	}
	lastTokenHeader, lastTokenVerdict = header, verdict
	return verdict
}

func jsSpace(r rune) bool {
	return r == 9 || r == 10 || r == 11 || r == 12 || r == 13 || r == 32 || r == 0xa0 || r == 0x1680 || r >= 0x2000 && r <= 0x200a || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000 || r == 0xfeff
}
