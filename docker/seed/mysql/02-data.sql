-- Seed data mirroring docker/seed/03-data.sql (PostgreSQL). IDs are explicit and
-- idempotent via INSERT IGNORE + UPDATE (MySQL has no ON CONFLICT DO UPDATE with
-- explicit IDs the same way, but INSERT INTO ... ON DUPLICATE KEY UPDATE works).

INSERT INTO todos (id, done, task, due, tags) VALUES
    (1, FALSE, 'finish tutorial', '2030-01-01', '["go","sql"]'),
    (2, TRUE,  'pat the cat',     NULL,         '["pets"]'),
    (3, FALSE, 'do laundry',      '2030-06-15', '["chores","home"]')
ON DUPLICATE KEY UPDATE
    done = VALUES(done),
    task = VALUES(task),
    due  = VALUES(due),
    tags = VALUES(tags);

INSERT INTO persons (id, name, age, email) VALUES
    (1, 'Alice', 30, 'alice@example.com'),
    (2, 'Bob',   25, 'bob@example.com'),
    (3, 'Carol', 35, NULL)
ON DUPLICATE KEY UPDATE
    name  = VALUES(name),
    age   = VALUES(age),
    email = VALUES(email);

INSERT INTO assignments (id, person_id, todo_id) VALUES
    (1, 1, 1),
    (2, 2, 1),
    (3, 1, 2)
ON DUPLICATE KEY UPDATE
    person_id = VALUES(person_id),
    todo_id   = VALUES(todo_id);

-- Reset AUTO_INCREMENT counters so the next insert gets id=4.
ALTER TABLE todos       AUTO_INCREMENT = 4;
ALTER TABLE persons     AUTO_INCREMENT = 4;
ALTER TABLE assignments AUTO_INCREMENT = 4;
