---
title: "Content negotiation"
description: "JSON, the singular object type, CSV, and scalar responses."
weight: 10
---

dbrest chooses a response format from the `Accept` header, the same way
PostgREST does. JSON is the default; a few other types are available for
specific needs.

## JSON

With no `Accept` header, or with `Accept: application/json`, a read returns an
array of objects:

```bash
curl 'localhost:3000/films?select=title,year'
# [ { "title": "...", "year": 2019 }, ... ]
```

## A single object

`application/vnd.pgrst.object+json` returns one object instead of an array:

```bash
curl 'localhost:3000/films?id=eq.1' \
  -H 'Accept: application/vnd.pgrst.object+json'
```

If the request matches zero rows or more than one, it is a `PGRST116` error,
because the singular type asked for exactly one row. This is the same media type
that scopes writes to a single returned row.

## CSV

`text/csv` returns rows as CSV, with a header line:

```bash
curl 'localhost:3000/films?select=title,year' \
  -H 'Accept: text/csv'
# title,year
# Parasite,2019
# ...
```

## Scalar responses

When a query projects a single scalar, you can ask for it raw rather than wrapped
in JSON:

- `application/octet-stream` returns the value as bytes.
- `text/plain` returns it as text.

These are useful for a function or a single-column read whose value you want to
stream directly.

## The OpenAPI root

`GET /` returns the OpenAPI document as `application/openapi+json`. That is its
own topic; see [OpenAPI](/reference/openapi/).
