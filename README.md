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
- **Authorization and RLS emulation**: on the emulated backend, table and column privileges gate every read and write (`42501` as `403`, or `401` for an unauthenticated request), a `*` projection is narrowed to the granted columns, and Row Level Security policies are injected as a bound predicate AND-ed above the whole client filter tree, so a client cannot OR its way past a policy, with `WITH CHECK` validated before any row is written.
- **Request context**: the verified claims, the request headers and cookies, the method, the path, and the role are carried on a backend-neutral context (with the GUC JSON serializers a native backend writes verbatim); on the emulated backend the values a policy needs are bound as parameters, and response controls (a status override and added headers a function or policy sets) are applied uniformly across reads, writes, and RPC.
- **Types and casts**: a single canonical PostgreSQL type surface, with the aliases a client may write (`integer`, `boolean`, `double precision`) folded onto it; a query-string operand is coerced against the column's canonical type in the frontend, so a non-integer on an integer column is a clean `22P02` `400` before the query reaches the engine, identical on every backend, while a pattern, an `is` keyword, and a text column are left alone. The value codecs render a driver-native `bool`, timestamp, and uuid to one canonical JSON form regardless of the engine's physical storage.
- **Full-text and regex operators**: `fts`/`plfts`/`phfts`/`wfts` and `match`/`imatch`, parsed identically on every backend (the parenthesized `fts(english)` config is read as a language, not a quantifier). On SQLite a full-text filter lowers to an FTS5 `MATCH` against the virtual table that shadows the column, with the FTS5 table and its shadow tables hidden from the exposed schema; a column with no covering FTS5 index is a clean `PGRST127` rather than a silent substring scan. Regex lowers to a registered RE2 `regexp()`, and a pattern using a feature RE2 lacks (a backreference, lookaround) is rejected up front with `PGRST127` instead of failing inside the engine.
- **OpenAPI root**: `GET /` returns the self-describing Swagger 2.0 document PostgREST emits (`application/openapi+json`), built from the schema model and the function registry: a path and definition per relation, the read/write operation set, `/rpc/<fn>` paths by volatility, primary-key and foreign-key notes, and a JWT security scheme when auth is configured. Each column advertises only the filter operators the active backend can actually serve, consulting the capability matrix, so the document never promises a feature the next request would reject. `openapi-mode=disabled` turns the root off; `openapi-server-proxy-uri` rewrites the advertised host and base path for service behind a reverse proxy.
- **Configuration**: a flat PostgREST-style config file (`key = "value"`, comments, triple-quoted multi-line values) layered with the environment, where the environment overrides the file key by key. Every option is settable under the PostgREST `PGRST_*` spelling, so an existing deployment's environment keeps working, and the native `DBREST_*` spelling, which wins when both are present. `db-backend` selects the engine (only `sqlite` is built in today; another known engine is a clear startup error, an unknown one a validation error), `db-uri` carries the connection string, and the file types and validates the full option surface (ports and modes are range- and enum-checked, an unknown key fails loudly) before the server starts. The command takes a single `-config` path and otherwise reads the environment.
- **Conformance harness**: a differential test harness that replays one neutral request corpus against a subject and a golden reference and compares the responses after normalization (canonical JSON, unordered object keys, set-versus-sequence row comparison driven by whether the request pins `order`, volatile-field masking by JSON pointer, transport headers dropped, the contractual headers and the four-key error envelope compared exactly). A capability-aware allowlist is the ledger of every documented divergence, and an allowlist tier that disagrees with the live capability matrix is a build failure. The matrix is made executable: a Native or Emulated feature must reproduce the golden response, and an Unsupported one must return `PGRST127` rather than a wrong answer, checked against the live SQLite subject. `go run ./cmd/dbrest-conformance --backend sqlite` reproduces a pass locally; CI runs it as a gating job. The golden side is currently a checked-in recorded corpus (the form captured PostgREST responses take); the live PostgreSQL-plus-PostgREST capture lands with the container CI matrix.
- A shared **IR-to-SQL compiler** parameterized by a per-engine `Dialect`, with every value bound and every identifier quoted.
- **Introspection** into the unified schema model and a planner that validates names and binds them.

The capability model, the backend SPI, and the error envelope are in place. The PostgreSQL dialect and its version-computed capabilities have landed (`backend/postgres`), the reference oracle the conformance harness diffs against; its pgx data plane is a follow-on slice, since it needs a live server to test. The MySQL, SQL Server, and MongoDB backends are on the roadmap and land against the same SPI; each joins the conformance harness by adding its fixture and a CI job, with no harness changes.

## Quick start

Write a config file naming the backend and the database:

```sh
cat > dbrest.conf <<'EOF'
db-backend  = "sqlite"
db-uri      = "file:./example.sqlite"
server-port = 3000
EOF

go run ./cmd/dbrest -config dbrest.conf
```

The same options are settable from the environment instead, with no file:

```sh
DBREST_DB_URI='file:./example.sqlite' DBREST_SERVER_PORT=3000 go run ./cmd/dbrest
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
| `pgtypes` | The canonical PostgreSQL type surface, alias folding, and the value codecs (operand parsing and JSON rendering). |
| `plan` | Name resolution: binds the IR to the model, raising the PGRST2xx resolution errors. |
| `backend` | The backend SPI and the four-tier `Capabilities` model. |
| `backend/sqlgen` | The single IR-to-SQL compiler, parameterized by a `Dialect`. |
| `backend/sqlite` | The SQLite reference backend (pure-Go [modernc.org/sqlite](https://modernc.org/sqlite), cgo-free). |
| `backend/postgres` | The PostgreSQL dialect and its version-computed capabilities (the reference oracle). The pgx data plane is a follow-on slice. |
| `auth` | Stateless JWT verification, role resolution, and the bounded SIEVE verification cache. |
| `authz` | The privilege and RLS registry: the column gate and the unbypassable policy injection. |
| `reqctx` | The per-request context handed to a backend (role, claims, headers, cookies, schema, and response controls). |
| `httpapi` | The HTTP frontend: router, read and write pipelines, PostgREST-shaped renderer. |
| `config` | The file and environment loader: the PostgREST option surface, the `PGRST_*`/`DBREST_*` env spellings, typing and validation. |
| `openapi` | The Swagger 2.0 generator behind the `GET /` root. |
| `conformance` | The differential harness: the neutral corpus, response normalization and comparison, the capability-aware allowlist, and the matrix and capability self-consistency checks. |
| `cmd/dbrest` | The server entry point. |
| `cmd/dbrest-conformance` | The local conformance runner (`--backend sqlite`). |

## Development

```sh
go test ./...                  # unit + end-to-end tests
go test ./... -race            # with the race detector
go test ./httpapi/ -bench .    # request benchmarks
go vet ./...

go run ./cmd/dbrest-conformance --backend sqlite   # replay the conformance corpus
```

The SQLite backend is cgo-free, so the whole suite runs anywhere Go runs, with no database to install.

## Design

The full design lives in the project specification (overview, the backend SPI, the capability matrix, the query IR, per-engine dialects, reads/writes/RPC, auth and RLS, content negotiation, OpenAPI, and the conformance plan). Implementation notes for what is built are written alongside the code.

## Compatibility

Where dbrest's behavior reproduces PostgREST, PostgREST is the reference: if a running PostgREST v14 and dbrest disagree on an in-scope feature, PostgREST wins and dbrest has the bug. The capability matrix is the single source of truth for what is native, emulated, best-effort, or unsupported on each backend; an unsupported feature returns `PGRST127` rather than a wrong answer.

## License

Apache 2.0. See [LICENSE](LICENSE).
