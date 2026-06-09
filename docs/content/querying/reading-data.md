---
title: "Reading data"
description: "Projection, ordering, pagination, and counting with GET."
weight: 10
---

Every table and view is a resource at its own path. A `GET` returns rows as a
JSON array. These examples use the films database from the
[quick start](/getting-started/quick-start/).

## Select all rows

```bash
curl 'localhost:3000/films'
```

Returns every column of every row as an array of objects. An empty result is
`[]` with `200`, never a `404`.

## Project columns

Use `select` to choose columns:

```bash
curl 'localhost:3000/films?select=title,year'
```

Rename a column in the output with `alias:column`:

```bash
curl 'localhost:3000/films?select=name:title,released:year'
```

## Filter rows

Filters are query parameters of the form `column=operator.value`:

```bash
# films from 2019 or later
curl 'localhost:3000/films?year=gte.2019'

# one film by id
curl 'localhost:3000/films?id=eq.1'
```

The full set of operators, boolean logic, and value syntax is on the
[operators](/querying/operators/) page.

## Order

`order` takes one or more columns, each with an optional direction and NULL
placement:

```bash
# highest rated first
curl 'localhost:3000/films?order=rating.desc'

# year ascending, then title
curl 'localhost:3000/films?order=year.asc,title'
```

NULL placement follows PostgreSQL: `nullsfirst` and `nullslast`.

```bash
curl 'localhost:3000/films?order=rating.desc.nullslast'
```

## Paginate

`limit` and `offset` page through a result:

```bash
curl 'localhost:3000/films?order=year&limit=2&offset=2'
```

The response carries a `Content-Range` header, and a partial page comes back as
`206 Partial Content` rather than `200`:

```bash
curl -i 'localhost:3000/films?order=year&limit=2'
# HTTP/1.1 206 Partial Content
# Content-Range: 0-1/*
```

You can also page with the `Range` header, the same way PostgREST does.

## A single object

By default a read returns an array even when it matches one row. Ask for a
single object with the singular media type:

```bash
curl 'localhost:3000/films?id=eq.1' \
  -H 'Accept: application/vnd.pgrst.object+json'
```

This returns one object, not an array. If the filter matches zero rows or more
than one, you get a `PGRST116` error, because the request asked for exactly one.

## Count

Ask for the total row count with the `Prefer: count=exact` header. The count
appears in `Content-Range` after the slash:

```bash
curl -i 'localhost:3000/films?limit=2' \
  -H 'Prefer: count=exact'
# Content-Range: 0-1/5
```

`count=planned` and `count=estimated` trade accuracy for speed on large tables.
Their fidelity per backend is noted in the capability model.

## Next

- [Operators](/querying/operators/) is the complete filtering reference.
- [Resource embedding](/querying/resource-embedding/) nests related rows.
- [Content negotiation](/reference/content-negotiation/) covers CSV and the
  other response types.
