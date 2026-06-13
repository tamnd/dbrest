package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/backend"
)

// listenMaxBackoff caps the reconnect wait, matching PostgREST's listener.
const listenMaxBackoff = 32 * time.Second

// Listen implements backend.Listener over LISTEN/NOTIFY. It dedicates a
// connection (separate from the pool, since a connection blocked waiting for a
// notification cannot serve queries) to the named channel and reconnects with
// exponential backoff capped at 32 seconds. After a reconnect it calls
// OnReconnect, because notifications sent while the connection was down are lost
// and the schema cache may be stale, mirroring PostgREST's "assume we lost
// notifications, refresh the schema cache" behavior.
func (b *Backend) Listen(ctx context.Context, channel string, h backend.ListenHandler) error {
	backoff := time.Second
	firstConnect := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := pgx.ConnectConfig(ctx, b.connConfig)
		if err != nil {
			if werr := waitBackoff(ctx, backoff); werr != nil {
				return werr
			}
			backoff = nextBackoff(backoff)
			continue
		}
		// (Re)connected: reset the backoff and signal a reconnect so the caller
		// can recover any notifications missed while the connection was down.
		backoff = time.Second
		if !firstConnect && h.OnReconnect != nil {
			h.OnReconnect()
		}
		firstConnect = false

		err = b.waitForNotifications(ctx, conn, channel, h)
		conn.Close(context.Background())
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// err is a lost-connection error; the loop above reconnects with backoff.
		_ = err
		if werr := waitBackoff(ctx, backoff); werr != nil {
			return werr
		}
		backoff = nextBackoff(backoff)
	}
}

// waitForNotifications issues LISTEN on the channel, then blocks delivering each
// notification's payload to the handler until the connection drops or ctx is
// canceled. It returns the error that ended the loop.
func (b *Backend) waitForNotifications(ctx context.Context, conn *pgx.Conn, channel string, h backend.ListenHandler) error {
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if h.OnNotify != nil {
			h.OnNotify(n.Payload)
		}
	}
}

// waitBackoff sleeps for d unless ctx is canceled first, in which case it returns
// ctx.Err().
func waitBackoff(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// nextBackoff doubles d, capped at listenMaxBackoff.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > listenMaxBackoff {
		return listenMaxBackoff
	}
	return d
}
