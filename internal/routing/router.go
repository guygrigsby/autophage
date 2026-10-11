// Package routing is the host adapter between Aperture's live model
// registry and agent-select's offline router (ADR 0010). It translates the
// registry into runtime offers, asks the router for an ordering and hands
// back the Aperture model id to run. It owns no ranking or policy: those
// are agent-select's.
package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/guygrigsby/llm/aperture"

	ar "github.com/guygrigsby/agent-select/routing"
)

// Tier is a place autophage picks a model.
type Tier string

const (
	Triage   Tier = "triage"
	Attempt  Tier = "attempt"
	Approved Tier = "approved"
	Repair   Tier = "repair"
)

// Mode is the router's ordering mode for a tier.
type Mode string

const (
	// Cost orders by exact upper cost first. Zero-priced local models lead.
	Cost Mode = "cost"
	// Performance orders by estimated quality first. With no quality
	// signals in the snapshot every candidate is unscored and the order
	// falls back to cost, so a tier should leave this off until signals
	// exist (ADR 0010).
	Performance Mode = "performance"
)

// ErrNotRouted means the tier has no routing mode configured: the caller
// uses its static model id, with no warning because nothing went wrong.
var ErrNotRouted = errors.New("routing: tier is not routed")

// Registry reads the gateway's live model entries.
type Registry func(context.Context) ([]aperture.ModelInfo, error)

// Config wires a Router.
type Config struct {
	Registry Registry
	// Modes names the routed tiers. A tier absent here is not routed.
	Modes map[Tier]Mode
	// Local lists gateway provider ids whose models run on the operator's
	// own hardware. They are asserted free: the gateway prints no price for
	// them, and an unknown price would rank behind every priced model.
	Local []string
	// LocalContext is the context window asserted for a local model whose
	// registry entry reports none. Zero leaves it unknown, and a request
	// with a token quantity then rejects the model.
	LocalContext int64
	// Snapshot is an adopted agent-select artifact. Empty means an empty
	// snapshot: every offer is unscored.
	Snapshot []byte
	Clock    func() time.Time
}

// Router picks a model per tier from the live registry.
type Router struct {
	registry     Registry
	modes        map[Tier]Mode
	local        map[string]bool
	localContext int64
	engine       *ar.Engine
	clock        func() time.Time
}

// Choice is one routed pick.
type Choice struct {
	// ModelID is the Aperture id to build the model with.
	ModelID string
	OfferID string
	// Warnings are the router's, verbatim: unscored estimate, unknown cost.
	Warnings     []string
	SnapshotHash string
}

// tierRequest is what a tier asks of a model. Token quantities are
// conservative estimates, since they only gate context and cost.
type tierRequest struct {
	task       string
	input      int64
	output     int64
	needsTools bool
}

var tierRequests = map[Tier]tierRequest{
	Triage:   {task: "general", input: 8_000, output: 400},
	Attempt:  {task: "code", input: 150_000, output: 16_000, needsTools: true},
	Approved: {task: "code", input: 150_000, output: 16_000, needsTools: true},
	Repair:   {task: "code", input: 60_000, output: 8_000, needsTools: true},
}

