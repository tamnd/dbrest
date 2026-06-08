-- Roles that PostgREST requires: an authenticator that can switch to limited
-- roles, an anonymous role for unauthenticated requests, and an application
-- role for authenticated users. dbrest mirrors exactly the same setup so both
-- servers run against an identical role hierarchy and the conformance harness
-- can compare them on equal footing.

-- The authenticator connects and immediately drops privileges.
CREATE ROLE authenticator NOINHERIT LOGIN PASSWORD 'authenticator_pass';

-- web_anon is the role for unauthenticated requests.
CREATE ROLE web_anon NOLOGIN;
GRANT web_anon TO authenticator;

-- web_user is the role for JWT-authenticated requests.
CREATE ROLE web_user NOLOGIN;
GRANT web_user TO authenticator;
