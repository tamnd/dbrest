// MongoDB compat seed — matches the PostgreSQL/MySQL/SQL Server compat corpus.
// Run with: mongosh mongodb://dbrest:Dbrest1Passw0rd@localhost:27017/dbrest seed.js
//
// Collections: todos, persons, assignments
// Types: integer id, boolean done, string task, string|null due, array tags

db = db.getSiblingDB("dbrest");

// Idempotent: drop and recreate.
db.todos.drop();
db.persons.drop();
db.assignments.drop();

db.todos.insertMany([
    { id: 1, done: false, task: "finish tutorial", due: "2030-01-01", tags: ["go", "sql"] },
    { id: 2, done: true,  task: "pat the cat",     due: null,         tags: ["pets"] },
    { id: 3, done: false, task: "do laundry",       due: "2030-06-15", tags: ["chores", "home"] }
]);

db.persons.insertMany([
    { id: 1, name: "Alice", age: 30, email: "alice@example.com" },
    { id: 2, name: "Bob",   age: 25, email: "bob@example.com" }
]);

db.assignments.insertMany([
    { id: 1, person_id: 1, todo_id: 1 },
    { id: 2, person_id: 2, todo_id: 3 }
]);

print("MongoDB seed complete: todos=3, persons=2, assignments=2");
