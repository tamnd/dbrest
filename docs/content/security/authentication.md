---
title: "Authentication"
description: "Stateless JWT verification and role resolution."
weight: 10
---

Authentication is stateless bearer-token verification. A client sends a JSON Web
Token, dbrest verifies it, reads a role from a claim, and runs the request as
that role. There are no sessions and no database round trip to authenticate.

## Send a token

Pass the JWT as a bearer token:

```bash
curl 'localhost:3000/films' \
  -H 'Authorization: Bearer <token>'
```

A request with no token runs as the anonymous role, set by `db-anon-role`.

## Configure verification

Set the verification key with `jwt-secret`. dbrest verifies HMAC (`HS*`), RSA
(`RS*`), and ECDSA (`ES*`) signatures, and can fetch a key set from a JWKS
document or URL for asymmetric keys:

```ini
jwt-secret         = "your-256-bit-secret"
jwt-aud            = "your-audience"
jwt-role-claim-key = ".role"
```

The signing algorithm is pinned, and the `none` algorithm swap is refused, so a
token cannot downgrade itself to unsigned.

## Claims that are checked

dbrest validates the standard registered claims:

- `exp` (expiry), `nbf` (not before), and `iat` (issued at), each with a
  configurable clock-skew allowance.
- `aud` (audience) when `jwt-aud` is set.

An expired or not-yet-valid token, or one failing verification, is rejected
before the request touches the database.

## The role claim

The role comes from a token claim, named by `jwt-role-claim-key`, which supports
a nested JSON path. When the claim is absent, the request falls back to the
anonymous role.

The outcomes follow PostgREST:

- A malformed or unverifiable token is `PGRST301`.
- A token that verifies but is missing a required element is `PGRST302`.
- A role that exists but lacks privilege for the request is `403`, and an
  unauthenticated request that needed a privilege is `401`.

## The verification cache

Verified tokens are kept in a bounded cache so a repeated token is not
re-verified from scratch on every request. The cache is sized by
`jwt-cache-max-entries` (default 1000, `0` disables it) and uses a SIEVE
eviction policy. It never extends a token's lifetime: an entry past its `exp` is
not honored.

## Same on every backend

JWT verification, claim checking, and role resolution all run in the frontend,
before any backend call, so authentication behaves identically on every engine.
What differs per backend is what happens next, when the resolved role meets the
authorization stage, covered in [authorization and RLS](/security/authorization-rls/).
