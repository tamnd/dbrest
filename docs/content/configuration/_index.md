---
title: "Configuration"
linkTitle: "Configuration"
description: "Options, backend selection, and the capability model."
weight: 20
featured: true
---

dbrest reads its configuration once at startup and otherwise stays stateless.
The option surface follows PostgREST: the same names and meanings where they
carry over, with a few additions for picking a backend and filling gaps on
engines that lack engine-side metadata. These pages cover the options, how to
choose and connect a backend, and the capability model that says what each
engine can do.
