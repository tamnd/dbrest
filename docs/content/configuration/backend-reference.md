---
title: "Backend reference"
description: "Connection strings and current status for each engine."
weight: 30
---

One option chooses the engine. Exactly one backend is active per process; there
is no multi-engine mode.

```ini
db-backend = "sqlite"
db-uri     = "file:./example.sqlite"
```

The connection string is the standard PostgREST `db-uri`, interpreted by the
selected backend's driver. dbrest does not invent a wrapper around the driver's
own format.

## SQLite

```ini
db-backend = "sqlite"
db-uri     = "file:app.db?_journal=WAL&_busy_timeout=5000"
```

The `db-uri` is a file path or a `file:` URI rather than a server address.
Pragmas such as WAL journaling and `busy_timeout` travel as URI parameters. The
driver is pure Go and cgo-free, so SQLite runs anywhere Go runs with nothing to
install. This is the reference backend and the one this guide uses.

## PostgreSQL

```ini
db-backend = "postgres"
db-uri     = "postgres://user:pass@host:5432/dbname?sslmode=require"
```

Parsed by `pgx`. The supported floor is PostgreSQL 13, since PostgREST v14
dropped end-of-life 12. This is the reference oracle the conformance harness
diffs against. The dialect and version-computed capabilities have landed; the
live data plane is a follow-on slice.

## MySQL and MariaDB

```ini
db-backend = "mysql"
db-uri     = "user:pass@tcp(host:3306)/dbname?parseTime=true&loc=UTC"
```

A `go-sql-driver/mysql` DSN. `parseTime=true` is recommended. The dialect and
capabilities have landed, the first real divergence from the PostgreSQL oracle:
an `IS NULL` sort key for NULL placement, a no-conflict-target upsert,
restricted cast targets, `REGEXP_LIKE`, and `MATCH ... AGAINST` boolean-mode
full text. The driver data plane is a follow-on slice.

## SQL Server

```ini
db-backend = "sqlserver"
db-uri     = "sqlserver://user:pass@host:1433?database=dbname"
```

A URL or ADO keyword connection string, parsed by `go-mssqldb`. The dialect and
capabilities have landed: bracket-quoted identifiers, named `@pN` placeholders,
`OFFSET`/`FETCH` paging that injects an `ORDER BY` when the client gave none, a
`CASE` NULL sort key, `OUTPUT` in place of `RETURNING`, a multi-statement
upsert, and `CONTAINS`/`FREETEXT` full text, with native roles, Row Level
Security, and a session-context store. The driver data plane is a follow-on
slice.

## MongoDB

```ini
db-backend = "mongodb"
db-uri     = "mongodb://user:pass@host:27017/dbname?replicaSet=rs0"
```

Parsed by the official Mongo driver. The URI selects the cluster; the exposed
database comes from the URI path or from `db-schemas`. This is the one engine
that does not use the SQL compiler: it lowers a filter to a `$match` query
document, a read to a `$match`/`$sort`/`$skip`/`$limit`/`$project` pipeline,
casts to `$convert`, and NULLS placement to an `$addFields` sort key. Array and
range operators are Unsupported, and the security model is emulated app-side.
Because MongoDB has no fixed schema and no foreign keys, the
[`declared-schema` and `declared-relationships`](/configuration/configuration/)
options carry the metadata the introspector would otherwise read. The live data
plane is a follow-on slice.

## Backends that need a running server

For local testing against PostgreSQL, MySQL, MariaDB, SQL Server, or MongoDB,
the repository's `docker/` directory has a Podman compose file per backend and a
`docker/all/` that runs them together. MongoDB runs as a single-node replica
set so its transaction capability resolves the way a production deployment
would.

```bash
podman compose -f docker/postgres/compose.yaml up -d   # one engine
podman compose -f docker/all/compose.yaml up -d        # all of them
```
