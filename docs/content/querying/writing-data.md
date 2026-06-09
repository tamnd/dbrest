---
title: "Writing data"
description: "Insert, update, upsert, and delete, with the PostgREST status rules."
weight: 40
---

Writes use `POST`, `PATCH`, `PUT`, and `DELETE`. The request body is JSON, the
filters that scope an update or delete are the same query-string filters reads
use, and the `Prefer` header controls what comes back.

## Insert

`POST` a JSON object, or an array of objects, to the table:

```bash
curl -X POST 'localhost:3000/directors' \
  -H 'Content-Type: application/json' \
  -d '{ "id": 4, "name": "Celine Sciamma" }'
```

A successful insert is `201 Created`. For a single inserted row, the response
carries a `Location` header with the primary-key filter that selects it.

Insert several rows at once by posting an array:

```bash
curl -X POST 'localhost:3000/films' \
  -H 'Content-Type: application/json' \
  -d '[
    { "id": 6, "title": "Portrait of a Lady on Fire", "year": 2019, "rating": 8.1, "director_id": 4 }
  ]'
```

## Get the written rows back

By default a write returns no body. Ask for the affected rows with
`Prefer: return=representation`:

```bash
curl -X POST 'localhost:3000/directors' \
  -H 'Content-Type: application/json' \
  -H 'Prefer: return=representation' \
  -d '{ "id": 5, "name": "Chloe Zhao" }'
# 201 Created
# [ { "id": 5, "name": "Chloe Zhao" } ]
```

`return=minimal` is the default and returns `204 No Content`.

## Update

`PATCH` updates the rows a filter selects. The body holds the columns to change:

```bash
# bump the rating of film 1
curl -X PATCH 'localhost:3000/films?id=eq.1' \
  -H 'Content-Type: application/json' \
  -d '{ "rating": 8.6 }'
```

A `PATCH` with no filter updates every row, so scope it carefully. As with
insert, `Prefer: return=representation` returns the updated rows, and the status
is `200` with a representation or `204` without.

## Upsert

A `POST` with `Prefer: resolution=merge-duplicates` inserts new rows and updates
existing ones on a primary-key or unique conflict:

```bash
curl -X POST 'localhost:3000/directors' \
  -H 'Content-Type: application/json' \
  -H 'Prefer: resolution=merge-duplicates' \
  -d '{ "id": 1, "name": "Bong Joon-ho (updated)" }'
```

`resolution=ignore-duplicates` keeps the existing row instead. `PUT` upserts a
single row addressed by its full primary key in the filter.

How an upsert lowers differs by backend: PostgreSQL uses `ON CONFLICT`, MySQL
uses a no-conflict-target form, and SQL Server drives a multi-statement upsert.
The observable result is the same. The ability to target a named unique
constraint is a backend capability, so an upsert that needs one on a backend
that cannot is reported rather than guessed.

## Delete

`DELETE` removes the rows a filter selects:

```bash
curl -X DELETE 'localhost:3000/films?id=eq.6'
```

A delete is `204 No Content`, or `200` with `Prefer: return=representation` to
get the removed rows back. A `DELETE` with no filter removes every row, so the
same caution as `PATCH` applies.

## Constraint failures

A constraint violation maps to the PostgREST SQLSTATE and the matching HTTP
status. A unique violation is a clean `409 Conflict`:

```bash
curl -i -X POST 'localhost:3000/directors' \
  -H 'Content-Type: application/json' \
  -d '{ "id": 1, "name": "duplicate id" }'
# HTTP/1.1 409 Conflict
# { "code": "23505", ... }
```

See [errors](/reference/errors/) for the full mapping from SQLSTATE to status.

## Transactions

Each write runs in the backend's transaction. The transaction tier is a
capability: Full on the relational engines, and on MongoDB it depends on the
deployment topology, which is why the test setup runs MongoDB as a replica set.
A write that needs a guarantee the backend cannot provide is reported rather
than silently weakened.
