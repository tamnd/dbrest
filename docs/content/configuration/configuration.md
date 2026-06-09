---
title: "Configuration"
description: "The config file, the environment, and how they combine."
weight: 10
---

dbrest takes a single config file and reads the environment. A value PostgREST
understands is understood here with the same meaning, so an existing PostgREST
config file is a valid starting point.

## Two sources, one precedence

There are two sources, applied in order:

1. A config file, a flat key/value file using the PostgREST option names.
2. Environment variables, one per option.

The environment overrides the file, key by key. This is PostgREST's own
precedence, so an operator templates a file and overrides per-deployment in the
environment.

## The config file format

The file is flat: `key = "value"`, with `#` comments and triple-quoted
multi-line values. Pass it with `-config`:

```bash
go run ./cmd/dbrest -config dbrest.conf
```

A minimal file names the backend, the database, and a port:

```ini
db-backend  = "sqlite"
db-uri      = "file:./example.sqlite"
server-port = 3000
```

The loader types and validates the whole option surface before the server
starts. Ports and modes are range- and enum-checked, and an unknown key fails
loudly rather than being ignored.

## Environment variables

Every option is settable from the environment with no file at all. dbrest
accepts two spellings:

- The PostgREST spelling `PGRST_*`, so an existing deployment's environment
  keeps working.
- The native spelling `DBREST_*` for the same option.

So `db-uri` is `PGRST_DB_URI` or `DBREST_DB_URI`. When both spellings are
present, the `DBREST_*` value wins, since it is the more specific intent.

```bash
DBREST_DB_URI='file:./example.sqlite' DBREST_SERVER_PORT=3000 go run ./cmd/dbrest
```

## The options that carry over from PostgREST

These behave as they do in PostgREST. The ones that touch the database take
effect through the backend's capabilities.

| Option | Meaning |
| --- | --- |
| `db-uri` | Connection string for the selected backend. |
| `db-schemas` | The exposed schema or schemas; only these are introspected. |
| `db-anon-role` | Role for unauthenticated requests. |
| `db-pre-request` | Function run before the main query to mutate the request context. |
| `db-extra-search-path` | Extra schemas on the search path for unqualified names. |
| `db-max-rows` / `max-rows` | Hard cap on rows returned per request, enforced as a `LIMIT`. |
| `jwt-secret` | HMAC secret or key used to verify the JWT. |
| `jwt-aud` | Required `aud` claim. |
| `jwt-role-claim-key` | JSON path to the role claim. |
| `jwt-cache-max-entries` | Bounded cache of verified JWTs. Default 1000, `0` disables. |
| `server-host` | Listen address for the API server. |
| `server-port` | Listen port for the API server. |
| `server-unix-socket` | Listen on a Unix socket instead of TCP. |
| `db-pool` | Maximum connections in the pool. |
| `db-pool-acquisition-timeout` | How long to wait for a pooled connection. |
| `openapi-mode` | `follow-privileges`, `ignore-privileges`, or `disabled`. |
| `openapi-server-proxy-uri` | Base URL advertised in the OpenAPI document. |
| `log-level` | `crit`, `error`, `warn`, `info`, or `debug`. |
| `log-query` | Boolean. When on, logs the lowered query for each request. |
| `server-cors-allowed-origins` | CORS allow-list. Empty allows all. |

The auth and OpenAPI options are pure frontend concerns and behave identically
on every engine.

## The dbrest additions

These exist because some backends carry no engine-side metadata. They are
optional on PostgreSQL, which has a real catalog, and load-bearing on MongoDB
and on foreign-key-less SQL schemas.

| Option | Purpose |
| --- | --- |
| `db-backend` | Selects the engine: `postgres`, `sqlite`, `mysql`, `sqlserver`, or `mongodb`. |
| `declared-schema` | Declared relations, columns, types, and primary keys for schemaless backends. |
| `declared-relationships` | The relationship registry for backends with no foreign keys. |
| `function-registry` | Named RPC functions for backends without stored procedures. |
| `policy-registry` | The privilege and RLS-policy registry for emulated backends. |
| `capability-overrides` | Narrow, safe overrides of a backend's published capability tier. |

`capability-overrides` can only move a cell within what the engine can actually
do. It cannot turn an Unsupported feature Native, because the
[capability matrix](/configuration/choosing-a-backend/) is the source of truth
and a divergence is a bug.

## The admin server

dbrest runs a second HTTP listener for operations, separate from the API port,
exactly as PostgREST's admin server does. Configure it independently and keep it
behind your network boundary.

| Option | Meaning |
| --- | --- |
| `admin-server-host` | Listen address for the admin server. |
| `admin-server-port` | Listen port. Unset disables the admin server. |

It serves `GET /live` (liveness), `GET /ready` (readiness, true once the schema
cache is built and the backend is healthy), `GET /metrics`, and a reload
trigger. Route traffic on readiness, not liveness. See
[deployment](/operations/deployment/) for how this fits a load balancer.
