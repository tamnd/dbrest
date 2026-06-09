---
title: "Types and casts"
description: "The canonical type surface, operand coercion, and explicit casts."
weight: 70
---

dbrest presents a single canonical PostgreSQL type surface across every backend.
A value you write in the query string is coerced against the column's type in
the frontend, before the request reaches the engine, so type behavior is
identical no matter which database is underneath.

## One canonical type surface

There is one set of type names, the PostgreSQL set. The aliases a client might
write are folded onto the canonical name:

- `integer` folds onto `int4`.
- `boolean` folds onto `bool`.
- `double precision` folds onto `float8`.

Whatever the engine stores physically, the value codecs render a driver-native
boolean, timestamp, or UUID to one canonical JSON form. A boolean is `true` or
`false`, a timestamp is one ISO form, a UUID is one canonical spelling,
regardless of how the engine keeps it.

## Operand coercion happens up front

A query-string operand is checked against the column's canonical type in the
frontend. A non-integer on an integer column is a clean `22P02` (`400`) before
the query reaches the engine, and the error is identical on every backend:

```bash
curl -i 'localhost:3000/films?id=eq.abc'
# HTTP/1.1 400 Bad Request
# { "code": "22P02", "message": "...", "details": null, "hint": null }
```

Patterns, the `is` keywords, and text columns are left alone, since coercing
them would be wrong.

## Explicit casts

Cast a column in `select` with the `::type` suffix, the way PostgREST does, to
control the output type:

```bash
curl 'localhost:3000/films?select=title,year::text'
```

The available cast targets depend on the backend. PostgreSQL casts broadly;
MySQL restricts cast targets to what `CAST` accepts; MongoDB casts through
`$convert`. A cast a backend cannot perform faithfully is reported rather than
guessed.

## Why this matters

Pushing type coercion into the frontend is what makes the contract uniform. The
boundary between a valid and an invalid value is the same on SQLite as on
PostgreSQL, and you see a type error as a clean `400` in development rather than
as an engine-specific failure deep in a query. The
[errors](/reference/errors/) page lists the type-related codes.
