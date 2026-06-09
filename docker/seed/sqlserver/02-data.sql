-- Seed data: matches the PostgreSQL/MySQL compat seed (spec 2023).
-- Idempotent: skip if rows already present.

IF NOT EXISTS (SELECT 1 FROM dbo.todos WHERE id = 1)
BEGIN
    SET IDENTITY_INSERT dbo.todos ON;
    INSERT INTO dbo.todos (id, done, task, due, tags) VALUES
        (1, 0, 'finish tutorial', '2030-01-01', '["go","sql"]'),
        (2, 1, 'pat the cat',     NULL,         '["pets"]'),
        (3, 0, 'do laundry',      '2030-06-15', '["chores","home"]');
    SET IDENTITY_INSERT dbo.todos OFF;

    -- Reset identity seed so future inserts start at 4.
    DBCC CHECKIDENT ('dbo.todos', RESEED, 3);
END
GO

IF NOT EXISTS (SELECT 1 FROM dbo.persons WHERE id = 1)
BEGIN
    SET IDENTITY_INSERT dbo.persons ON;
    INSERT INTO dbo.persons (id, name, age, email) VALUES
        (1, 'Alice', 30, 'alice@example.com'),
        (2, 'Bob',   25, 'bob@example.com');
    SET IDENTITY_INSERT dbo.persons OFF;

    DBCC CHECKIDENT ('dbo.persons', RESEED, 2);
END
GO

IF NOT EXISTS (SELECT 1 FROM dbo.assignments WHERE id = 1)
BEGIN
    SET IDENTITY_INSERT dbo.assignments ON;
    INSERT INTO dbo.assignments (id, person_id, todo_id) VALUES
        (1, 1, 1),
        (2, 2, 3);
    SET IDENTITY_INSERT dbo.assignments OFF;

    DBCC CHECKIDENT ('dbo.assignments', RESEED, 2);
END
GO