// New builds a Router, adopting cfg.Snapshot or starting from an empty one.
func New(cfg Config) (*Router, error) {
	if cfg.Registry == nil {
		return nil, errors.New("routing: registry is required")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	var snap *ar.Snapshot
	var err error
	if len(cfg.Snapshot) > 0 {
		snap, err = ar.DecodeSnapshot(cfg.Snapshot)
	} else {
		now := clock().UTC().Truncate(time.Millisecond)
		snap, err = ar.NewSnapshot(ar.SnapshotData{Schema: 1, CreatedAt: now, KnowledgeCutoff: now, Policy: "educated-v1", Distribution: "local"})
	}
	if err != nil {
		return nil, fmt.Errorf("routing: snapshot: %w", err)
	}
	local := map[string]bool{}
	for _, p := range cfg.Local {
		local[p] = true
	}
	return &Router{registry: cfg.Registry, modes: cfg.Modes, local: local, localContext: cfg.LocalContext, engine: ar.NewEngine(snap), clock: clock}, nil
}

// Routed reports whether the tier picks from the registry.
func (r *Router) Routed(t Tier) bool { return r.modes[t] != "" }

// Choose reads the live registry and routes one request for the tier. Any
// error means the caller keeps its static model: the registry was
// unreachable, the request was malformed or nothing was admissible.
func (r *Router) Choose(ctx context.Context, tier Tier) (Choice, error) {
	mode := r.modes[tier]
	tr, ok := tierRequests[tier]
	if mode == "" || !ok {
		return Choice{}, ErrNotRouted
	}
	infos, err := r.registry(ctx)
	if err != nil {
		return Choice{}, fmt.Errorf("routing: registry: %w", err)
	}
	offers := toRuntime(r.local, r.localContext, infos)
	caps := []string{"text_input", "text_output"}
	if tr.needsTools {
		caps = append(caps, "tool_use")
	}
	now := r.clock().UTC().Truncate(time.Millisecond)
	req, err := ar.NewRequest(ar.RequestData{
		ID: fmt.Sprintf("%s-%d", tier, now.UnixMilli()), Task: tr.task, Mode: string(mode),
		EffectiveAt: now, RuntimeGeneration: generation(offers), Runtime: offers,
		RequiredCapabilities: caps, ImageOperation: "none",
		InputTokens: tr.input, OutputTokens: tr.output, AllowUnscored: true,
	})
	if err != nil {
		return Choice{}, fmt.Errorf("routing: request: %w", err)
	}
	dec, err := r.engine.Route(req)
	if err != nil || dec.Selected == nil {
		return Choice{}, fmt.Errorf("routing: nothing admissible among %d registry entries%s", len(offers), rejectionSummary(dec))
	}
	warnings := slices.Concat(dec.Warnings, dec.Selected.Warnings, dec.Selected.Estimate.Warnings)
	if !dec.Selected.Estimate.Known {
		warnings = append(warnings, "estimate_unscored")
	}
	slices.Sort(warnings)
	return Choice{ModelID: dec.Selected.UpstreamID, OfferID: dec.Selected.OfferID, Warnings: slices.Compact(warnings), SnapshotHash: dec.SnapshotHash}, nil
}

// rejectionSummary names the most common rejection reasons, so a log line
// says why a registry full of models yielded none.
func rejectionSummary(d ar.Decision) string {
	counts := map[string]int{}
	for _, rj := range d.Rejections {
		for _, reason := range rj.Reasons {
			counts[reason]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	reasons := make([]string, 0, len(counts))
	for reason, n := range counts {
		reasons = append(reasons, fmt.Sprintf("%s x%d", reason, n))
	}
	slices.Sort(reasons)
	return ": " + strings.Join(reasons, ", ")
}

const chatEndpoint = "/v1/chat/completions"

// toRuntime maps the registry one to one onto runtime offers. An entry is
// eligible when the gateway serves it on the endpoint this host's adapter
// speaks and authenticates the host for it; a listed model is not an
// executable one. Entries with an identity the router would reject are
// dropped, so one odd entry cannot fail every request.
func toRuntime(local map[string]bool, localContext int64, infos []aperture.ModelInfo) []ar.RuntimeOfferData {
	seen := map[string]bool{}
	var out []ar.RuntimeOfferData
	for _, m := range infos {
		if !validIDPart(m.Provider) || !validIDPart(m.ID) {
			continue
		}
		id := m.Provider + "/" + m.ID
		if seen[id] {
			continue
		}
		seen[id] = true
		o := ar.RuntimeOfferData{ID: id, Provider: m.Provider, UpstreamID: m.ID,
			Eligible: slices.Contains(m.Endpoints, chatEndpoint) && !m.RequiresClientAuth}
		if o.Eligible {
			// The aperture adapter speaks OpenAI chat with tool calling;
			// that is the host's capability, not a claim about the model.
			o.Capabilities = []string{"text_input", "text_output", "tool_use"}
		}
		ctxTokens := m.ContextTokens
		if ctxTokens == 0 && local[m.Provider] {
			ctxTokens = localContext
		}
		if ctxTokens > 0 {
			o.Limits = append(o.Limits, ar.LimitData{Resource: "context_tokens", Maximum: ctxTokens})
		}
		if m.MaxOutputTokens > 0 {
			o.Limits = append(o.Limits, ar.LimitData{Resource: "output_tokens", Maximum: m.MaxOutputTokens})
		}
		in, out2 := m.InputPrice, m.OutputPrice
		if local[m.Provider] {
			in, out2 = "0", "0"
		}
		pin, okIn := perMillion(in)
		pout, okOut := perMillion(out2)
		if okIn && okOut {
			o.Prices = []ar.PriceData{{Component: "input_tokens", USDPerMillion: pin}, {Component: "output_tokens", USDPerMillion: pout}}
		}
		out = append(out, o)
	}
	return out
}

// validIDPart mirrors the router's bound on identity strings.
func validIDPart(s string) bool {
	if s == "" || len(s) > 512 || s != strings.TrimSpace(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}

// perMillion converts the gateway's USD-per-token decimal to the router's
// USD-per-million decimal, exactly. It rejects anything that is not a plain
// nonnegative decimal, so an exponent form cannot inflate the rate.
func perMillion(perToken string) (string, bool) {
	if perToken == "" || strings.Count(perToken, ".") > 1 {
		return "", false
	}
	for _, r := range perToken {
		if (r < '0' || r > '9') && r != '.' {
			return "", false
		}
	}
	rat, ok := new(big.Rat).SetString(perToken)
	if !ok {
		return "", false
	}
	rat.Mul(rat, big.NewRat(1_000_000, 1))
	s := rat.FloatString(18)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s, true
}

// generation is the runtime generation: a hash over the whole sorted
// runtime projection, so any registry metadata change changes it.
func generation(offers []ar.RuntimeOfferData) string {
	norm := make([]ar.RuntimeOfferData, len(offers))
	for i, o := range offers {
		o.Capabilities = sortedCopy(o.Capabilities)
		o.UnsupportedCapabilities = sortedCopy(o.UnsupportedCapabilities)
		o.Limits = slices.SortedFunc(slices.Values(o.Limits), func(a, b ar.LimitData) int { return strings.Compare(a.Resource, b.Resource) })
		o.Prices = slices.SortedFunc(slices.Values(o.Prices), func(a, b ar.PriceData) int { return strings.Compare(a.Component, b.Component) })
		if o.Limits == nil {
			o.Limits = []ar.LimitData{}
		}
		if o.Prices == nil {
			o.Prices = []ar.PriceData{}
		}
		norm[i] = o
	}
	slices.SortFunc(norm, func(a, b ar.RuntimeOfferData) int {
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return strings.Compare(a.UpstreamID, b.UpstreamID)
	})
	b, err := ar.CanonicalEncode(norm)
	if err != nil {
		// Unreachable for DTOs the router already accepts; a distinct
		// constant keeps the request valid rather than panicking.
		return strings.Repeat("0", 64)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return out
}
