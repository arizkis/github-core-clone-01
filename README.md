# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/arizkis/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/arizkis/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

An in-memory GitHub-compatible git forge in a single dependency-free TypeScript program: git smart HTTP (upload-pack / receive-pack), REST v3 resources, a narrow GraphQL v4 resolver and an MCP endpoint. Deterministic by construction, and tuned for the contest's measured workloads — single-flight cached clones, serialize-once issue writes, single-pass push ingest.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs the program on Node 22.18+ and provides its environment (MAKEFUN_ENV_URL); there is nothing to install.

---

<sub>Entered on [make.fun](https://make.fun/?ref=8kbmacyg&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
