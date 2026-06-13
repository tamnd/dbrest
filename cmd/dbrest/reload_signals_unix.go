//go:build unix

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// watchSignals installs the two reload signals. Reload failures log and keep
// the previous state; they never terminate the process. SIGUSR1 and SIGUSR2 are
// Unix-only, so this handler is built only on Unix; see reload_signals_other.go
// for the no-op on the platforms that lack them.
func (a *app) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for s := range ch {
			switch s {
			case syscall.SIGUSR1:
				log.Printf("dbrest: received SIGUSR1, reloading the schema cache")
				if err := a.reloadSchema(); err != nil {
					log.Printf("dbrest: schema cache reload failed, keeping the old cache: %v", err)
				}
			case syscall.SIGUSR2:
				log.Printf("dbrest: received SIGUSR2, reloading the configuration")
				if err := a.reloadConfig(os.Environ()); err != nil {
					log.Printf("dbrest: config reload failed, keeping the old config: %v", err)
				}
			}
		}
	}()
}
