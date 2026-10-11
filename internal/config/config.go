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
	Deploy  DeployConfig  `toml:"deploy"`
	Routing RoutingConfig `toml:"routing"`
}

type DBConfig struct {
	URL string `toml:"url"`
}

type GitHubConfig struct {
	AppID         int64  `toml:"app_id"`
	PrivateKey    string `toml:"private_key"` // path to the App's PEM
	BotLogin      string `toml:"bot_login"`   // e.g. autophage[bot]
	OperatorLogin string `toml:"operator_login"`
	// WebhookSecret is read from the environment:
	// AUTOPHAGE_GITHUB_WEBHOOK_SECRET. The model gateway needs no secret:
	// Aperture is on the tailnet and authenticates the host.
}

type ModelTier struct {
	Model string `toml:"model"` // Aperture model id, as /v1/models lists it
	// Route, when set, picks the model per use from the live registry
	// (ADR 0010) and Model becomes the fallback: "cost" or "performance".
	// Empty runs Model as configured.
	Route string `toml:"route"`
}

// RoutingConfig is the host side of agent-select routing.
type RoutingConfig struct {
	// LocalProviders are gateway provider ids that run on the operator's
	// own hardware. Their models are asserted free.
	LocalProviders []string `toml:"local_providers"`
	// LocalContextTokens maps a local model id to the context window it
	// serves, for models whose registry entry reports none (llama-swap's
	// context_size on trig). A local model absent here is never chosen for
	// a request with a token quantity.
	LocalContextTokens map[string]int64 `toml:"local_context_tokens"`
	// Snapshot is the path of an agent-select snapshot artifact adopted at
	// boot. Empty routes with no quality signals.
	Snapshot string `toml:"snapshot"`
}

type ModelConfig struct {
	Triage   ModelTier `toml:"triage"`
	Auto     ModelTier `toml:"auto"`
	Approved ModelTier `toml:"approved"`
	// Repair is the tier upkeep rounds run on. It can be a smaller model
	// than the attempts: a repair reads a failing check and a diff that
	// already exists rather than writing a fix from a description.
	Repair ModelTier `toml:"repair"`
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
		Budget:  BudgetConfig{Auto: BudgetTier{Turns: 80, WallClock: "90m", DiffLines: 2000}, Approved: BudgetTier{Turns: 300, WallClock: "6h", DiffLines: 10000}},
		Sandbox: SandboxConfig{Image: "localhost/autophage-sandbox:latest", WorkspacesDir: "~/.local/share/autophage/workspaces", Concurrency: 2},
		Label:   LabelConfig{Approved: "approved"},
		GitHub:  GitHubConfig{BotLogin: "autophage[bot]"},
		Model: ModelConfig{
			// Triage and repair route on cost: the local Qwens on trig lead
			// as free, and the configured id is the fallback.
			Triage:   ModelTier{Model: "zai/glm-5.3", Route: "cost"},
			Auto:     ModelTier{Model: "anthropic/claude-sonnet-5"},
			Approved: ModelTier{Model: "anthropic/claude-sonnet-5"},
			// The free route on the local Qwen: repairs are high-volume
			// background work, and a repair reads a failing check and an
			// existing diff rather than writing a fix from a description.
			Repair: ModelTier{Model: "qwen-27b", Route: "cost"},
		},
		Routing: RoutingConfig{LocalProviders: []string{"trig"}, LocalContextTokens: map[string]int64{"qwen-27b": 131072, "qwen": 32768}},
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

// DeployConfig is push-to-deploy. An empty Repo disables the whole
// mechanism; setting it requires AUTOPHAGE_DEPLOY_WEBHOOK_SECRET in the
// environment.
type DeployConfig struct {
	Repo       string `toml:"repo"`
	InstallDir string `toml:"install_dir"`
}

// Secrets are the values that never live in the file.
type Secrets struct {
	WebhookSecret []byte
	// DeployWebhookSecret verifies the repo's release webhook. It is a
	// host-execution secret, deliberately separate from the App's webhook
	// secret, and only required when [deploy] repo is set.
	DeployWebhookSecret []byte
}

// LoadSecrets reads the environment. The webhook secret is required to run
// the daemon; the model gateway contributes none, because Aperture is on the
// tailnet and authenticates the host rather than a key.
func LoadSecrets() (Secrets, error) {
	ws := os.Getenv("AUTOPHAGE_GITHUB_WEBHOOK_SECRET")
	if ws == "" {
		return Secrets{}, errors.New("AUTOPHAGE_GITHUB_WEBHOOK_SECRET is not set")
	}
	return Secrets{WebhookSecret: []byte(ws), DeployWebhookSecret: []byte(os.Getenv("AUTOPHAGE_DEPLOY_WEBHOOK_SECRET"))}, nil
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
	if r := c.Deploy.Repo; r != "" {
		parts := strings.Split(r, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(r, " \t") {
			errs = append(errs, fmt.Errorf("deploy.repo must be owner/name, got %q", r))
		}
	}
	for name, tier := range map[string]ModelTier{"triage": c.Model.Triage, "auto": c.Model.Auto, "approved": c.Model.Approved, "repair": c.Model.Repair} {
		if tier.Model == "" {
			errs = append(errs, fmt.Errorf("model.%s.model is required", name))
		}
		if tier.Route != "" && tier.Route != "cost" && tier.Route != "performance" {
			errs = append(errs, fmt.Errorf("model.%s.route must be \"cost\", \"performance\" or empty, got %q", name, tier.Route))
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
