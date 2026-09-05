// Package config is autophaged's config.toml shape, loaded by perch. Secrets
// come from the environment, never from the file.
package config

import (
	"errors"
	"fmt"
	"os"
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

// Default is the starting point perch overlays the file on.
func Default() Config {
	return Config{
		Listen:  "127.0.0.1:8080",
		DB:      DBConfig{URL: "postgres:///autophage"},
		Budget:  BudgetConfig{Auto: BudgetTier{Turns: 40, WallClock: "45m", DiffLines: 600}, Approved: BudgetTier{Turns: 150, WallClock: "3h", DiffLines: 3000}},
		Sandbox: SandboxConfig{Image: "localhost/autophage-sandbox:latest", WorkspacesDir: "~/.local/share/autophage/workspaces", Concurrency: 2},
		Label:   LabelConfig{Approved: "approved"},
		GitHub:  GitHubConfig{BotLogin: "autophage[bot]"},
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

// Validate checks the file's values and turns the budget tiers into domain
// budgets. Model ids are checked only for presence.
func (c Config) Validate() (auto, approved resolution.Budget, err error) {
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
	auto, e := c.Budget.Auto.budget("auto")
	errs = append(errs, e)
	approved, e = c.Budget.Approved.budget("approved")
	errs = append(errs, e)
	return auto, approved, errors.Join(errs...)
}

func (t BudgetTier) budget(name string) (resolution.Budget, error) {
	wall, err := time.ParseDuration(t.WallClock)
	if err != nil {
		return resolution.Budget{}, fmt.Errorf("budget.%s.wall_clock: %w", name, err)
	}
	b, err := resolution.NewBudget(t.Turns, wall, t.DiffLines)
	if err != nil {
		return resolution.Budget{}, fmt.Errorf("budget.%s: %w", name, err)
	}
	return b, nil
}
