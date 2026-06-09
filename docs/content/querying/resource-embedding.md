---
title: "Resource embedding"
description: "Nest related resources in one request through foreign keys."
weight: 30
---

Resource embedding nests a related resource inside `select`, resolved through
the foreign keys dbrest introspects. One request returns a film and its
director together, with no second round trip and no client-side join.

## Embed a related row

The films table has a `director_id` foreign key to `directors`. Embed the
director by naming the related table inside `select`:

```bash
curl 'localhost:3000/films?select=title,directors(name)&order=title'
```

Each film object now carries a nested `directors` object:

```json
[
  { "title": "Arrival", "directors": { "name": "Denis Villeneuve" } },
  { "title": "Dune", "directors": { "name": "Denis Villeneuve" } }
]
```

## Choose the embedded columns

The parentheses take their own `select` list, including aliases:

```bash
curl 'localhost:3000/films?select=title,directors(director:name)'
```

## Embed in the other direction

Embedding works both ways. From a director, embed the related films:

```bash
curl 'localhost:3000/directors?select=name,films(title,year)&order=name'
```

A one-to-many embed nests an array:

```json
[
  { "name": "Greta Gerwig", "films": [
      { "title": "Lady Bird", "year": 2017 },
      { "title": "Little Women", "year": 2019 }
  ] }
]
```

## Filter and order on an embed

Filters and ordering can target an embedded resource by qualifying the column
with the embed name:

```bash
# directors, with only their films from 2019, newest first
curl 'localhost:3000/directors?select=name,films(title,year)&films.year=eq.2019&films.order=year.desc'
```

## When a relationship is missing or ambiguous

Embedding is resolved against introspected foreign keys. If you name a relation
that has no foreign key linking it, you get `PGRST200`. If more than one foreign
key could satisfy the embed and the request does not disambiguate, you get
`PGRST201`, which lists the candidates so you can pick one.

```json
{ "code": "PGRST200", "message": "...", "details": "...", "hint": "..." }
```

## How it works per backend

On the relational backends, an embed becomes a join and the nested JSON is
assembled in the engine. On MongoDB it becomes a `$lookup` or `$graphLookup`
pipeline stage. The result shape is identical either way.

Backends without real foreign keys, such as MongoDB or a foreign-key-less SQL
schema, get their relationships from the
[`declared-relationships`](/configuration/configuration/) registry, which
records the same `from`, `to`, matching columns, and cardinality the
introspector would otherwise read from a catalog.
