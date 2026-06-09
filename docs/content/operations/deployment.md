---
title: "Deployment"
description: "Process model, health checks, schema reload, and containers."
weight: 10
---

dbrest is a single stateless process. It reads its configuration once at
startup, holds the schema cache in memory, and serves requests. Scale it
horizontally by running more copies behind a load balancer.

## Run the server

Point it at a config file:

```bash
dbrest -config /etc/dbrest/dbrest.conf
```

Or drive it entirely from the environment, which suits a container:

```bash
DBREST_DB_BACKEND=postgres \
DBREST_DB_URI='postgres://user:pass@db:5432/app?sslmode=require' \
DBREST_SERVER_PORT=3000 \
dbrest
```

## The admin server and health checks

Enable the admin server on its own port and keep it behind your network
boundary:

```ini
admin-server-port = 3001
```

It serves the health endpoints a load balancer needs:

- `GET /live` is liveness: the process is up and listening.
- `GET /ready` is readiness: the backend connection is healthy and the schema
  cache is built.
- `GET /metrics` is Prometheus-style metrics.

Route traffic on readiness, not liveness. A fresh instance is live before it is
ready, because it is only ready once the first schema cache has been built and
the backend pool reports a usable connection. Sending requests on liveness would
hit an instance whose cache is not yet loaded.

## Reload the schema without restarting

When the database schema changes, rebuild the cache in place rather than
restarting:

- Send `SIGUSR1` to the process. This works on every backend.
- Or call the reload endpoint on the admin server.
- On PostgreSQL, `NOTIFY pgrst, 'reload schema'` triggers it through the
  database, the same channel PostgREST uses.
- A TTL poll can catch out-of-band changes, using a cheap version check on
  SQLite and a change-stream check on MongoDB.

A failed reload leaves the previous cache in place and logs the error, so a bad
change does not take the server down.

## Connection pooling

Each backend owns its pool. Size it with `db-pool` and bound the wait for a
connection with `db-pool-acquisition-timeout`. These map onto each driver's own
pool knobs.

## Containers

The repository ships a `Dockerfile`. For local testing against a real engine,
`docker/` has a Podman compose file per backend and a `docker/all/` that runs
them together. MongoDB runs as a single-node replica set so its transaction
capability resolves the way a production deployment would.

```bash
podman compose -f docker/postgres/compose.yaml up -d
```

## Behind a reverse proxy

If dbrest runs behind a proxy that terminates TLS or rewrites the path, set
`openapi-server-proxy-uri` so the OpenAPI document advertises the public URL
rather than the internal one. Configure CORS with
`server-cors-allowed-origins`.
