package backend

import "context"

// Listener is an optional backend capability for PostgREST's db-channel: a
// dedicated connection that waits for a database notification asking the server
// to reload. A backend that cannot listen does not implement it, and the server
// reloads on signals only. PostgreSQL implements it over LISTEN/NOTIFY.
type Listener interface {
	// Listen opens a dedicated connection, waits for notifications on the named
	// channel, and invokes the handler for each one until ctx is canceled. It
	// blocks, so the caller runs it in a goroutine; it reconnects on a dropped
	// connection with capped backoff and calls OnReconnect after re-establishing,
	// because notifications sent while it was down are lost and the cache may be
	// stale. It returns ctx.Err() when ctx is canceled.
	Listen(ctx context.Context, channel string, h ListenHandler) error
}

// ListenHandler carries the callbacks a Listener invokes. Decoding a payload into
// a reload action (the empty / "reload schema" / "reload config" contract) is the
// caller's job, so the wire contract lives in one engine-agnostic place rather
// than being duplicated in each backend.
type ListenHandler struct {
	// OnNotify is called for each notification with its raw payload. A nil func
	// is a no-op.
	OnNotify func(payload string)
	// OnReconnect is called after the connection is re-established following a
	// drop, signaling that notifications may have been missed. A nil func is a
	// no-op. It is not called for the first connection.
	OnReconnect func()
}
