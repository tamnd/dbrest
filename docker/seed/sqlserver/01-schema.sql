-- SQL Server seed: create database + user + tables for dbrest compat tests.
-- Run as SA against the master database, then switch to dbrest for tables.

-- Create database if it does not exist.
IF NOT EXISTS (SELECT name FROM sys.databases WHERE name = 'dbrest')
    CREATE DATABASE dbrest;
GO

USE dbrest;
GO

-- Create login + user with limited permissions.
IF NOT EXISTS (SELECT name FROM sys.server_principals WHERE name = 'dbrest')
    CREATE LOGIN dbrest WITH PASSWORD = 'Dbrest!Passw0rd';
GO

IF NOT EXISTS (SELECT name FROM sys.database_principals WHERE name = 'dbrest')
BEGIN
    CREATE USER dbrest FOR LOGIN dbrest;
    ALTER ROLE db_owner ADD MEMBER dbrest;
END
GO

-- todos: mirrors the PostgreSQL/MySQL seed as closely as SQL Server allows.
-- BIT is the SQL Server boolean; INT IDENTITY is the auto-increment PK.
-- tags is NVARCHAR(MAX) (no array type; array-op tests target PGRST127).
IF NOT EXISTS (SELECT * FROM sys.tables WHERE name = 'todos' AND schema_id = SCHEMA_ID('dbo'))
CREATE TABLE dbo.todos (
    id   INT IDENTITY(1,1) PRIMARY KEY,
    done BIT           NOT NULL DEFAULT 0,
    task NVARCHAR(500) NOT NULL,
    due  DATE          NULL,
    tags NVARCHAR(MAX) NULL
);
GO

IF NOT EXISTS (SELECT * FROM sys.tables WHERE name = 'persons' AND schema_id = SCHEMA_ID('dbo'))
CREATE TABLE dbo.persons (
    id    INT IDENTITY(1,1) PRIMARY KEY,
    name  NVARCHAR(255) NOT NULL,
    age   INT           NULL,
    email NVARCHAR(255) NULL
);
GO

IF NOT EXISTS (SELECT * FROM sys.indexes WHERE name = 'uq_persons_email')
    ALTER TABLE dbo.persons ADD CONSTRAINT uq_persons_email UNIQUE (email);
GO

IF NOT EXISTS (SELECT * FROM sys.tables WHERE name = 'assignments' AND schema_id = SCHEMA_ID('dbo'))
CREATE TABLE dbo.assignments (
    id        INT IDENTITY(1,1) PRIMARY KEY,
    person_id INT NOT NULL REFERENCES dbo.persons(id),
    todo_id   INT NOT NULL REFERENCES dbo.todos(id)
);
GO
