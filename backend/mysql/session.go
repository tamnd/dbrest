package mysql

// MySQL has no GUC-in-SQL session store, so there is no per-request role switch
// or context push. The request context (JWT claims, method, path, headers) is
// bound as parameters when policy predicates are injected into the IR
// (SessionContext: Emulated). This file exists as a documentation anchor; the
// actual emulation lives in the authz package. See spec 15.
