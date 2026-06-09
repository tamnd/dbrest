---
title: "OpenAPI"
description: "The self-describing Swagger 2.0 document at the root."
weight: 30
---

`GET /` returns a self-describing OpenAPI document, the same Swagger 2.0
document PostgREST emits, served as `application/openapi+json`:

```bash
curl 'localhost:3000/' \
  -H 'Accept: application/openapi+json'
```

## What it contains

The document is built from the schema model and the function registry:

- A path and a definition per relation.
- The read and write operation set for each.
- The `/rpc/<name>` paths, organized by volatility.
- Primary-key and foreign-key notes.
- A JWT security scheme when authentication is configured.

## It never over-promises

Each column advertises only the filter operators the active backend can actually
serve, by consulting the [capability model](/configuration/choosing-a-backend/).
The document never lists a feature that the next request would reject with
`PGRST127`. So a client that generates calls from the spec stays within what the
running backend supports.

## Configuration

A few options shape the root:

- `openapi-mode` controls how privileges shape the spec: `follow-privileges`,
  `ignore-privileges`, or `disabled` to turn the root off entirely.
- `openapi-server-proxy-uri` rewrites the advertised host and base path, for a
  service running behind a reverse proxy.
- `db-root-spec` names a function returning a custom root document, which
  overrides the generated one.

```ini
openapi-mode             = "follow-privileges"
openapi-server-proxy-uri = "https://api.example.com/v1"
```
