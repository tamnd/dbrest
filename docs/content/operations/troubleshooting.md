---
title: "Troubleshooting"
description: "The errors you are most likely to hit, and what they mean."
weight: 20
---

Most problems surface as a clear error envelope. This page maps the common ones
to a cause and a fix. The [errors](/reference/errors/) page is the full code
list.

## "Not implemented" (PGRST127)

The feature you used is Unsupported on the active backend. The message names the
feature and the backend. This is by design: dbrest returns it rather than a
wrong answer. Check the [capability model](/configuration/choosing-a-backend/);
often another backend serves the feature Native, or there is an
equivalent that is supported. A common case is regex on SQL Server, or array and
range operators away from PostgreSQL.

## "Relation not found" (PGRST205)

The name in the path is not in the exposed schema. Check that:

- The name is spelled correctly and is a table, view, or collection.
- It lives in a schema listed in `db-schemas`. Only exposed schemas are
  introspected.
- The schema cache is current. If you changed the schema, reload it (see
  [deployment](/operations/deployment/)).

## "Could not find a relationship" (PGRST200 / PGRST201)

An embed could not be resolved. `PGRST200` means there is no foreign key linking
the two relations; `PGRST201` means there is more than one and the request is
ambiguous. On backends without real foreign keys, add the link to
[`declared-relationships`](/configuration/configuration/). For an ambiguous
embed, disambiguate by naming the specific relationship.

## "No function found" (PGRST202)

No registered function matched the name and argument signature at `/rpc`. Check
the argument names and that the function is in the
[`function-registry`](/configuration/configuration/) on backends without native
stored procedures. Remember the verb rule: a read-only function answers `GET`, a
volatile one needs `POST`, and a `GET` to a volatile function is `405`.

## A type error on a filter (22P02)

A value in the query string does not match the column's type, for example a
non-integer on an integer column. dbrest catches this in the frontend and
returns `400` before the query runs. Fix the value, or cast the column
explicitly if you meant a different type. See
[types and casts](/querying/types-and-casts/).

## Auth failures (PGRST301 / PGRST302 / 401 / 403)

- `PGRST301`: the token is malformed or fails verification. Check `jwt-secret`
  and the signing algorithm.
- `PGRST302`: the token verified but is missing a required element, for example
  the audience when `jwt-aud` is set.
- `401`: the request was unauthenticated and needed a privilege. Send a token.
- `403`: the role is authenticated but lacks privilege for the request. Check
  the grants for that role.

See [authentication](/security/authentication/) and
[authorization and RLS](/security/authorization-rls/).

## An empty result returns 200, not 404

This is intended. An empty match is `[]` with `200`. A `404` only happens for a
path that is not a resource at all. If you expected exactly one row, use the
singular object type and handle `PGRST116` when zero or many rows match.

## The server will not start

Configuration is typed and validated before the listener opens, so a bad option
fails loudly at startup. Read the error: an unknown key, an out-of-range port,
an invalid mode, or a `db-uri` the backend cannot parse will each say so. A
known but unbuilt backend is a clear startup error; an unknown backend is a
validation error.
