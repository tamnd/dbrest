//go:build !unix

package main

import "log"

// watchSignals is a no-op on platforms without the SIGUSR1/SIGUSR2 reload
// signals (Windows). Reloads there are driven by the db-channel listener and
// the admin API; the signal path simply is not available.
func (a *app) watchSignals() {
	log.Printf("dbrest: signal-driven reload is unavailable on this platform; use the db-channel or the admin API")
}
