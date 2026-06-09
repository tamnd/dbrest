---
title: "Installation"
description: "Build dbrest from source with Go."
weight: 20
---

dbrest is a Go program. The SQLite backend uses the pure-Go
[modernc.org/sqlite](https://modernc.org/sqlite) driver, so the whole thing
builds and runs with no cgo and no database to install.

## Requirements

- Go 1.22 or newer.

## Build from source

Clone the repository and build the server:

```bash
git clone https://github.com/tamnd/dbrest
cd dbrest
go build -o dbrest ./cmd/dbrest
```

That produces a `dbrest` binary in the current directory. You can also run it
without building a binary first:

```bash
go run ./cmd/dbrest -config dbrest.conf
```

## Verify

Ask the server for its version flags, or just start it against a throwaway
config and curl the root. The next page does exactly that. If `go build` ran
clean, you are ready for the [quick start](/getting-started/quick-start/).

## Other backends

The SQLite backend is built in and needs nothing else. The PostgreSQL, MySQL,
SQL Server, and MongoDB backends connect to a running server; the
[backend reference](/configuration/backend-reference/) covers their connection
strings and current status. For local testing against a real engine, the
repository's `docker/` directory has a Podman compose file per backend.
