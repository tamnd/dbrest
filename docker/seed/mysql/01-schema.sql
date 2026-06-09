-- MySQL compat fixture schema. Mirrors the PostgreSQL seed (docker/seed/) with
-- engine-appropriate types:
--   text[]   → JSON        (stored as ["go","sql"] arrays)
--   boolean  → BOOL        (TINYINT(1) alias; tinyInt1IsBool=true in DSN → Go bool)
--   serial   → INT AUTO_INCREMENT
--
-- There is no separate "api" schema in MySQL; tables live in the "dbrest"
-- database directly. The web_anon role is emulated in-app by dbrest; no MySQL
-- user switching is required for the compat tests.

CREATE TABLE IF NOT EXISTS todos (
    id   INT          AUTO_INCREMENT PRIMARY KEY,
    done BOOL         NOT NULL DEFAULT FALSE,
    task TEXT         NOT NULL,
    due  DATE         DEFAULT NULL,
    tags JSON         NOT NULL DEFAULT (JSON_ARRAY())
);

CREATE TABLE IF NOT EXISTS persons (
    id    INT  AUTO_INCREMENT PRIMARY KEY,
    name  TEXT NOT NULL,
    age   INT  DEFAULT NULL,
    email VARCHAR(255) UNIQUE DEFAULT NULL
);

CREATE TABLE IF NOT EXISTS assignments (
    id        INT AUTO_INCREMENT PRIMARY KEY,
    person_id INT DEFAULT NULL,
    todo_id   INT DEFAULT NULL,
    UNIQUE KEY uniq_person_todo (person_id, todo_id),
    CONSTRAINT fk_assign_person FOREIGN KEY (person_id) REFERENCES persons(id) ON DELETE CASCADE,
    CONSTRAINT fk_assign_todo   FOREIGN KEY (todo_id)   REFERENCES todos(id)   ON DELETE CASCADE
);
