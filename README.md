# dbrest

A REST server that speaks the [PostgREST](https://postgrest.org) API on top of any database.

PostgREST turns a PostgreSQL database into a RESTful API by reading the database's own catalogs and serving every table, view, and function as an HTTP resource. dbrest keeps that exact HTTP contract (the same URL grammar, operators, resource embedding, `Prefer` headers, error envelopes, and OpenAPI root) and makes the database underneath pluggable. Point it at PostgreSQL, SQLite, MySQL, SQL Server, or MongoDB and a client written against PostgREST should not be able to tell the difference.

dbrest is a compatible reimplementation of PostgREST, and saying so is the point. The compatibility target is the PostgREST v14 line.

## The idea

The PostgREST contract is independent of how rows are stored. A client sees URLs, status codes, headers, and JSON; it cannot see whether a filter became a SQL `WHERE`, a MongoDB `$match`, or whether embedding became a `JOIN` or a `$lookup`.

So dbrest splits in two:

- a single **engine-agnostic frontend** that parses an HTTP request into an abstract query representation (the IR) and plans it against a unified schema model, and
- a set of **backends** that lower that IR to one concrete engine.

The frontend never branches on the engine. It consults each backend's declared **capabilities** and either lowers a feature natively, rewrites an emulated one, or rejects an unsupported one with a precise error. Adding a database is implementing one interface, not forking the server.

```
HTTP ─▶ parse ─▶ plan ─▶ authorize ─▶ Backend.Execute ─▶ render ─▶ HTTP
        (IR)     (model)               (one engine)       (PostgREST-shaped)
```

## Status

Early, and built subsystem by subsystem against a complete design spec. What works end to end today:

- **Reads** (`GET`/`HEAD`) over the **SQLite** reference backend: column projection and aliases, the horizontal-filter operators, `and`/`or`/`not` trees, `order` with PostgreSQL NULLS placement, `limit`/`offset` pagination with `Content-Range` and `206`/`200`, the singular-object media type with the `PGRST116` rule, and empty-result and unknown-name errors in the unified envelope.
- **Writes** (`POST`/`PATCH`/`PUT`/`DELETE`): insert, update, upsert, and delete with the `201`/`200`/`204` status rule, a `Location` header for a single inserted row, `return=representation`, and SQLite constraint failures mapped to PostgREST SQLSTATEs (a unique violation is a clean `409`).
- **Resource embedding**: `select=title,director(name)` nests related resources, resolved against introspected foreign keys and assembled as JSON in the engine, with `PGRST200`/`PGRST201` for missing and ambiguous relationships.
- **Content negotiation** beyond JSON: the singular object type, `text/csv`, and the scalar `application/octet-stream`/`text/plain` types.
- **RPC** at `/rpc/<fn>` over a portable function registry: scalar, setof, and table returns, `GET`/`POST` by volatility (a `GET` to a volatile function is `405`), the read-only versus read-write transaction, post-filtering a table return, and `PGRST202` when no function matches.
- **JWT authentication**: stateless bearer-token verification (HMAC, RSA, ECDSA), pinned algorithms with the `none` swap refused, `exp`/`nbf`/`iat`/`aud` with clock skew, the role claim with nested-path support and the anon fallback, `PGRST301`/`PGRST302`/`403` outcomes, and a bounded SIEVE verification cache that never extends a token's lifetime.
- A shared **IR-to-SQL compiler** parameterized by a per-engine `Dialect`, with every value bound and every identifier quoted.
- **Introspection** into the unified schema model and a planner that validates names and binds them.

The capability model, the backend SPI, and the error envelope are in place. RLS emulation, request context and GUCs, OpenAPI, and the PostgreSQL/MySQL/SQL Server/MongoDB backends are on the roadmap and land against the same SPI.

## Quick start

```sh
go run ./cmd/dbrest -db ./example.sqlite -addr :3000
```

Then query it the way you would query PostgREST:

```sh
# every column, all rows
curl 'localhost:3000/films'

# project, filter, order, paginate
curl 'localhost:3000/films?select=title,year&year=gte.2000&order=year.desc&limit=10'

# a single object instead of an array
curl 'localhost:3000/films?id=eq.42' \
  -H 'Accept: application/vnd.pgrst.object+json'
```

An empty match is `[]` with `200`, never `404`. A name that is not in the schema is a PostgREST error envelope:

```json
{ "code": "PGRST205", "message": "...", "details": null, "hint": null }
```

## Layout

Flat packages, no `internal/`, no `/vN` suffixes.

| Package | Role |
|---------|------|
| `pgerr` | The unified error envelope and the PGRST code table; one serializer for byte-identical bodies across engines. |
| `ir` | The engine-agnostic query IR and the URL/`Prefer` parser (pure syntax; PGRST1xx errors). |
| `schema` | The unified schema model every backend's introspection produces. |
| `plan` | Name resolution: binds the IR to the model, raising the PGRST2xx resolution errors. |
| `backend` | The backend SPI and the four-tier `Capabilities` model. |
| `backend/sqlgen` | The single IR-to-SQL compiler, parameterized by a `Dialect`. |
| `backend/sqlite` | The SQLite reference backend (pure-Go [modernc.org/sqlite](https://modernc.org/sqlite), cgo-free). |
| `reqctx` | The per-request context handed to a backend (role, claims, response controls). |
| `httpapi` | The HTTP frontend: router, read and write pipelines, PostgREST-shaped renderer. |
| `cmd/dbrest` | The server entry point. |

## Development

```sh
go test ./...                  # unit + end-to-end tests
go test ./... -race            # with the race detector
go test ./httpapi/ -bench .    # request benchmarks
go vet ./...
```

The SQLite backend is cgo-free, so the whole suite runs anywhere Go runs, with no database to install.

## Design

The full design lives in the project specification (overview, the backend SPI, the capability matrix, the query IR, per-engine dialects, reads/writes/RPC, auth and RLS, content negotiation, OpenAPI, and the conformance plan). Implementation notes for what is built are written alongside the code.

## Compatibility

Where dbrest's behavior reproduces PostgREST, PostgREST is the reference: if a running PostgREST v14 and dbrest disagree on an in-scope feature, PostgREST wins and dbrest has the bug. The capability matrix is the single source of truth for what is native, emulated, best-effort, or unsupported on each backend; an unsupported feature returns `PGRST127` rather than a wrong answer.

## License

Apache 2.0. See [LICENSE](LICENSE).
