// Command autophaged watches enrolled repositories, gates and triages their
// issues and runs budgeted attempts through the Runner.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/guygrigsby/jess/ledger"
	"github.com/guygrigsby/perch/config"
	"github.com/guygrigsby/perch/daemon"

	rootapp "github.com/guygrigsby/autophage"
	"github.com/guygrigsby/autophage/internal/agent"
	"github.com/guygrigsby/autophage/internal/api"
	"github.com/guygrigsby/autophage/internal/app"
	appconfig "github.com/guygrigsby/autophage/internal/config"
	"github.com/guygrigsby/autophage/internal/github"
	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/sandbox"
	"github.com/guygrigsby/autophage/internal/store"
)

var version = "dev"

func main() {
	addrFlag := flag.String("addr", "", "listen address (overrides config/env)")
	flag.Parse()

	cfg := appconfig.Default()
	if err := config.Load("autophage", &cfg); err != nil {
		log.Fatalf("load config: %v", err)
	}
	autoBudget, approvedBudget, err := cfg.Validate()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	secrets, err := appconfig.LoadSecrets()
	if err != nil {
		log.Fatalf("secrets: %v", err)
	}
	dir, err := config.Dir("autophage")
	if err != nil {
		log.Fatalf("config dir: %v", err)
	}
	ctx, cancel := daemon.SignalContext()
	defer cancel()

	st, err := store.Open(ctx, cfg.DB.URL)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	pem, err := os.ReadFile(expandHome(cfg.GitHub.PrivateKey))
	if err != nil {
		log.Fatalf("read app key: %v", err)
	}
	gh, err := github.NewClient(github.ClientConfig{AppID: cfg.GitHub.AppID, PrivateKeyPEM: pem, UserAgent: "autophage/" + version,
		Installations: func(ctx context.Context, repo string) (int64, error) {
			r, err := st.GetRepository(ctx, repo)
			return r.InstallationID, err
		}})
	if err != nil {
		log.Fatalf("github: %v", err)
	}

	ledgerDB := st.SQLDB()
	pg, err := ledger.NewPostgres(ledgerDB)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	clock := resolution.SystemClock{}
	runner := &agent.Runner{
		Store:  st,
		GitHub: gh,
		Sandbox: &sandbox.Manager{Image: cfg.Sandbox.Image, WorkspacesDir: expandHome(cfg.Sandbox.WorkspacesDir), Memory: "4g", CPUs: "4", Pids: 512,
			BotName: cfg.GitHub.BotLogin, BotEmail: strings.TrimSuffix(cfg.GitHub.BotLogin, "[bot]") + "[bot]@users.noreply.github.com", Logf: log.Printf},
		Models:        agent.Models{Key: secrets.OpenRouterKey, Title: "autophage"},
		AttemptModel:  cfg.Model.Auto.Model,
		ApprovedModel: cfg.Model.Approved.Model,
		TriageModel:   cfg.Model.Triage.Model,
		Ledger:        pg,
		Clock:         clock,
		Logf:          log.Printf,
		Metrics:       api.RunnerMetrics{},
	}
	dispatcher := &app.Dispatcher{
		Store:      st,
		Translator: &github.Translator{Store: st, Clock: clock, ApprovedLabel: cfg.Label.Approved, BotLogin: cfg.GitHub.BotLogin},
		Triage:     &app.Triage{Store: st, Triager: runner.Triager(), GitHub: gh, Clock: clock},
		Scheduler:  &app.Scheduler{Store: st, Runner: runner, Concurrency: cfg.Sandbox.Concurrency, Clock: clock, Budgets: app.BudgetPolicy{Auto: autoBudget, Approved: approvedBudget}, GitHub: gh},
		Commenter:  &app.Commenter{Store: st, GitHub: gh, Label: cfg.Label.Approved},
		Enrollment: &app.Enrollment{Store: st, GitHub: gh, Label: cfg.Label.Approved},
		Recovery:   &app.Recovery{Store: st, Clock: clock},
		Canceller:  runner,
	}

	handler := api.New(dir, rootapp.Static(), api.Deps{
		Base: ctx, Store: st, GitHub: gh, Clock: clock, OperatorLogin: cfg.GitHub.OperatorLogin,
		Webhook: github.WebhookHandler(st, secrets.WebhookSecret, clock),
		Sweep:   dispatcher.Sweep, Ledger: pg, Stop: runner.Stop,
		Version: version, StartedAt: clock.Now(), Concurrency: cfg.Sandbox.Concurrency,
	})
	addr := daemon.ResolveAddr(*addrFlag, "AUTOPHAGE_LISTEN", cfg.Listen)
	// Timeouts, because /webhook/github faces the public internet through
	// Funnel: without them a slow-loris client holds a connection and its
	// goroutine open for as long as it likes.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := dispatcher.Run(ctx); err != nil {
			log.Printf("dispatcher: %v", err)
			cancel()
		}
	}()
	log.Printf("autophaged %s listening on %s", version, addr)
	if err := daemon.Serve(ctx, srv, 10*time.Second); err != nil {
		log.Printf("serve: %v", err)
	}
	cancel()
	wg.Wait()
	_ = ledgerDB.Close()
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return home + p[1:]
	}
	return p
}
