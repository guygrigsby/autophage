// Command autophaged is the rookery daemon: it serves the API and the embedded SPA.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/guygrigsby/perch/config"
	"github.com/guygrigsby/perch/daemon"
	rootapp "github.com/guygrigsby/autophage"
	"github.com/guygrigsby/autophage/internal/api"
)

// appConfig is the app's config.toml shape. Extend per app.
type appConfig struct {
	Listen string `toml:"listen"`
}

func main() {
	addrFlag := flag.String("addr", "", "listen address (overrides config/env)")
	flag.Parse()

	cfg := appConfig{Listen: ":8080"}
	if err := config.Load("autophage", &cfg); err != nil {
		log.Fatalf("load config: %v", err)
	}

	dir, err := config.Dir("autophage")
	if err != nil {
		log.Fatalf("config dir: %v", err)
	}

	addr := daemon.ResolveAddr(*addrFlag, "AUTOPHAGE_LISTEN", cfg.Listen)
	srv := &http.Server{Addr: addr, Handler: api.New(dir, rootapp.Static())}

	ctx, cancel := daemon.SignalContext()
	defer cancel()

	log.Printf("autophaged listening on %s", addr)
	if err := daemon.Serve(ctx, srv, 10*time.Second); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
