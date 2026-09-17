// Package config is autophaged's config.toml shape, loaded by perch. Secrets
// come from the environment, never from the file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/guygrigsby/autophage/internal/resolution"
)

type Config struct {
	Listen  string        `toml:"listen"`
	DB      DBConfig      `toml:"db"`
	GitHub  GitHubConfig  `toml:"github"`
	Model   ModelConfig   `toml:"model"`
	Budget  BudgetConfig  `toml:"budget"`
	Sandbox SandboxConfig `toml:"sandbox"`
	Label   LabelConfig   `toml:"label"`
	Upkeep  UpkeepConfig  `toml:"upkeep"`
}

type DBConfig struct {
	URL string `toml:"url"`
}

type GitHubConfig struct {
	AppID         int64  `toml:"app_id"`
	PrivateKey    string `toml:"private_key"` // path to the App's PEM
	BotLogin      string `toml:"bot_login"`   // e.g. autophage[bot]
	OperatorLogin string `toml:"operator_login"`
	// WebhookSecret and the OpenRouter key are read from the environment:
	// AUTOPHAGE_GITHUB_WEBHOOK_SECRET and OPENROUTER_API_KEY.
}

type ModelTier struct {
	Model string `toml:"model"` // OpenRouter model id
}

type ModelConfig struct {
	Triage   ModelTier `toml:"triage"`
	Auto     ModelTier `toml:"auto"`
	Approved ModelTier `toml:"approved"`
}

type BudgetTier struct {
	Turns     int    `toml:"turns"`
	WallClock string `toml:"wall_clock"` // Go duration
	DiffLines int    `toml:"diff_lines"`
}

type BudgetConfig struct {
	Auto     BudgetTier `toml:"auto"`
	Approved BudgetTier `toml:"approved"`
}

type SandboxConfig struct {
	Image         string `toml:"image"`
	WorkspacesDir string `toml:"workspaces_dir"`
	Concurrency   int    `toml:"concurrency"`
}

type LabelConfig struct {
	Approved string `toml:"approved"`
}

// UpkeepConfig is the dependabot half. Off by default: turning it on
// subscribes the App to three more events and lets autophage push to
// branches it did not create, which is an operator's decision rather than a
// default.
type UpkeepConfig struct {
	Enabled         bool   `toml:"enabled"`
	DependabotLogin string `toml:"dependabot_login"`
	// RoundCap bounds the push, check, push cycle: how many repair rounds one
	// bump may spend before it is abandoned, before operator retry grants.
	RoundCap int `toml:"round_cap"`
	// CheckWindow is how long a bump waits for a conclusive check rollup
	// before it is abandoned. It has to outlast a slow matrix build and a
	// queued Actions runner without stranding a repository that runs no
	// checks on pull requests at all.
	CheckWindow string     `toml:"check_window"` // Go duration
	Budget      BudgetTier `toml:"budget"`
}

// Default is the starting point perch overlays the file on.
func Default() Config {
	return Config{
		Listen:  "127.0.0.1:8080",
		DB:      DBConfig{URL: "postgres:///autophage"},
		Budget:  BudgetConfig{Auto: BudgetTier{Turns: 40, WallClock: "45m", DiffLines: 600}, Approved: BudgetTier{Turns: 150, WallClock: "3h", DiffLines: 3000}},
		Sandbox: SandboxConfig{Image: "localhost/autophage-sandbox:latest", WorkspacesDir: "~/.local/share/autophage/workspaces", Concurrency: 2},
		Label:   LabelConfig{Approved: "approved"},
		GitHub:  GitHubConfig{BotLogin: "autophage[bot]"},
		Upkeep: UpkeepConfig{
			DependabotLogin: "dependabot[bot]",
			RoundCap:        3,
			CheckWindow:     "6h",
			// Smaller than auto: a repair reads a failing check and a diff
			// that already exists rather than writing a fix from a
			// description.
			Budget: BudgetTier{Turns: 25, WallClock: "30m", DiffLines: 300},
		},
	}
}

// Secrets are the values that never live in the file.
type Secrets struct {
	WebhookSecret []byte
	OpenRouterKey string
}

