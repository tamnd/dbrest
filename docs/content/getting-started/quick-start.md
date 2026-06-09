---
title: "Quick start"
description: "A running API over a SQLite file, and your first queries, in two minutes."
weight: 30
---

This page stands up a real API over a SQLite file and runs the first few
queries. Every other example in this guide uses the same database, so it is
worth a couple of minutes to set up.

## Create the example database

Save this schema and seed data, and load it into a SQLite file. If you have the
`sqlite3` CLI:

```bash
cat > seed.sql <<'EOF'
CREATE TABLE directors (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE films (
  id          INTEGER PRIMARY KEY,
  title       TEXT NOT NULL,
  year        INTEGER NOT NULL,
  rating      REAL,
  director_id INTEGER REFERENCES directors(id)
);

INSERT INTO directors (id, name) VALUES
  (1, 'Bong Joon-ho'),
  (2, 'Greta Gerwig'),
  (3, 'Denis Villeneuve');

INSERT INTO films (id, title, year, rating, director_id) VALUES
  (1, 'Parasite',     2019, 8.5, 1),
  (2, 'Lady Bird',    2017, 7.4, 2),
  (3, 'Little Women', 2019, 7.8, 2),
  (4, 'Dune',         2021, 8.0, 3),
  (5, 'Arrival',      2016, 7.9, 3);
EOF

sqlite3 example.sqlite < seed.sql
```

No `sqlite3` binary? Any tool that writes a SQLite file works, and dbrest will
also create the file if you prefer to insert rows over HTTP later.

## Write a config file

dbrest reads a flat PostgREST-style config file. Name the backend and the
database:

```bash
cat > dbrest.conf <<'EOF'
db-backend  = "sqlite"
db-uri      = "file:./example.sqlite"
server-port = 3000
EOF
```

## Start the server

```bash
go run ./cmd/dbrest -config dbrest.conf
```

The same options are settable from the environment with no file at all:

```bash
DBREST_DB_URI='file:./example.sqlite' DBREST_SERVER_PORT=3000 go run ./cmd/dbrest
```

## Query it

Every table is now a resource. Ask for all films:

```bash
curl 'localhost:3000/films'
```

Project columns, filter, order, and paginate, all from the query string:

```bash
curl 'localhost:3000/films?select=title,year&year=gte.2019&order=year.desc&limit=10'
```

Ask for a single object instead of an array:

```bash
curl 'localhost:3000/films?id=eq.1' \
  -H 'Accept: application/vnd.pgrst.object+json'
```

Nest a related resource in the same request, resolved through the foreign key:

```bash
curl 'localhost:3000/films?select=title,directors(name)&order=title'
```

## Two rules worth knowing now

An empty match is an empty array with `200`, never a `404`:

```bash
curl -i 'localhost:3000/films?id=eq.999'
# HTTP/1.1 200 OK
# []
```

A name that is not in the schema is a PostgREST error envelope, not an HTML
page:

```json
{ "code": "PGRST205", "message": "...", "details": null, "hint": null }
```

## Next

- [Reading data](/querying/reading-data/) goes deep on projection, filtering,
  ordering, and pagination.
- [Writing data](/querying/writing-data/) covers insert, update, upsert, and
  delete.
- [Configuration](/configuration/configuration/) lists every option and how the
  file and environment combine.
