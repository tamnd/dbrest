---
title: "Security"
linkTitle: "Security"
description: "JWT authentication, authorization, RLS, and the request context."
weight: 40
featured: true
---

dbrest reproduces the PostgREST security model: a request carries a JWT, the
token resolves to a role, and that role's privileges and Row Level Security
policies decide what it may read and write. On engines without native roles or
RLS, dbrest emulates them so the observable behavior matches. These pages cover
authentication, authorization, and the request context a policy or function
sees.
