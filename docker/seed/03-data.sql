-- Seed data for the conformance tests. Both the PostgREST and dbrest stacks
-- load this data so the compat harness compares responses from the same state.
-- The IDs are explicit so inserts are idempotent and repeatable.

INSERT INTO api.todos (id, done, task, due) VALUES
    (1, false, 'finish tutorial', '2030-01-01'),
    (2, true,  'pat the cat',     NULL),
    (3, false, 'do laundry',      '2030-06-15')
ON CONFLICT (id) DO NOTHING;

INSERT INTO api.persons (id, name, age, email) VALUES
    (1, 'Alice', 30, 'alice@example.com'),
    (2, 'Bob',   25, 'bob@example.com')
ON CONFLICT (id) DO NOTHING;

INSERT INTO api.assignments (id, person_id, todo_id) VALUES
    (1, 1, 1),
    (2, 2, 3)
ON CONFLICT (id) DO NOTHING;

-- Reset sequences so future inserts do not collide with seeded IDs.
SELECT setval('api.todos_id_seq',       (SELECT MAX(id) FROM api.todos));
SELECT setval('api.persons_id_seq',     (SELECT MAX(id) FROM api.persons));
SELECT setval('api.assignments_id_seq', (SELECT MAX(id) FROM api.assignments));
