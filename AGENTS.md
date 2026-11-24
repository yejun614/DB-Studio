# AGENTS.md — DB Studio

Single Go module (`dbstudio`), single binary, embedded web frontend. No Makefile, no bundler.

## Commands

```bash
gofmt -l .                   # must print nothing
go vet ./...
go test -count=1 ./...

go run ./cmd/dbstudio -dev     # serves web/ from disk; refresh to see JS/CSS changes
CGO_ENABLED=0 go build -o bin/dbstudio ./cmd/dbstudio   # Windows: bin/dbstudio.exe

go run ./scripts/gen-favicon    # regenerate web/favicon.{svg,ico} + apple-touch-icon.png
scripts/release.sh [version]    # or .\scripts\release.ps1 -Version v...
go run ./cmd/dbstudio reset-password [-data ./data] [username]
```

- Pre-commit gate: run `gofmt -w .` first, then `gofmt -l .` must print nothing before `go vet`/tests; CI rejects any unformatted file.

## Focused tests

```bash
go test ./internal/erd/ -run TestName -count=1
go test ./internal/dbx/ -integration -run TestIntrospect -v
DBSTUDIO_INTEGRATION=1 go test -count=1 ./...
```

Integration tests auto-skip unless `-integration` or `DBSTUDIO_INTEGRATION=1` is set. Unit suites cover store, monitor, dblog, erd, erdhub, migrate, vcs; `erdhub` runs its hub without WebSocket and `vcs` tests pin each provider's API contract with httptest.

## Integration DBs

Start `docker compose -f docker/compose.test.yaml up -d` first (Oracle: `--profile oracle`). Each test creates its own database—never share one, because schema fingerprints include every table and packages run in parallel. See `docs/development.md` for ports, credentials, and ClickHouse CPU/RAM gotchas.

## Non-obvious constraints

- `embed.go` stays at repo root: `go:embed` can't reference parent dirs (`all:web`).
- No frontend build step: the browser reads ES modules directly. `go test ./...` runs `assets_test.go`, which catches calls to undeclared JS names and unused CSS variables—treat failures as real.
- Pure-Go drivers only; no CGO or platform toolchain deps, or single-binary releases break.
- Platform-specific code is in `internal/hostmon` (`host_*.go`, `oslog_*.go`). CI cross-builds `linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64`; run at least `GOOS=windows go build -o /dev/null ./cmd/dbstudio` and the linux variant before touching those files.
- RBAC has one gate: `auth.resolveWithPolicy` (project participation → access scope → grade/capability). HTTP, WebSocket, AI tools, and macros all pass through it; don't add parallel checks.
- `schema`, `metric`, and `dblog` are separate packages to break import cycles between `dbx` and others; respect those boundaries.
- `mcp` knows nothing about the app; tool execution lives in `api` (`tokenapi.go` gateway, then `mcp_handlers.go`/`rest_handlers.go` add protocols).
- Store metastore uses numbered, embedded SQL files under `internal/store/migrations`; `store.Open` applies them automatically. New migrations need the next sequential number. Table rebuilds that must disable foreign keys must start with the `-- +no-foreign-keys` marker; the runner then pins one connection, disables FKs, runs the migration in a transaction, re-enables FKs, and checks with `PRAGMA foreign_key_check`.
- Timestamps are stored as fixed 9-digit fractional seconds so string ordering matches time ordering; don't switch to RFC3339Nano if editing store time helpers.
- Master encryption key is `DBSTUDIO_MASTER_KEY` (env only) or `data/master.key`; losing it makes stored DB credentials undecryptable, and every clustered node must use the same key. Cluster secret is env-only as `DBSTUDIO_CLUSTER_SECRET`. Neither has a CLI flag.
- CLI flags win over `DBSTUDIO_*` env vars (which win over defaults). Docker images set only `DBSTUDIO_ADDR`/`DBSTUDIO_DATA`, so passing `-addr :9000` overrides env.
- `reset-password` requires an existing meta DB and refuses to run on a replica; a wrong `-data` is rejected rather than silently creating an empty DB.
- Keep build ldflags (version/commit/date) single-sourced by building releases through `scripts/release.sh` or `scripts/release.ps1`, not ad-hoc `go build`.

## Detailed reference

- `docs/development.md` — containers, ports, credentials, ClickHouse tuning.
- `docs/architecture.md` — package map and data flow.
- `docs/operations.md` — keys, Docker, clustering, master key.
- `.github/workflows/ci.yml` / `release.yml` — exact CI and release gates.
