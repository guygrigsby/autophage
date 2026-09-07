package main

import (
	"github.com/guygrigsby/perch/config"

	appconfig "github.com/guygrigsby/autophage/internal/config"
)

// fallbackAddr is where the CLI looks when no config file names a listen
// address.
const fallbackAddr = "http://127.0.0.1:8080"

// daemonAddr turns the daemon's configured listen address into the CLI's
// default base URL, so an operator who moved the daemon off the default port
// does not have to repeat the port on every command. --addr still overrides.
func daemonAddr(listen string) string {
	if listen == "" {
		return fallbackAddr
	}
	return "http://" + listen
}

// configuredAddr reads the same config file the daemon reads. A missing or
// unreadable file means the fallback: the CLI must still run before the
// daemon is configured, if only to print help.
func configuredAddr() string {
	cfg := appconfig.Default()
	if err := config.Load(appID, &cfg); err != nil {
		return fallbackAddr
	}
	return daemonAddr(cfg.Listen)
}
