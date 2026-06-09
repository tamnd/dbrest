---
title: "Choosing a backend"
description: "The four-tier capability model, and how to read the matrix."
weight: 20
---

Not every engine can serve every PostgREST feature the same way. dbrest is
honest about this through a capability model. The frontend never branches on an
engine by name; it reads the capabilities a backend publishes and degrades or
rejects accordingly.

## The four tiers

Every feature sits in one of four tiers on a given backend.

- **Native.** The engine does what PostgreSQL does. dbrest lowers the request
  and passes through.
- **Emulated.** The engine lacks the feature, so dbrest builds an equivalent
  that produces the same observable HTTP behavior, including status and body.
- **Best-effort.** An approximation with a documented divergence. The behavior
  is close but not byte-identical, and the difference is recorded.
- **Unsupported.** No faithful equivalent exists. A request that needs it
  returns `PGRST127` (`400`, "not implemented") naming the feature and backend.
  It is never silently wrong.

The governing rule: match the observable HTTP behavior even when the mechanism
differs, and when that is impossible on a backend, fail loudly.

## Tiers are computed from the live server

A backend's capabilities are computed once when it connects, from the actual
server version. A feature gated on, say, MariaDB 10.5 or SQL Server 2022
resolves to the right tier for the server you actually pointed at. The same
backend can publish a different tier against a different version.

## How to read the matrix

The full capability matrix lives in the project design spec and is the single
source of truth. The shape worth carrying in your head:

- **PostgreSQL** is the reference. Almost everything is Native.
- **SQLite** is the cgo-free reference backend used throughout this guide.
  Reads, writes, embedding, RPC through the function registry, and the security
  plane are all in place. Full-text search lowers to FTS5. Array and range
  operators are Best-effort or Unsupported.
- **MySQL and MariaDB** reach the same behavior by different mechanisms: an
  explicit `IS NULL` sort key for NULL placement, a no-conflict-target upsert,
  restricted cast targets, `REGEXP_LIKE`, and `MATCH ... AGAINST` full text.
- **SQL Server** is the quirkiest on syntax and the closest to PostgreSQL on
  the security model: bracket-quoted identifiers, `OFFSET`/`FETCH` paging,
  `OUTPUT` in place of `RETURNING`, native roles and Row Level Security, and
  `CONTAINS`/`FREETEXT` full text. Regex is Unsupported.
- **MongoDB** does not use the SQL compiler at all. It lowers a filter to a
  `$match`, a read to an aggregation pipeline, casts to `$convert`, and
  embedding to `$lookup`. Array and range operators are Unsupported, and the
  security model is emulated app-side.

## What this means for you

Pick the engine your data already lives in. The API contract is the same on all
of them. Where a backend cannot serve something, you get a clear `PGRST127`
rather than a wrong answer, so you can see the boundary in development instead
of discovering it in production. The pages in this guide note, feature by
feature, where a backend diverges.

For connection strings and per-backend status, see the
[backend reference](/configuration/backend-reference/).
