---
title: "Migrating from PostgREST"
description: "What carries over unchanged, and the few things to check."
weight: 40
---

If you run PostgREST today, dbrest is meant to be a drop-in for the HTTP
contract. The URL grammar, operators, embedding, `Prefer` headers, error
envelopes, and OpenAPI root are the same. This page is the short list of what
carries over and what to check.

## What carries over unchanged

- **The API.** Every request your clients make against PostgREST works against
  dbrest: projection, the operator set, boolean trees, ordering with NULLS
  placement, `limit`/`offset` and `Range` pagination, the singular object type,
  resource embedding, `/rpc` calls, and content negotiation.
- **The errors.** The four-key envelope and the `PGRST` and `SQLSTATE` codes are
  the same.
- **The config names.** dbrest reads PostgREST option names, and accepts the
  `PGRST_*` environment spelling, so your existing config file and environment
  are a valid starting point. The native `DBREST_*` spelling exists too and wins
  when both are set.
- **Auth.** JWT verification, the role claim, the anon role, and the JWT cache
  behave as in PostgREST v14.

## What to set for dbrest

- **Pick a backend.** Add `db-backend` and point `db-uri` at it. On PostgreSQL
  there is nothing else to learn.
- **Mind the v14 baseline.** The compatibility target is PostgREST v14. If you
  are on an older PostgREST, note the v14 changes: `jwt-cache-max-entries`
  replaces the old `jwt-cache-max-lifetime`, `log-query` is a boolean, and the
  `/config` admin endpoint is gone.

## What to check when the backend is not PostgreSQL

On PostgreSQL, dbrest is the reference and nearly everything is Native. On the
other engines, a few features are Emulated, Best-effort, or Unsupported. Before
you switch, check the [capability model](/configuration/choosing-a-backend/) for:

- **Array and range operators** (`cs`, `cd`, `ov`, `sl`, `sr`, `adj`), which are
  Best-effort or Unsupported away from PostgreSQL.
- **Regex** (`match`, `imatch`), which is Unsupported on SQL Server.
- **Full text**, which is Best-effort on most engines and needs an index, for
  example an FTS5 index on SQLite.
- **NULLS ordering** and **upsert conflict targets**, which reach the same
  result by a different mechanism and are worth a spot check.

An Unsupported feature returns `PGRST127` naming the feature and backend, so you
find the boundary in testing rather than in production.

## What the backends still need

On engines without engine-side metadata, you supply it through configuration:

- MongoDB and foreign-key-less SQL schemas need
  [`declared-relationships`](/configuration/configuration/) for embedding, and
  MongoDB needs `declared-schema` for types and keys.
- SQLite and MongoDB need a [`function-registry`](/configuration/configuration/)
  for `/rpc` functions.
- The emulated backends need a [`policy-registry`](/configuration/configuration/)
  for privileges and RLS.

## A migration checklist

1. Stand dbrest up against your existing PostgreSQL with your current config and
   confirm your test suite passes unchanged.
2. If you are moving to another engine, read the capability model and run the
   conformance harness against that backend.
3. Supply the declared registries the target backend needs.
4. Re-point your clients. They should not need a change.
