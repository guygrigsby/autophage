package resolution

import "testing"

func TestNewRequesterDerivesTrust(t *testing.T) {
	cases := []struct {
		assoc Association
		want  Trust
	}{
		{AssociationOwner, Trusted},
		{AssociationMember, Trusted},
		{AssociationCollaborator, Trusted},
		{AssociationContributor, Untrusted},
		{AssociationFirstTimeContributor, Untrusted},
		{AssociationFirstTimer, Untrusted},
		{AssociationNone, Untrusted},
	}
	for _, c := range cases {
		r, err := NewRequester("alice", c.assoc)
		if err != nil {
			t.Fatalf("%s: %v", c.assoc, err)
		}
		if r.Trust != c.want {
			t.Errorf("%s: trust = %s, want %s", c.assoc, r.Trust, c.want)
		}
	}
}

func TestNewRequesterRefusesBadInput(t *testing.T) {
	if _, err := NewRequester("", AssociationOwner); err == nil {
		t.Error("empty login accepted")
	}
	if _, err := NewRequester("alice", Association("boss")); err == nil {
		t.Error("unknown association accepted")
	}
}

func TestParseAssociationFromGitHubCasing(t *testing.T) {
	a, err := ParseAssociation("FIRST_TIME_CONTRIBUTOR")
	if err != nil || a != AssociationFirstTimeContributor {
		t.Errorf("got %s, %v", a, err)
	}
	if _, err := ParseAssociation("nope"); err == nil {
		t.Error("unknown value accepted")
	}
}
