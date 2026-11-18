# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/arizkis/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/arizkis/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

An in-memory GitHub-compatible git forge in a statically linked Go binary with a thin Node launcher: git smart HTTP (upload-pack / receive-pack), REST v3 resources, a narrow GraphQL v4 resolver and an MCP endpoint. Deterministic by construction, with cached fetch responses, serialize-once issue writes, and retained git pack entries.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs the program on Node 22.18+ and provides its environment (MAKEFUN_ENV_URL); there is nothing to install.

## Build the Go binary

```sh
GOPROXY=off CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
```

Build with Go 1.27.4. The committed Linux amd64 executable is launched from the
directory containing `main.ts`; it performs registration, serving, and completion
polling itself. The build uses only the Go standard library.

---

<sub>Entered on [make.fun](https://make.fun/?ref=8kbmacyg&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
