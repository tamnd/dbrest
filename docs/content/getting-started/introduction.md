---
title: "Introduction"
description: "What dbrest is, and why it splits the PostgREST contract from the engine underneath."
weight: 10
---

dbrest is a REST server that speaks the [PostgREST](https://postgrest.org) API
on top of any database. PostgREST turns a PostgreSQL database into a RESTful API
by reading the database's own catalogs and serving every table, view, and
function as an HTTP resource. dbrest keeps that exact HTTP contract and makes the
database underneath pluggable.

The compatibility target is the PostgREST v14 line. Where dbrest reproduces
PostgREST behavior, PostgREST is the reference: if a running PostgREST v14 and
dbrest disagree on an in-scope feature, PostgREST wins and dbrest has the bug.

## The idea

The PostgREST contract does not depend on how rows are stored. A client sees
URLs, status codes, headers, and JSON. It cannot see whether a filter became a
SQL `WHERE`, a MongoDB `$match`, or whether embedding became a `JOIN` or a
`$lookup`.

So dbrest splits in two:

- A single engine-agnostic frontend that parses an HTTP request into an abstract
  query representation and plans it against a unified schema model.
- A set of backends that lower that representation to one concrete engine.

The frontend never branches on the engine. It consults each backend's declared
capabilities and either lowers a feature natively, rewrites an emulated one, or
rejects an unsupported one with a precise error.

```
HTTP -> parse -> plan -> authorize -> Backend.Execute -> render -> HTTP
        (IR)     (model)              (one engine)       (PostgREST-shaped)
```

## What you get

Because the contract is PostgREST, you get a full REST API generated from your
schema with no code to write:

- Reads with column projection, filter operators, boolean logic, ordering, and
  pagination.
- Writes: insert, update, upsert, and delete, with the PostgREST status and
  header rules.
- Resource embedding across foreign keys, so related rows nest in one request.
- Stored procedures exposed at `/rpc/<name>`.
- Stateless JWT authentication, with role resolution from a token claim.
- Authorization through table and column privileges and Row Level Security,
  emulated on engines that do not have it natively.
- A self-describing OpenAPI document at the root.

## Choosing a backend

Not every engine can serve every feature the same way. dbrest is honest about
this through a four-tier capability model: a feature is Native, Emulated,
Best-effort, or Unsupported on a given backend, and an Unsupported feature
returns a clear `PGRST127` error rather than a wrong answer. The
[capability model](/configuration/choosing-a-backend/) explains the tiers, and
each backend's page lists where it sits.

SQLite is the reference backend and is cgo-free, so it runs anywhere Go runs with
no server to stand up. It is the backend this guide uses for every example.

## Where to go next

- [Installation](/getting-started/installation/) builds the server.
- [Quick start](/getting-started/quick-start/) gets you querying in two minutes.
- Already use PostgREST? Jump to
  [migrating from PostgREST](/reference/migrating-from-postgrest/).
