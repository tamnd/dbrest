---
title: "Operators"
description: "The full filter operator set, boolean logic, and value syntax."
weight: 20
---

A filter is a query parameter of the form `column=operator.value`. The operator
set is fixed and backend-neutral: the frontend parses each filter into a
canonical operator, and the backend lowers it to its engine. The examples use
the films database from the [quick start](/getting-started/quick-start/).

## Comparison

| Operator | Meaning | Example |
| --- | --- | --- |
| `eq` | equal | `id=eq.1` |
| `neq` | not equal | `year=neq.2019` |
| `gt` | greater than | `rating=gt.8` |
| `gte` | greater than or equal | `year=gte.2019` |
| `lt` | less than | `rating=lt.8` |
| `lte` | less than or equal | `year=lte.2017` |

```bash
curl 'localhost:3000/films?rating=gte.8&order=rating.desc'
```

## Pattern and regex

| Operator | Meaning |
| --- | --- |
| `like` | pattern match, `*` is the wildcard |
| `ilike` | case-insensitive pattern match |
| `match` | regular expression |
| `imatch` | case-insensitive regular expression |

```bash
# titles starting with "L"
curl 'localhost:3000/films?title=like.L*'

# case-insensitive
curl 'localhost:3000/films?title=ilike.little*'
```

`like` uses `*` as the wildcard, which the frontend translates to the engine's
own wildcard. Regex (`match`, `imatch`) and full text are covered in detail on
the [full-text search](/querying/full-text-search/) page. Regex availability
differs by backend; an unsupported pattern is a clean `PGRST127` rather than a
silent wrong answer.

## Membership and null

| Operator | Meaning | Example |
| --- | --- | --- |
| `in` | value is one of a list | `id=in.(1,2,3)` |
| `is` | `null`, `true`, `false`, `unknown`, or `not_null` | `rating=is.null` |
| `isdistinct` | null-safe inequality | `rating=isdistinct.8` |

```bash
curl 'localhost:3000/films?id=in.(1,3,5)'

curl 'localhost:3000/films?rating=is.not_null'
```

## Full-text and array operators

| Operator | Meaning |
| --- | --- |
| `fts`, `plfts`, `phfts`, `wfts` | full-text search variants |
| `cs`, `cd`, `ov` | array or range contains, contained by, overlap |
| `sl`, `sr`, `nxr`, `nxl`, `adj` | range position |

Full text is on its [own page](/querying/full-text-search/). The array and range
operators are Native on PostgreSQL and Best-effort or Unsupported elsewhere; see
[choosing a backend](/configuration/choosing-a-backend/).

## Negation

Prefix any operator with `not.` to negate it:

```bash
# every film not from 2019
curl 'localhost:3000/films?year=not.eq.2019'
```

## Boolean logic

Combine filters with `and`, `or`, and `not`. Top-level filters are already
AND-ed together:

```bash
# year >= 2018 AND rating >= 8 (two filters, implicitly AND)
curl 'localhost:3000/films?year=gte.2018&rating=gte.8'
```

For `or`, or for nesting, use the tree form. The operands go in parentheses:

```bash
# rating >= 8 OR year = 2016
curl 'localhost:3000/films?or=(rating.gte.8,year.eq.2016)'
```

`and`, `or`, and `not` nest arbitrarily:

```bash
curl 'localhost:3000/films?and=(year.gte.2017,or(rating.gte.8,title.like.A*))'
```

## Quantified modifiers

`(any)` and `(all)` apply an operator across a set, for example matching a
column against any of several patterns:

```bash
curl 'localhost:3000/films?title=like(any).{L*,A*}'
```

These are Native on the relational backends and Emulated on MongoDB.

## Values and types

A value in the query string is coerced against the column's type in the
frontend, before the query reaches the engine, so the result is identical on
every backend. A non-integer on an integer column is a clean `22P02` (`400`)
up front:

```bash
curl -i 'localhost:3000/films?id=eq.abc'
# HTTP/1.1 400 Bad Request
# { "code": "22P02", ... }
```

Patterns, the `is` keywords, and text columns are left as written. See
[types and casts](/querying/types-and-casts/) for the canonical type surface and
how to cast explicitly.
