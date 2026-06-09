---
title: "Full-text search and regex"
linkTitle: "Full-text and regex"
description: "The fts family and the match operators, and how they lower per backend."
weight: 60
---

dbrest parses the PostgREST full-text and regex operators identically on every
backend and lowers them to whatever the engine provides. Where an engine cannot
serve a query faithfully, the answer is a clean `PGRST127` rather than a silent
substring scan.

## Full-text operators

| Operator | Meaning |
| --- | --- |
| `fts` | full-text search |
| `plfts` | plain full-text search |
| `phfts` | phrase full-text search |
| `wfts` | web-style full-text search |

A full-text filter looks like any other filter:

```bash
curl 'localhost:3000/films?title=fts.little'
```

You can name a language configuration in parentheses. This is read as a
language, not as a quantifier:

```bash
curl 'localhost:3000/films?title=fts(english).woman'
```

## How full text lowers per backend

- **PostgreSQL** uses `tsvector` and the `@@` match, the native mechanism.
- **SQLite** lowers a full-text filter to an FTS5 `MATCH` against a virtual
  table that shadows the column. The FTS5 table and its shadow tables are hidden
  from the exposed schema. A column with no covering FTS5 index is a clean
  `PGRST127`, not a silent substring scan.
- **MySQL and MariaDB** use `MATCH ... AGAINST` in boolean mode.
- **SQL Server** uses `CONTAINS` and `FREETEXT`.
- **MongoDB** uses its text index.

The exact tier per backend is recorded in the
[capability model](/configuration/choosing-a-backend/). Most full-text cases are
Best-effort: close to PostgreSQL but not byte-identical, with the divergence
documented.

## Regex

| Operator | Meaning |
| --- | --- |
| `match` | regular expression match |
| `imatch` | case-insensitive regular expression match |

```bash
curl 'localhost:3000/films?title=match.^L'
```

On SQLite, regex lowers to a registered RE2 `regexp()` function. RE2 is fast and
linear-time but does not support every PCRE feature. A pattern that uses
something RE2 lacks, such as a backreference or lookaround, is rejected up front
with `PGRST127` instead of failing inside the engine:

```json
{ "code": "PGRST127", "message": "...not implemented...", "details": "...", "hint": "..." }
```

On MySQL and MariaDB regex is `REGEXP_LIKE` and Native. On SQL Server regex is
Unsupported, so `match` and `imatch` there return `PGRST127`. Check the
[capability model](/configuration/choosing-a-backend/) before relying on regex
for a given engine.
