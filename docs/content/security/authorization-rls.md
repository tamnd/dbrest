---
title: "Authorization and RLS"
description: "Table and column privileges, and Row Level Security that a client cannot bypass."
weight: 20
---

Once a request has a role, authorization decides what that role may see and do.
dbrest reproduces PostgREST's two layers: table and column privileges, and Row
Level Security policies. On engines that enforce these natively, dbrest passes
through. On engines that do not, it emulates them with the same observable
result.

## Table and column privileges

Privileges gate every read and write. A role with no privilege on a table gets
`403`, or `401` if the request was unauthenticated. The check is per column as
well as per table:

- A read of a table the role cannot select is `403` (mapped from the engine's
  `42501`).
- A `*` projection is narrowed to the columns the role may actually see, so a
  `select=*` never leaks an ungranted column.

## Row Level Security

Row Level Security restricts which rows a role may see or change, beyond which
tables. A policy is a predicate that is applied to every query for that role.

The key property is that a client cannot escape a policy. The policy predicate
is injected as a bound condition AND-ed above the entire client filter tree. A
client cannot OR its way past it: even a request like
`?or=(...)` is still wrapped by the policy's AND, so the policy always holds.

For writes, a policy's `WITH CHECK` condition is validated before any row is
written, so a write that would create a row the role is not allowed to see is
rejected rather than committed.

## Native versus emulated

Whether this runs in the engine or in dbrest depends on the backend:

- **PostgreSQL** and **SQL Server** have native roles and Row Level Security.
  dbrest sets the role and lets the engine enforce policies.
- **SQLite**, **MySQL/MariaDB**, and **MongoDB** have no native RLS. dbrest
  emulates it: the column gate narrows projections, and the policy predicate is
  injected into the query before it runs. On the emulated backends this comes
  from the [`policy-registry`](/configuration/configuration/) option, which
  records per-role table and column grants and per-role predicate and
  with-check rules.

The injection happens in the frontend, so by the time the backend executes, the
policy is already part of the query. A read sees only the rows the role may see,
on every backend, by the same contract.

## Putting it together

A request flows: verify the token, resolve the role, gate the columns, inject
the policy predicate, then execute. Because the gate and the injection happen
before execution, an emulated backend reaches the same answer a native one does.
The values a policy needs at runtime, the role, the verified claims, and the
request metadata, are carried on the request context, covered next.
