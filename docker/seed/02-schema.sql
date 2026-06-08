-- Conformance schema: canonical tables the compat test harness uses.
-- Every table is in the "api" schema so PostgREST and dbrest both expose it
-- without touching "public". The web_anon role gets read-only access;
-- web_user gets full write access. This mirrors the PostgREST tutorial setup
-- so the compat suite can run unmodified PostgREST tutorial requests against
-- both servers and diff the responses.

CREATE SCHEMA IF NOT EXISTS api;

CREATE TABLE api.todos (
    id      serial  PRIMARY KEY,
    done    boolean NOT NULL DEFAULT false,
    task    text    NOT NULL,
    due     date
);

CREATE TABLE api.persons (
    id        serial PRIMARY KEY,
    name      text   NOT NULL,
    age       int,
    email     text   UNIQUE
);

CREATE TABLE api.assignments (
    id        serial  PRIMARY KEY,
    person_id int     REFERENCES api.persons(id) ON DELETE CASCADE,
    todo_id   int     REFERENCES api.todos(id)   ON DELETE CASCADE,
    UNIQUE (person_id, todo_id)
);

-- web_anon can read and write everything in api for the compat test suite.
-- (A production deployment would keep web_anon read-only and add JWT auth for
-- writes, but the compat tests run without a JWT secret so we open writes here
-- to allow both servers to return identical 201/204 on the write test cases.)
GRANT USAGE  ON SCHEMA api          TO web_anon;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA api TO web_anon;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA api TO web_anon;

-- web_user can read and write.
GRANT USAGE  ON SCHEMA api          TO web_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA api TO web_user;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA api TO web_user;