// LoadSecrets reads the environment. Both are required to run the daemon.
func LoadSecrets() (Secrets, error) {
	ws := os.Getenv("AUTOPHAGE_GITHUB_WEBHOOK_SECRET")
	or := os.Getenv("OPENROUTER_API_KEY")
	var errs []error
	if ws == "" {
		errs = append(errs, errors.New("AUTOPHAGE_GITHUB_WEBHOOK_SECRET is not set"))
	}
	if or == "" {
		errs = append(errs, errors.New("OPENROUTER_API_KEY is not set"))
	}
	return Secrets{WebhookSecret: []byte(ws), OpenRouterKey: or}, errors.Join(errs...)
}

// Budgets is everything Validate turns the file into: the domain budgets and
// the Upkeep numbers, so a caller gets one value rather than a growing list
// of returns.
type Budgets struct {
	Auto     resolution.Budget
	Approved resolution.Budget
	// Repair is the zero Budget when upkeep is off.
	Repair        resolution.Budget
	UpkeepEnabled bool
	RoundCap      int
	CheckWindow   time.Duration
}

// Validate checks the file's values and turns the budget tiers into domain
// budgets. Model ids are checked only for presence. The upkeep section is
// checked only when it is enabled, so a half-written one cannot stop a
// daemon that is not using it.
func (c Config) Validate() (Budgets, error) {
	var b Budgets
	var errs []error
	if c.GitHub.AppID <= 0 {
		errs = append(errs, errors.New("github.app_id is required"))
	}
	if c.GitHub.PrivateKey == "" {
		errs = append(errs, errors.New("github.private_key is required"))
	}
	if c.GitHub.OperatorLogin == "" {
		errs = append(errs, errors.New("github.operator_login is required"))
	}
	for name, tier := range map[string]ModelTier{"triage": c.Model.Triage, "auto": c.Model.Auto, "approved": c.Model.Approved} {
		if tier.Model == "" {
			errs = append(errs, fmt.Errorf("model.%s.model is required", name))
		}
	}
	if c.Sandbox.Concurrency <= 0 {
		errs = append(errs, errors.New("sandbox.concurrency must be positive"))
	}
	if c.Label.Approved == "" {
		errs = append(errs, errors.New("label.approved is required"))
	}
	var e error
	b.Auto, e = c.Budget.Auto.budget("auto")
	errs = append(errs, e)
	b.Approved, e = c.Budget.Approved.budget("approved")
	errs = append(errs, e)
	if c.Upkeep.Enabled {
		b.UpkeepEnabled = true
		// Trust here is a login match, so a typo does not fail closed: it
		// silently widens trust to whatever account the typo names. The
		// "[bot]" suffix is the cheap shape check that catches it.
		if c.Upkeep.DependabotLogin == "" {
			errs = append(errs, errors.New("upkeep.dependabot_login is required"))
		} else if !strings.HasSuffix(c.Upkeep.DependabotLogin, "[bot]") {
			errs = append(errs, fmt.Errorf("upkeep.dependabot_login %q is not a bot account (it must end in \"[bot]\"): trust here is a login match, so a typo would widen it to a human", c.Upkeep.DependabotLogin))
		}
		if c.Upkeep.RoundCap < 1 {
			errs = append(errs, errors.New("upkeep.round_cap must be at least 1"))
		}
		b.RoundCap = c.Upkeep.RoundCap
		b.CheckWindow, e = parseDuration("upkeep.check_window", c.Upkeep.CheckWindow)
		errs = append(errs, e)
		b.Repair, e = c.Upkeep.Budget.budget("upkeep.budget")
		errs = append(errs, e)
	}
	return b, errors.Join(errs...)
}

// parseDuration reports the field name with the failure, so an operator
// reading the log knows which line of the file to fix.
func parseDuration(field, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	return d, nil
}

// budget turns one tier into a domain budget. name is the file path of the
// tier, reported with any failure so an operator knows which line to fix.
// Upkeep passes its own already-qualified name ("upkeep.budget"); the
// Resolution tiers pass a bare one and get the "budget." prefix.
func (t BudgetTier) budget(name string) (resolution.Budget, error) {
	label := name
	if !strings.Contains(name, ".") {
		label = "budget." + name
	}
	wall, err := time.ParseDuration(t.WallClock)
	if err != nil {
		return resolution.Budget{}, fmt.Errorf("%s.wall_clock: %w", label, err)
	}
	b, err := resolution.NewBudget(t.Turns, wall, t.DiffLines)
	if err != nil {
		return resolution.Budget{}, fmt.Errorf("%s: %w", label, err)
	}
	return b, nil
}
