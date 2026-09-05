package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

func (h *handlers) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := h.d.Store.CountByState(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	open, err := h.d.Store.OpenAttempts(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	queued, err := h.d.Store.QueuedCases(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	attempting := make([]map[string]any, 0, len(open))
	for _, a := range open {
		attempting = append(attempting, map[string]any{"attempt_id": a.AttemptID, "repository": a.Repository, "number": a.Number, "ordinal": a.Ordinal, "elapsed_s": int(h.d.Clock.Now().Sub(a.StartedAt).Seconds())})
	}
	var last any
	pending, _ := h.d.Store.UnprocessedDeliveries(ctx)
	if len(pending) > 0 {
		last = pending[len(pending)-1].ReceivedAt
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": h.d.Version, "uptime_s": int(h.d.Clock.Now().Sub(h.d.StartedAt).Seconds()),
		"cases_by_state": counts, "attempting": attempting, "queue_depth": len(queued), "concurrency": h.d.Concurrency,
		"pending_deliveries": len(pending), "last_pending_delivery_at": last,
	})
}

func (h *handlers) listCases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.CaseFilter{State: q.Get("state"), Repository: q.Get("repository"), After: q.Get("cursor")}
	if f.State != "" {
		if _, err := resolution.ParseCaseState(f.State); err != nil {
			writeErr(w, fmt.Errorf("%w: %v", errInvalidRequest, err))
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, fmt.Errorf("%w: limit", errInvalidRequest))
			return
		}
		f.Limit = n
	}
	rows, next, err := h.d.Store.ListCases(r.Context(), f)
	if err != nil {
		writeErr(w, fmt.Errorf("%w: %v", errInvalidRequest, err))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{"repository": c.Repository, "number": c.Number, "state": c.State, "requester_login": c.RequesterLogin, "requester_trust": c.RequesterTrust, "received_at": c.ReceivedAt, "latest_outcome_kind": c.LatestOutcomeKind})
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out, "next_cursor": next})
}

func caseKey(r *http.Request) (string, int, error) {
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("%w: number", errInvalidRequest)
	}
	return r.PathValue("owner") + "/" + r.PathValue("repo"), n, nil
}

func (h *handlers) getCase(w http.ResponseWriter, r *http.Request) {
	repo, n, err := caseKey(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	c, err := h.d.Store.GetCase(r.Context(), repo, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, caseJSON(c))
}

// run is the operator's manual start: create the case from GitHub when it
// does not exist, then record an operator approval.
func (h *handlers) run(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo, n, err := caseKey(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := h.d.Store.GetRepository(ctx, repo); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := h.d.Store.GetCase(ctx, repo, n); errors.Is(err, store.ErrNotFound) {
		detail, err := h.d.GitHub.GetIssue(ctx, repo, n)
		if err != nil {
			writeErr(w, fmt.Errorf("%w: %v", errUpstream, err))
			return
		}
		c, err := resolution.NewCase(repo, n, detail.Requester, h.d.Clock.Now())
		if err != nil {
			writeErr(w, err)
			return
		}
		if err := h.d.Store.CreateCase(ctx, c); err != nil && !errors.Is(err, store.ErrConflict) {
			writeErr(w, err)
			return
		}
	} else if err != nil {
		writeErr(w, err)
		return
	}
	approver, err := resolution.NewRequester(h.d.OperatorLogin, resolution.AssociationOwner)
	if err != nil {
		writeErr(w, err)
		return
	}
	c, err := h.d.Store.UpdateCase(ctx, repo, n, func(c *resolution.Case) error {
		switch c.State() {
		case resolution.Queued, resolution.Attempting, resolution.Done, resolution.Closed:
			return resolution.Refused("run in state %s", c.State())
		}
		return c.RecordApproval(resolution.Approval{Approver: approver, Source: resolution.SourceOperator, ApprovedAt: h.d.Clock.Now()})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if h.d.Sweep != nil {
		go h.d.Sweep(contextWithoutCancel(ctx))
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"repository": repo, "number": n, "state": c.State()})
}

func caseJSON(c *resolution.Case) map[string]any {
	attempts := make([]map[string]any, 0, len(c.Attempts()))
	for _, a := range c.Attempts() {
		attempts = append(attempts, attemptJSON(a))
	}
	approvals := make([]map[string]any, 0, len(c.Approvals()))
	for _, a := range c.Approvals() {
		approvals = append(approvals, map[string]any{"approver": a.Approver.Login, "association": a.Approver.Association, "source": a.Source, "approved_at": a.ApprovedAt, "delivery_id": a.DeliveryID})
	}
	transitions := make([]map[string]any, 0, len(c.Transitions()))
	for _, t := range c.Transitions() {
		transitions = append(transitions, map[string]any{"from": t.From, "to": t.To, "cause": t.Cause, "occurred_at": t.OccurredAt})
	}
	out := map[string]any{
		"id": c.ID(), "repository": c.Repository(), "number": c.Number(), "state": c.State(), "branch": c.Branch(),
		"requester":   map[string]any{"login": c.Requester().Login, "association": c.Requester().Association, "trust": c.Requester().Trust},
		"received_at": c.ReceivedAt(), "approvals": approvals, "attempts": attempts, "transitions": transitions,
	}
	if t := c.Triage(); t != nil {
		out["triage"] = map[string]any{"size": t.Size, "rationale": t.Rationale, "model": t.Model, "triaged_at": t.TriagedAt}
	}
	if cl := c.Closure(); cl != nil {
		out["closure"] = map[string]any{"closed_at": cl.ClosedAt, "delivery_id": cl.DeliveryID}
	}
	return out
}

func attemptJSON(a resolution.Attempt) map[string]any {
	out := map[string]any{
		"id": a.ID, "ordinal": a.Ordinal, "kind": a.Kind, "started_at": a.StartedAt, "brief": a.Brief,
		"budget": map[string]any{"turns": a.Budget.MaxTurns(), "wall_clock": a.Budget.MaxWallClock().String(), "diff_lines": a.Budget.MaxDiffLines()},
	}
	if a.Run != nil {
		out["run"] = map[string]any{"run_id": a.Run.RunID, "model": a.Run.Model, "base_sha": a.Run.BaseSha, "began_at": a.Run.BeganAt}
	}
	if o := a.Outcome; o != nil {
		oj := map[string]any{"kind": o.Kind, "ended_at": o.EndedAt, "summary": o.Summary,
			"usage": map[string]any{"turns": o.Usage.Turns, "input_tokens": o.Usage.InputTokens, "output_tokens": o.Usage.OutputTokens, "wall_clock": o.Usage.WallClock.String(), "diff_lines": o.Usage.DiffLines}}
		switch o.Kind {
		case resolution.PullRequestOpened:
			oj["pr_number"], oj["head_sha"] = o.PRNumber, o.HeadSha
		case resolution.BudgetExhausted:
			oj["limit"] = o.Limit
		case resolution.FailedOutcome:
			oj["class"], oj["message"] = o.Class, o.Message
		case resolution.Aborted:
			oj["reason"] = o.Reason
		}
		out["outcome"] = oj
	}
	return out
}

func contextWithoutCancel(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }
