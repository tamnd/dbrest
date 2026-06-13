package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/tamnd/dbrest/backend"
)

// TestIntegrationListen drives the live db-channel path: a NOTIFY on the channel
// the backend LISTENs on is delivered to the handler with its payload intact.
// NOTIFY only reaches sessions already listening, so the test notifies on a
// ticker until the listener has subscribed and the payload arrives.
func TestIntegrationListen(t *testing.T) {
	be := openBE(t)
	const channel = "dbrest_test_chan"

	got := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = be.Listen(ctx, channel, backend.ListenHandler{
			OnNotify: func(payload string) { got <- payload },
		})
	}()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-tick.C:
			if _, err := be.Pool().Exec(ctx, "SELECT pg_notify($1, $2)", channel, "reload schema"); err != nil {
				t.Fatalf("pg_notify: %v", err)
			}
		case p := <-got:
			if p != "reload schema" {
				t.Fatalf("payload = %q, want %q", p, "reload schema")
			}
			return
		case <-deadline:
			t.Fatal("no notification delivered within 5s")
		}
	}
}

// TestIntegrationListenStopsOnCancel confirms Listen returns promptly with the
// context error once its context is canceled, so the boot-time goroutine does
// not leak.
func TestIntegrationListenStopsOnCancel(t *testing.T) {
	be := openBE(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- be.Listen(ctx, "dbrest_test_cancel", backend.ListenHandler{}) }()

	// Give the listener a moment to connect and subscribe, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Listen returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Listen did not return within 3s of cancel")
	}
}
