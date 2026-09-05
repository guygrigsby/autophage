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
	c.Model.Triage.Model = "moonshotai/kimi-k2.7-code"
	c.Model.Auto.Model = "moonshotai/kimi-k3"
	c.Model.Approved.Model = "moonshotai/kimi-k3"
	return c
}

func TestValidateComplete(t *testing.T) {
	auto, approved, err := complete().Validate()
	if err != nil {
		t.Fatal(err)
	}
	if auto.MaxTurns() != 40 || approved.MaxTurns() != 150 {
		t.Errorf("budgets = %d %d", auto.MaxTurns(), approved.MaxTurns())
	}
}

func TestValidateReportsEveryMissingField(t *testing.T) {
	c := Default()
	c.Budget.Auto.WallClock = "soon"
	_, _, err := c.Validate()
	if err == nil {
		t.Fatal("empty config validated")
	}
	for _, want := range []string{"github.app_id", "github.private_key", "github.operator_login", "model.triage.model", "model.auto.model", "model.approved.model", "budget.auto.wall_clock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestLoadSecretsRequiresBoth(t *testing.T) {
	t.Setenv("AUTOPHAGE_GITHUB_WEBHOOK_SECRET", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := LoadSecrets(); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("err = %v", err)
	}
	t.Setenv("AUTOPHAGE_GITHUB_WEBHOOK_SECRET", "s")
	t.Setenv("OPENROUTER_API_KEY", "k")
	s, err := LoadSecrets()
	if err != nil || string(s.WebhookSecret) != "s" || s.OpenRouterKey != "k" {
		t.Errorf("secrets = %+v %v", s, err)
	}
}
