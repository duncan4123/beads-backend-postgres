# Beads Backend Postgres

Standalone Postgres backend plugin for Beads and Gas City.

This repository builds `bd-backend-postgres`, a process plugin that speaks the
`beads.backend.v1alpha1` newline-delimited JSON protocol used by the Beads
backend plugin architecture.

The current source is intentionally Beads-derived because the Postgres backend
implementation depends on Beads `internal/...` storage packages. The runtime
shape is still split-process:

```text
bd or gc
  -> reads .beads/metadata.json
  -> starts bd-backend-postgres
  -> talks beads.backend.v1alpha1 over stdin/stdout
  -> plugin owns direct Postgres access
```

## Build

```sh
go build -o bd-backend-postgres ./cmd/bd-backend-postgres
```

## Smoke Test

```sh
bd-backend-postgres capabilities
printf '' | bd-backend-postgres serve
```

The Gas City pack `bd-gc-postgres` builds and installs this binary under:

```text
<city>/.gc/runtime/packs/bd-gc-postgres/bin/bd-backend-postgres
```

## Runtime Configuration

During init, provide a Postgres URL and schema:

```sh
export GC_POSTGRES_URL='postgres://user:password@127.0.0.1:5432/beads?sslmode=disable'
export GC_POSTGRES_SCHEMA='my_city'
```

The plugin writes `backend = "postgres"` metadata and redacts embedded
passwords before persisting `postgres_dsn`.
