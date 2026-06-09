---
title: "Request context"
description: "The claims, headers, and metadata a policy or function can read, and the response controls it can set."
weight: 30
---

Every request carries a backend-neutral context: the verified claims, the
request headers and cookies, the method, the path, and the resolved role. A
policy or function reads from it, and can write back response controls. In
PostgreSQL these are GUCs (the `request.*` settings); dbrest carries the same
information on every backend.

## What the context holds

- The verified JWT claims.
- The request headers and cookies.
- The HTTP method and path.
- The resolved role.

On PostgreSQL these are exposed as the GUC settings a policy or function reads
with `current_setting`. dbrest serializes the claims, headers, and cookies to
the same JSON a native backend writes verbatim. On an emulated backend, the
values a policy needs are bound as query parameters, so a policy predicate can
reference the current role or a claim without the engine having a GUC mechanism.

## The pre-request hook

`db-pre-request` names a function that runs before the main query and can mutate
the request context. It is the place to derive a value, enforce a cross-cutting
rule, or set a response header for every request. On PostgreSQL it is a native
function; on the other backends it is a registered function in the
[`function-registry`](/configuration/configuration/).

## Response controls

A function or policy can influence the response, and dbrest applies these
uniformly across reads, writes, and RPC:

- A status override, to return a specific HTTP status.
- Added response headers.

This is how a function can, for example, set a `Location` or a cache header, or
signal a particular status, and have it take effect the same way whether the
request was a read, a write, or an RPC call.

## Why it is backend-neutral

Because the context is assembled in the frontend and handed to the backend in a
neutral form, a policy written against the request role or a claim behaves the
same on SQLite as on PostgreSQL. The engine that lacks GUCs gets the same values
as bound parameters, and the engine that has them gets the JSON it expects. The
contract is the information available to a policy, not the mechanism that
carries it.
