// Package routing is the host adapter between Aperture's live model
// registry and agent-select's ranker (ADR 0010). It turns the registry into
// candidates, asks route.Rank for an ordering and hands back the Aperture
// model id to run. Ranking policy is agent-select's.
package routing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/guygrigsby/llm/aperture"

	"github.com/guygrigsby/agent-select/route"
)

// Tier is a place autophage picks a model.
type Tier string

const (
	Triage   Tier = "triage"
	Attempt  Tier = "attempt"
	Approved Tier = "approved"
	Repair   Tier = "repair"
)

// Mode is the ordering for a tier: "cost" or "quality".
type Mode = route.Mode

const (
	Cost    = route.Cost
	Quality = route.Quality
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
	// LocalOnly restricts a routed tier to models on Local providers.
	LocalOnly map[Tier]bool
	// Local lists gateway provider ids whose models run on the operator's
	// own hardware. They are asserted free: the gateway prints no price for
	// them, and an unknown price ranks behind every priced model.
	Local []string
	// LocalContext maps a local model id to its context window, for models
	// whose registry entry reports none. A local model absent here has an
	// unknown window, and a request with a token quantity rejects it.
	LocalContext map[string]int64
	// Scorer supplies quality opinions; nil leaves every candidate unscored.
	Scorer route.Scorer
}

// Router picks a model per tier from the live registry.
type Router struct {
	registry     Registry
	modes        map[Tier]Mode
	localOnly    map[Tier]bool
	local        map[string]bool
	localContext map[string]int64
	scorer       route.Scorer
}

// Choice is one routed pick.
type Choice struct {
	// ModelID is the Aperture id to build the model with.
	ModelID string
	// Warnings are the ranker's for the pick: unscored, cost_unknown.
	Warnings []string
	// Score is the pick's quality basis, empty when unscored.
	Score string
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
	Triage:   {task: "triage", input: 8_000, output: 400},
	Attempt:  {task: "code", input: 150_000, output: 16_000, needsTools: true},
	Approved: {task: "code", input: 150_000, output: 16_000, needsTools: true},
	Repair:   {task: "code", input: 60_000, output: 8_000, needsTools: true},
}

// New builds a Router.
func New(cfg Config) (*Router, error) {
	if cfg.Registry == nil {
		return nil, errors.New("routing: registry is required")
	}
	local := map[string]bool{}
	for _, p := range cfg.Local {
		local[p] = true
	}
	return &Router{registry: cfg.Registry, modes: cfg.Modes, localOnly: cfg.LocalOnly, local: local, localContext: cfg.LocalContext, scorer: cfg.Scorer}, nil
}

// Routed reports whether the tier picks from the registry.
func (r *Router) Routed(t Tier) bool { return r.modes[t] != "" }

// Choose reads the live registry and ranks it for the tier. Any error means
// the caller keeps its static model: the registry was unreachable or
// nothing was admissible.
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
	cands := toCandidates(r.local, r.localContext, r.localOnly[tier], infos)
	task := route.Task{Name: tr.task, Mode: mode, Need: []string{"text"}, InputTokens: tr.input, OutputTokens: tr.output, AllowUnscored: true}
	if tr.needsTools {
		task.Need = append(task.Need, "tools")
	}
	res := route.Rank(cands, task, r.scorer)
	best, ok := res.Best()
	if !ok {
		return Choice{}, fmt.Errorf("routing: nothing admissible among %d registry entries%s", len(cands), rejectionSummary(res))
	}
	c := Choice{ModelID: best.ID, Warnings: best.Warnings}
	if best.Score != nil {
		c.Score = best.Score.Basis
	}
	return c, nil
}

// rejectionSummary names the rejection reasons by count, so a log line says
// why a registry full of models yielded none.
func rejectionSummary(res route.Result) string {
	counts := map[string]int{}
	for _, rj := range res.Rejected {
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

// toCandidates maps the registry onto candidates. Only entries the gateway
// serves on the endpoint this host's adapter speaks and authenticates the
// host for are candidates: a listed model is not an executable one. The
// aperture adapter wires OpenAI chat with tool calling, so every candidate
// carries "text" and "tools"; that is the host's capability, not a claim
// about any model's quality. onlyLocal drops every non-local provider.
// Duplicate ids keep the first.
func toCandidates(local map[string]bool, localContext map[string]int64, onlyLocal bool, infos []aperture.ModelInfo) []route.Candidate {
	seen := map[string]bool{}
	var out []route.Candidate
	for _, m := range infos {
		if !validID(m.ID) || seen[m.ID] {
			continue
		}
		if !slices.Contains(m.Endpoints, chatEndpoint) || m.RequiresClientAuth || (onlyLocal && !local[m.Provider]) {
			continue
		}
		seen[m.ID] = true
		c := route.Candidate{ID: m.ID, Caps: []string{"text", "tools"}, ContextTokens: m.ContextTokens, MaxOutputTokens: m.MaxOutputTokens}
		isLocal := local[m.Provider]
		if c.ContextTokens == 0 && isLocal {
			c.ContextTokens = localContext[m.ID]
		}
		if isLocal {
			zero := 0.0
			c.InputUSDPerMTok, c.OutputUSDPerMTok = &zero, &zero
		} else {
			c.InputUSDPerMTok, c.OutputUSDPerMTok = perMillion(m.InputPrice), perMillion(m.OutputPrice)
		}
		out = append(out, c)
	}
	return out
}

func validID(s string) bool {
	if s == "" || s != strings.TrimSpace(s) {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// perMillion converts the gateway's USD-per-token decimal to USD per
// million. Anything that is not a plain nonnegative decimal is unknown.
func perMillion(perToken string) *float64 {
	if perToken == "" || strings.Trim(perToken, "0123456789.") != "" {
		return nil
	}
	v, err := strconv.ParseFloat(perToken, 64)
	if err != nil {
		return nil
	}
	v *= 1e6
	return &v
}
