---
title: "dbrest"
description: "A REST server that speaks the PostgREST API on top of any database."
---

dbrest serves the [PostgREST](https://postgrest.org) HTTP API, the same URL
grammar, operators, resource embedding, `Prefer` headers, error envelopes, and
OpenAPI root, on top of a database you choose. Point it at PostgreSQL, SQLite,
MySQL, SQL Server, or MongoDB. A client written against PostgREST should not be
able to tell the difference.

This guide takes you from a running server in two minutes to the details of
querying, writing, securing, and operating it. If you already know PostgREST,
the [migration page](/reference/migrating-from-postgrest/) is the fastest way
in: nearly everything you know carries over, and this guide marks the few places
where a backend cannot serve a feature.

Every example here runs against the SQLite quick start, so you can follow along
with nothing more than Go installed.
