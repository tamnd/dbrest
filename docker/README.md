# Local database engines for development

These compose files bring up each engine dbrest targets, for local development
and testing with Podman. One file per engine under `docker/<engine>/compose.yaml`,
plus `docker/all/` to run them all together.

The examples use `podman compose`; `podman-compose` and `docker compose` read the
same files. Each file is self-contained: its own named volume for persistence and
a healthcheck so you can tell when the server is ready.

## State of play

The dialect and lowering layer for every engine has landed (`backend/postgres`,
`backend/mysql`, `backend/sqlserver`, `backend/mongo`), but the driver data plane
that runs queries over a live connection is a follow-on slice for each. Today the
only engine wired all the way through `cmd/dbrest` is SQLite, which needs no
server:

```
db-backend = "sqlite"
db-uri     = "file:dev.db"
```

So these compose files are here now to back the data-plane slices as they land
and to give the conformance harness (spec 22) real engines to diff against. Until
a backend's data plane is built in, selecting it in `cmd/dbrest` is a clear
startup error, not a silent fallback.

## One engine

```
podman compose -f docker/postgres/compose.yaml up -d
podman compose -f docker/postgres/compose.yaml down        # add -v to drop data
```

| Engine | Image | Host port | `db-backend` | `db-uri` |
|--------|-------|-----------|--------------|----------|
| PostgreSQL | `postgres:16` | 5432 | `postgres` | `postgres://dbrest:Dbrest!Passw0rd@localhost:5432/dbrest` |
| MySQL | `mysql:8.4` | 3306 | `mysql` | `dbrest:Dbrest!Passw0rd@tcp(localhost:3306)/dbrest` |
| MariaDB | `mariadb:11.4` | 3307 | `mysql` | `dbrest:Dbrest!Passw0rd@tcp(localhost:3307)/dbrest` |
| SQL Server | `mssql/server:2022-latest` | 1433 | `sqlserver` | `sqlserver://sa:Dbrest!Passw0rd@localhost:1433?database=dbrest` |
| MongoDB | `mongo:7` | 27017 | `mongodb` | `mongodb://localhost:27017/dbrest?replicaSet=rs0` |

The shared credentials are user `dbrest`, password `Dbrest!Passw0rd`, database
`dbrest`. The password satisfies SQL Server's complexity policy, so it is reused
everywhere for simplicity. These are local development defaults; do not carry them
into anything reachable.

## All engines

```
podman compose -f docker/all/compose.yaml up -d
podman compose -f docker/all/compose.yaml down -v
```

The host ports were chosen not to collide, so every engine can run at once.
MariaDB sits on 3307 to stay clear of MySQL on 3306.

## Notes per engine

- **MariaDB vs MySQL.** Both use the `mysql` backend, but the capability profile
  differs: MariaDB 10.5+ has a native `RETURNING`, which the dialect grades Native
  where stock MySQL is Emulated. Running both lets you exercise both gate paths.

- **SQL Server has no application database on first start.** Create it once the
  server is healthy:

  ```
  podman exec dbrest-sqlserver /opt/mssql-tools18/bin/sqlcmd \
    -S localhost -U sa -P 'Dbrest!Passw0rd' -C -Q 'CREATE DATABASE dbrest'
  ```

  The image is SQL Server 2022, the floor where JSON assembly and
  `IS DISTINCT FROM` are Native. `REGEXP_LIKE` is a 2025 feature, so on this image
  the regex operators stay Unsupported and are rejected with PGRST127 before
  lowering; move to a 2025 image to test that path.

- **MongoDB runs as a single-node replica set.** The capability gate gives
  `Transactions = TxFull` only on a replica set or sharded cluster, so a lone
  `mongod` would report `TxNone`. The healthcheck initiates the set on first start,
  so transactions and `tx=rollback` work without an extra step. Authentication is
  off, which is why a keyFile (required for an authenticated replica set) is not
  configured.
