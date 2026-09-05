package resolution

// Requester is the person who opened the issue or added the label, with the
// trust verdict made at the time under the rule then in force.
type Requester struct {
	Login       string
	Association Association
	Trust       Trust
}

// NewRequester is the only way to build a Requester: it computes the trust
// verdict. Owner, member and collaborator are trusted; everything else is
// not. CONTRIBUTOR means any merged PR ever, which is not a trust signal.
func NewRequester(login string, assoc Association) (Requester, error) {
	if login == "" {
		return Requester{}, Invalid("requester login is empty")
	}
	if _, err := ParseAssociation(string(assoc)); err != nil {
		return Requester{}, err
	}
	trust := Untrusted
	switch assoc {
	case AssociationOwner, AssociationMember, AssociationCollaborator:
		trust = Trusted
	}
	return Requester{Login: login, Association: assoc, Trust: trust}, nil
}
