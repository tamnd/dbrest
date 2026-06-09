---
title: "Stored functions (RPC)"
linkTitle: "Functions (RPC)"
description: "Call database functions over HTTP at /rpc."
weight: 50
---

A function is exposed at `/rpc/<name>`. PostgREST calls these RPC. dbrest serves
them through a portable function registry, so the same contract works on engines
that have no native stored procedures.

## Call a function

A read-only function is called with `GET`, passing arguments as query
parameters:

```bash
curl 'localhost:3000/rpc/films_in_year?year=2019'
```

A function that writes is called with `POST`, passing arguments as a JSON body:

```bash
curl -X POST 'localhost:3000/rpc/add_film' \
  -H 'Content-Type: application/json' \
  -d '{ "title": "First Cow", "year": 2019, "director_id": 2 }'
```

## GET versus POST follows volatility

Whether a function may be called with `GET` depends on its volatility. A
read-only (immutable or stable) function answers `GET`. A volatile function,
one that writes, must be called with `POST`. A `GET` to a volatile function is
`405 Method Not Allowed`, so a cacheable verb never triggers a write.

## Return shapes

A function can return a scalar, a set of values, or a table. A table return
behaves like a regular resource: you can project and post-filter it with the
same query-string syntax.

```bash
# a table-returning function, filtered and projected like any table
curl 'localhost:3000/rpc/top_films?select=title,rating&rating=gte.8&order=rating.desc'
```

## When no function matches

If no registered function matches the name and argument signature, the response
is `PGRST202`:

```json
{ "code": "PGRST202", "message": "...", "details": null, "hint": "..." }
```

## How functions are provided per backend

On PostgreSQL the functions are the database's own, read from the catalog. On
backends without native stored procedures, such as SQLite and MongoDB, the
[`function-registry`](/configuration/configuration/) option supplies them: each
entry is a named function with its argument signature and volatility, backed by
a parameterized query or pipeline, or a Go handler. The HTTP contract, the verb
rules, the return shapes, and the `PGRST202` miss, is identical either way.
