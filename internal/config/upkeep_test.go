package config

import (
	"strings"
	"testing"
	"time"
)

func TestUpkeepIsOffByDefault(t *testing.T) {
	b, err := complete().Validate()
	if err != nil {
		t.Fatal(err)
	}
	if b.UpkeepEnabled {
		t.Error("upkeep is on by default; it subscribes to three more events and writes to branches autophage did not create")
	}
}

func TestUpkeepDefaultsAreUsable(t *testing.T) {
	c := complete()
	c.Upkeep.Enabled = true
	b, err := c.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if !b.UpkeepEnabled {
		t.Fatal("upkeep did not turn on")
	}
	if b.Repair.MaxTurns() == 0 {
		t.Error("no repair budget")
	}
	if b.RoundCap < 1 {
		t.Errorf("round cap = %d", b.RoundCap)
	}
	if b.CheckWindow < time.Hour {
		t.Errorf("check window = %s: it has to outlast a queued Actions runner", b.CheckWindow)
	}
	if c.Upkeep.DependabotLogin == "" {
		t.Error("no dependabot login")
	}
}

func TestUpkeepReportsItsOwnMissingFields(t *testing.T) {
	c := complete()
	c.Upkeep.Enabled = true
	c.Upkeep.DependabotLogin = ""
	c.Upkeep.RoundCap = -1
	c.Upkeep.CheckWindow = "soon"
	c.Upkeep.Budget.Turns = 0
	_, err := c.Validate()
	if err == nil {
		t.Fatal("a broken upkeep section validated")
	}
	for _, want := range []string{"upkeep.dependabot_login", "upkeep.round_cap", "upkeep.check_window", "upkeep.budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// A broken upkeep section must not block a daemon that is not using it.
func TestUpkeepIsNotValidatedWhenOff(t *testing.T) {
	c := complete()
	c.Upkeep.CheckWindow = "soon"
	c.Upkeep.RoundCap = -1
	if _, err := c.Validate(); err != nil {
		t.Errorf("err = %v, want nil while upkeep is off", err)
	}
}
