package config

import (
	"strings"
	"testing"
)

func complete() Config {
	c := Default()
	c.GitHub.AppID = 1
	c.GitHub.PrivateKey = "/x/app.pem"
	c.GitHub.OperatorLogin = "guy"
	c.Model.Triage.Model = "zai/glm-5.3"
	c.Model.Auto.Model = "anthropic/claude-sonnet-5"
	c.Model.Approved.Model = "anthropic/claude-sonnet-5"
	c.Model.Repair.Model = "qwen-27b"
	return c
}

func TestValidateComplete(t *testing.T) {
	b, err := complete().Validate()
	if err != nil {
		t.Fatal(err)
	}
	if b.Auto.MaxTurns() != 80 || b.Approved.MaxTurns() != 300 {
		t.Errorf("budgets = %d %d", b.Auto.MaxTurns(), b.Approved.MaxTurns())
	}
}

func TestValidateReportsEveryMissingField(t *testing.T) {
	c := Default()
	c.GitHub = GitHubConfig{}
	c.Model = ModelConfig{}
	c.Budget.Auto.WallClock = "soon"
	_, err := c.Validate()
	if err == nil {
		t.Fatal("empty config validated")
	}
	for _, want := range []string{"github.app_id", "github.private_key", "github.operator_login", "model.triage.model", "model.auto.model", "model.approved.model", "model.repair.model", "budget.auto.wall_clock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestLoadSecretsRequiresTheWebhookSecret(t *testing.T) {
	t.Setenv("AUTOPHAGE_GITHUB_WEBHOOK_SECRET", "")
	if _, err := LoadSecrets(); err == nil || !strings.Contains(err.Error(), "AUTOPHAGE_GITHUB_WEBHOOK_SECRET") {
		t.Errorf("err = %v", err)
	}
	t.Setenv("AUTOPHAGE_GITHUB_WEBHOOK_SECRET", "s")
	s, err := LoadSecrets()
	if err != nil || string(s.WebhookSecret) != "s" {
		t.Errorf("secrets = %+v %v", s, err)
	}
}

// deploy.repo is optional but must be owner/name when present.
func TestValidateDeployRepo(t *testing.T) {
	c := complete()
	if _, err := c.Validate(); err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, bad := range []string{"guygrigsby", "a/b/c", " /x", "x/ ", ""} {
		if bad == "" {
			continue
		}
		c.Deploy.Repo = bad
		if _, err := c.Validate(); err == nil || !strings.Contains(err.Error(), "deploy.repo") {
			t.Errorf("repo %q: %v", bad, err)
		}
	}
	c.Deploy.Repo = "guygrigsby/autophage"
	if _, err := c.Validate(); err != nil {
		t.Errorf("valid repo: %v", err)
	}
}

func TestValidateRouteModes(t *testing.T) {
	c := complete()
	c.Model.Triage.Route = "cost"
	c.Model.Repair.Route = "quality"
	if _, err := c.Validate(); err != nil {
		t.Fatalf("valid routes rejected: %v", err)
	}
	c.Model.Auto.Route = "cheapest"
	_, err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "model.auto.route") {
		t.Fatalf("err = %v, want model.auto.route named", err)
	}
}

func TestDefaultModels(t *testing.T) {
	c := Default()
	for name, tier := range map[string]ModelTier{"triage": c.Model.Triage, "auto": c.Model.Auto, "repair": c.Model.Repair} {
		if tier.Model != "qwen-27b" || tier.Route != "" {
			t.Errorf("%s = %+v, want qwen-27b pinned", name, tier)
		}
	}
	if c.Model.Approved.Model != "anthropic/claude-sonnet-5.5" || c.Model.Approved.Route != "" {
		t.Errorf("approved = %+v", c.Model.Approved)
	}
	if len(c.Routing.LocalProviders) != 1 || c.Routing.LocalProviders[0] != "trig" {
		t.Errorf("local providers = %v", c.Routing.LocalProviders)
	}
}

func TestValidateLocalOnly(t *testing.T) {
	c := complete()
	c.Model.Repair.Route, c.Model.Repair.LocalOnly = "cost", true
	if _, err := c.Validate(); err != nil {
		t.Fatalf("local-only repair on qwen-27b rejected: %v", err)
	}
	c.Model.Triage.Route, c.Model.Triage.LocalOnly = "cost", true // fallback zai/glm-5.3 is remote
	if _, err := c.Validate(); err == nil || !strings.Contains(err.Error(), "model.triage.local_only") {
		t.Fatalf("err = %v, want a remote fallback rejected", err)
	}
	c = complete()
	c.Model.Repair.Route, c.Model.Repair.LocalOnly = "", true
	if _, err := c.Validate(); err == nil || !strings.Contains(err.Error(), "model.repair.route") {
		t.Fatalf("err = %v, want local_only without route rejected", err)
	}
}
