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
	budgets, err := cfg.Validate()
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
		}, Metrics: api.RequestMetrics{}})
	if err != nil {
		log.Fatalf("github: %v", err)
	}

	ledgerDB := st.SQLDB()
	pg, err := ledger.NewPostgres(ledgerDB)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	clock := resolution.SystemClock{}
	// One sandbox manager and one model factory, shared by the issue runner
	// and the repair runner: they contend for the same workspace locks and
	// the same pool, which only works if it is literally the same manager.
	sb := &sandbox.Manager{Image: cfg.Sandbox.Image, WorkspacesDir: expandHome(cfg.Sandbox.WorkspacesDir), Memory: "4g", CPUs: "4", Pids: 512,
		BotName: cfg.GitHub.BotLogin, BotEmail: strings.TrimSuffix(cfg.GitHub.BotLogin, "[bot]") + "[bot]@users.noreply.github.com", Logf: log.Printf}
	models := agent.Models{Key: secrets.OpenRouterKey, Title: "autophage", Metrics: api.ModelMetrics{}}
	runner := &agent.Runner{
		Store:         st,
		GitHub:        gh,
		Sandbox:       sb,
		Models:        models,
		AttemptModel:  cfg.Model.Auto.Model,
		ApprovedModel: cfg.Model.Approved.Model,
		TriageModel:   cfg.Model.Triage.Model,
		Ledger:        pg,
		Clock:         clock,
		Logf:          log.Printf,
		Metrics:       api.RunnerMetrics{},
	}
	translator := &github.Translator{Store: st, Clock: clock, ApprovedLabel: cfg.Label.Approved, BotLogin: cfg.GitHub.BotLogin, Metrics: api.TranslateMetrics{}}
	scheduler := &app.Scheduler{Store: st, Runner: runner, Concurrency: cfg.Sandbox.Concurrency, Clock: clock, Budgets: app.BudgetPolicy{Auto: budgets.Auto, Approved: budgets.Approved, Repair: budgets.Repair}, GitHub: gh}
	dispatcher := &app.Dispatcher{
		Store:      st,
		Translator: translator,
		Triage:     &app.Triage{Store: st, Triager: runner.Triager(), GitHub: gh, Clock: clock, Model: cfg.Model.Triage.Model, Metrics: api.TriageMetrics{}},
		Scheduler:  scheduler,
		Commenter:  &app.Commenter{Store: st, GitHub: gh, Label: cfg.Label.Approved, Metrics: api.CommentMetrics{}},
		Enrollment: &app.Enrollment{Store: st, GitHub: gh, Label: cfg.Label.Approved, Clock: clock},
		Recovery:   &app.Recovery{Store: st, Clock: clock},
		Canceller:  runner,
		Metrics:    api.SweepMetrics{},
	}
	// stopRepair stays nil while upkeep is off, and the stop endpoint then
	// answers that the round is not running here.
	var stopRepair func(string) bool
	if budgets.UpkeepEnabled {
		// Wiring Rollup is what turns the three Upkeep events on: without it
		// the translator ignores them and nothing else here is reachable.
		translator.DependabotLogin = cfg.Upkeep.DependabotLogin
		translator.Rollup = gh
		translator.RoundCap = budgets.RoundCap
		dispatcher.StaleCheckSweeper = &app.StaleCheckSweeper{Store: st, Clock: clock, Window: budgets.CheckWindow}
		dispatcher.BumpRecovery = &app.BumpRecovery{Store: st, Clock: clock, RoundCap: budgets.RoundCap}
		repairer := &agent.Repairer{
			Store: st, GitHub: gh, Sandbox: sb, Models: models,
			Model: cfg.Model.Auto.Model, RoundCap: budgets.RoundCap,
			Ledger: pg, Clock: clock, Logf: log.Printf, Metrics: api.RunnerMetrics{},
		}
		dispatcher.RepairScheduler = &app.RepairScheduler{
			Store: st, Runner: repairer, GitHub: gh, Concurrency: cfg.Sandbox.Concurrency,
			Clock: clock, Budgets: app.BudgetPolicy{Repair: budgets.Repair}, RoundCap: budgets.RoundCap,
			DependabotLogin: cfg.Upkeep.DependabotLogin,
		}
		dispatcher.RepairCanceller = repairer
		stopRepair = repairer.Stop
		log.Printf("upkeep: on for dependabot pull requests (round cap %d, check window %s)", budgets.RoundCap, budgets.CheckWindow)
	}

	handler := api.New(dir, rootapp.Static(), api.Deps{
		Base: ctx, Store: st, GitHub: gh, Clock: clock, OperatorLogin: cfg.GitHub.OperatorLogin,
		Webhook: github.WebhookHandler(st, secrets.WebhookSecret, clock),
		Sweep:   dispatcher.Sweep, Ledger: pg, Stop: runner.Stop, StopRepair: stopRepair,
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
	// The dispatcher waits on the runners itself, but only while its own Run
	// is still in the loop: a dispatcher that returned early (a Recovery
	// failure, a context already done when Run was entered) leaves attempts
	// in flight. They own the database and the ledger, so the wait comes
	// before the pool and the ledger are closed under them, bounded the same
	// way the dispatcher bounds its own.
	if !scheduler.WaitTimeout(app.ShutdownWait) {
		log.Printf("shutdown: gave up after %s on running attempts: %s", app.ShutdownWait, strings.Join(scheduler.Running(), ", "))
	}
	_ = ledgerDB.Close()
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return home + p[1:]
	}
	return p
}
