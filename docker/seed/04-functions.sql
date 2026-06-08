-- RPC functions and RLS objects for the compat test suite (groups 16-19 in
-- the test matrix). Both the PostgREST and dbrest stacks load this file.

-- Stable: returns the count of todos. Used to test stable-function GET + POST.
CREATE OR REPLACE FUNCTION api.get_todos_count()
RETURNS integer LANGUAGE sql STABLE SECURITY INVOKER AS $$
  SELECT count(*)::integer FROM api.todos;
$$;
GRANT EXECUTE ON FUNCTION api.get_todos_count() TO web_anon, web_user;

-- Volatile: inserts a todo and returns it. Tests volatile function + tx=rollback.
CREATE OR REPLACE FUNCTION api.add_todo(task text)
RETURNS api.todos LANGUAGE sql VOLATILE SECURITY INVOKER AS $$
  INSERT INTO api.todos (task) VALUES (task) RETURNING *;
$$;
GRANT EXECUTE ON FUNCTION api.add_todo(text) TO web_user;

-- Context readers: prove request context GUCs arrive correctly.
CREATE OR REPLACE FUNCTION api.get_request_method()
RETURNS text LANGUAGE sql STABLE SECURITY INVOKER AS $$
  SELECT current_setting('request.method', true);
$$;
GRANT EXECUTE ON FUNCTION api.get_request_method() TO web_anon, web_user;

CREATE OR REPLACE FUNCTION api.get_request_path()
RETURNS text LANGUAGE sql STABLE SECURITY INVOKER AS $$
  SELECT current_setting('request.path', true);
$$;
GRANT EXECUTE ON FUNCTION api.get_request_path() TO web_anon, web_user;

CREATE OR REPLACE FUNCTION api.get_jwt_claims()
RETURNS json LANGUAGE sql STABLE SECURITY INVOKER AS $$
  SELECT COALESCE(current_setting('request.jwt.claims', true), '{}')::json;
$$;
GRANT EXECUTE ON FUNCTION api.get_jwt_claims() TO web_anon, web_user;

-- Custom status: RAISE PT204 drives a 204 response.
CREATE OR REPLACE FUNCTION api.raise_204()
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY INVOKER AS $$
BEGIN
  RAISE SQLSTATE 'PT204';
END;
$$;
GRANT EXECUTE ON FUNCTION api.raise_204() TO web_user;

-- Custom header: sets response.headers GUC.
CREATE OR REPLACE FUNCTION api.raise_custom_header()
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY INVOKER AS $$
BEGIN
  PERFORM set_config('response.headers', '[{"X-Custom-Header":"hello"}]', true);
END;
$$;
GRANT EXECUTE ON FUNCTION api.raise_custom_header() TO web_user;

-- RLS table: each row is owned by a role; a policy hides other owners.
CREATE TABLE IF NOT EXISTS api.private_todos (
  id     serial PRIMARY KEY,
  owner  text   NOT NULL,
  task   text   NOT NULL
);
ALTER TABLE api.private_todos ENABLE ROW LEVEL SECURITY;
ALTER TABLE api.private_todos FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_owner ON api.private_todos;
CREATE POLICY rls_owner ON api.private_todos
  USING (owner = current_setting('request.jwt.claims', true)::json ->> 'role');

GRANT SELECT         ON api.private_todos           TO web_anon, web_user;
GRANT INSERT, UPDATE ON api.private_todos           TO web_user;
GRANT USAGE, SELECT  ON api.private_todos_id_seq    TO web_user;

-- Read-only view: write attempts return 405 (read_only_sql_transaction).
CREATE OR REPLACE VIEW api.readonly_view AS
  SELECT id, task FROM api.todos;
GRANT SELECT ON api.readonly_view TO web_anon, web_user;

-- Seed private_todos for RLS tests (Group 19). Insert rows with owner='web_user'
-- (the role the JWT will claim) so test 19.2 returns non-empty results.
INSERT INTO api.private_todos (id, owner, task) VALUES
    (1, 'web_user', 'user private task'),
    (2, 'other_role', 'not visible to web_user')
ON CONFLICT (id) DO NOTHING;
SELECT setval('api.private_todos_id_seq', (SELECT MAX(id) FROM api.private_todos));
