---
title: "Errors"
description: "The error envelope, and the codes you will meet."
weight: 20
---

Every error is a JSON envelope with the same four keys on every backend, served
by one serializer so the body is byte-identical across engines:

```json
{ "code": "PGRST205", "message": "...", "details": null, "hint": null }
```

- `code` is a PostgREST `PGRSTxxx` code or a PostgreSQL `SQLSTATE`.
- `message` is a human-readable summary.
- `details` and `hint` are optional, and carry context when there is some.

A name that is not in the schema, an empty match scoped to a single object, a
type mismatch, a constraint violation: all of them come back in this shape, not
as an HTML page.

## Frontend codes (PGRST)

These come from the request frontend, before or around the engine call. They are
identical on every backend.

| Code | Meaning |
| --- | --- |
| `PGRST116` | A singular request matched zero or more than one row. |
| `PGRST127` | The feature is not supported on this backend. The message names the feature and backend. |
| `PGRST200` | An embed named a relationship that has no foreign key. |
| `PGRST201` | An embed is ambiguous; more than one relationship matches. The details list the candidates. |
| `PGRST202` | No function matched the name and argument signature at `/rpc`. |
| `PGRST205` | The relation name is not in the exposed schema. |
| `PGRST301` | The JWT is malformed or fails verification. |
| `PGRST302` | The JWT verified but is missing a required element. |

`PGRST1xx` codes cover parse errors in the URL grammar and `Prefer` header, and
are also identical on every backend.

## Engine codes (SQLSTATE)

A failure that originates in the engine maps to a PostgreSQL `SQLSTATE` and the
matching HTTP status, so the same condition reads the same way regardless of the
underlying database.

| Code | Meaning | Status |
| --- | --- | --- |
| `22P02` | Invalid text representation, for example a non-integer on an integer column. | `400` |
| `23505` | Unique violation. | `409` |
| `42501` | Insufficient privilege. | `403` (or `401` if unauthenticated) |

For example, a type mismatch is caught in the frontend and returned as `22P02`
before the query runs:

```bash
curl -i 'localhost:3000/films?id=eq.abc'
# HTTP/1.1 400 Bad Request
# { "code": "22P02", "message": "...", "details": null, "hint": null }
```

And a duplicate primary key is a clean `409`:

```bash
curl -i -X POST 'localhost:3000/directors' \
  -H 'Content-Type: application/json' \
  -d '{ "id": 1, "name": "duplicate" }'
# HTTP/1.1 409 Conflict
# { "code": "23505", ... }
```

## The PGRST127 boundary

`PGRST127` is the one code that is specific to dbrest's design. It is how a
backend says "I cannot serve this faithfully" instead of returning a wrong
answer. If you see it, the [capability model](/configuration/choosing-a-backend/)
explains why that feature is Unsupported on that engine, and often another
backend serves it Native.
