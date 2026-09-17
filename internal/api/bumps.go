package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

func (h *handlers) listBumps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.BumpFilter{State: q.Get("state"), Repository: q.Get("repository"), After: q.Get("cursor")}
	if f.State != "" {
		if _, err := upkeep.ParseBumpState(f.State); err != nil {
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
	rows, next, err := h.d.Store.ListBumps(r.Context(), f)
	if err != nil {
		if errors.Is(err, store.ErrBadCursor) {
			writeErr(w, fmt.Errorf("%w: cursor", errInvalidRequest))
			return
		}
		writeErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		out = append(out, map[string]any{
			"repository": b.Repository, "number": b.Number, "branch": b.Branch, "state": b.State,
			"head_sha": b.HeadSha, "rounds": b.Rounds, "latest_conclusion": b.LatestConclusion, "opened_at": b.OpenedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"bumps": out, "next_cursor": next})
}

func (h *handlers) getBump(w http.ResponseWriter, r *http.Request) {
	repo, n, err := caseKey(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := h.d.Store.GetBump(r.Context(), repo, n)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bumpJSON(b))
}

// retryBump is the operator's recourse against a sticky abandonment, and the
// only thing in the system that undoes one. It re-queues; the scheduler
// still decides when the round runs, so the operator cannot jump the queue.
func (h *handlers) retryBump(w http.ResponseWriter, r *http.Request) {
	repo, n, err := caseKey(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := h.d.Store.UpdateBump(r.Context(), repo, n, func(b *upkeep.Bump) error {
		return b.Retry(h.d.Clock.Now())
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	go h.d.Sweep(h.d.Base)
	writeJSON(w, http.StatusAccepted, map[string]any{"repository": b.Repository(), "number": b.Number(), "state": string(b.State())})
}

func (h *handlers) getRepair(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := h.d.Store.GetBumpByRepair(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, a := range b.Attempts() {
		if a.ID == id {
			writeJSON(w, http.StatusOK, repairJSON(b, a))
			return
		}
	}
	writeErr(w, store.ErrNotFound)
}

// stopRepair cancels a running round. The runner records
// Aborted{OperatorStop} after the push, so whatever the agent committed
// still reaches the branch.
func (h *handlers) stopRepair(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := h.d.Store.GetBumpByRepair(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	a := b.OpenRepair()
	if a == nil || a.ID != id {
		writeErr(w, resolution.Refused("repair round %s is not open", id))
		return
	}
	if h.d.StopRepair == nil || !h.d.StopRepair(id) {
		writeErr(w, resolution.Refused("repair round %s is not running in this daemon", id))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"repair_id": id,
		"bump":      map[string]any{"repository": b.Repository(), "number": b.Number()},
		"state":     string(b.State()),
	})
}

// watch and unwatch are the operator's opt-in. Enrollment authorises issues;
// a watch authorises bumps, and it is deliberately a second decision.
func (h *handlers) watch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	repository, err := h.d.Store.GetRepository(r.Context(), repo)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !repository.Enrolled() {
		writeErr(w, store.ErrNotFound)
		return
	}
	watch, err := upkeep.NewWatch(repo, h.d.Clock.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := h.d.Store.WatchRepository(r.Context(), watch); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repository": repo, "watched_at": watch.WatchedAt})
}

func (h *handlers) unwatch(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	if err := h.d.Store.UnwatchRepository(r.Context(), repo); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repository": repo, "watched": false})
}

func bumpJSON(b *upkeep.Bump) map[string]any {
	verdicts := make([]map[string]any, 0, len(b.Verdicts()))
	for _, v := range b.Verdicts() {
		verdicts = append(verdicts, map[string]any{
			"head_sha": v.HeadSha, "conclusion": string(v.Conclusion),
			"failing_contexts": v.FailingContexts, "details_url": v.DetailsURL, "concluded_at": v.ConcludedAt,
		})
	}
	rounds := make([]map[string]any, 0, len(b.Attempts()))
	for _, a := range b.Attempts() {
		rounds = append(rounds, roundJSON(a))
	}
	transitions := make([]map[string]any, 0, len(b.Transitions()))
	for _, t := range b.Transitions() {
		transitions = append(transitions, map[string]any{"from": string(t.From), "to": string(t.To), "cause": string(t.Cause), "occurred_at": t.OccurredAt})
	}
	out := map[string]any{
		"repository": b.Repository(), "number": b.Number(), "branch": b.Branch(), "base_branch": b.BaseBranch(),
		"head_sha": b.HeadSha(), "state": string(b.State()), "opened_at": b.OpenedAt(),
		"verdicts": verdicts, "rounds": rounds, "transitions": transitions,
	}
	if a := b.Abandonment(); a != nil {
		out["abandonment"] = map[string]any{"reason": string(a.Reason), "detail": a.Detail, "abandoned_at": a.AbandonedAt}
	}
	if c := b.Closure(); c != nil {
		out["closure"] = map[string]any{"kind": string(c.Kind), "delivery_id": c.DeliveryID, "closed_at": c.ClosedAt}
	}
	return out
}

func roundJSON(a upkeep.RepairAttempt) map[string]any {
	out := map[string]any{
		"repair_id": a.ID, "round": a.Round, "base_sha": a.BaseSha, "brief": a.Brief, "started_at": a.StartedAt,
		"budget": map[string]any{
			"max_turns": a.Budget.MaxTurns(), "max_wall_clock_s": int(a.Budget.MaxWallClock().Seconds()), "max_diff_lines": a.Budget.MaxDiffLines(),
		},
	}
	if a.Run != nil {
		out["run"] = map[string]any{"run_id": a.Run.RunID, "model": a.Run.Model, "base_sha": a.Run.BaseSha, "began_at": a.Run.BeganAt}
	}
	if o := a.Outcome; o != nil {
		outcome := map[string]any{
			"kind": string(o.Kind), "summary": o.Summary, "ended_at": o.EndedAt,
			"usage": map[string]any{
				"turns": o.Usage.Turns, "input_tokens": o.Usage.InputTokens, "output_tokens": o.Usage.OutputTokens,
				"wall_clock_s": int(o.Usage.WallClock.Seconds()), "diff_lines": o.Usage.DiffLines,
			},
		}
		// Only the variant fields the kind says are meaningful, so a reader
		// never has to know which empty strings to ignore.
		switch o.Kind {
		case upkeep.Pushed:
			outcome["head_sha"] = o.HeadSha
		case upkeep.BudgetExhausted:
			outcome["limit"] = string(o.Limit)
		case upkeep.RepairFailed:
			outcome["class"] = string(o.Class)
			outcome["message"] = o.Message
		case upkeep.RepairAborted:
			outcome["reason"] = string(o.Reason)
		}
		out["outcome"] = outcome
	}
	return out
}

func repairJSON(b *upkeep.Bump, a upkeep.RepairAttempt) map[string]any {
	out := roundJSON(a)
	out["bump"] = map[string]any{"repository": b.Repository(), "number": b.Number(), "state": string(b.State())}
	return out
}

// upkeepStatus adds the Upkeep half to the status payload. Always present,
// even with upkeep off: an operator reading zeros learns that nothing is
// happening, where a missing key leaves them wondering which build they are
// talking to.
func (h *handlers) upkeepStatus(ctx context.Context, out map[string]any) error {
	counts, err := h.d.Store.CountBumpsByState(ctx)
	if err != nil {
		return err
	}
	open, err := h.d.Store.OpenRepairs(ctx)
	if err != nil {
		return err
	}
	queued, err := h.d.Store.QueuedBumps(ctx)
	if err != nil {
		return err
	}
	watched, err := h.d.Store.WatchedRepositories(ctx)
	if err != nil {
		return err
	}
	repairing := make([]map[string]any, 0, len(open))
	for _, o := range open {
		repairing = append(repairing, map[string]any{
			"repair_id": o.ID, "repository": o.Repository, "number": o.Number, "round": o.Round,
		})
	}
	out["bumps_by_state"] = counts
	out["repairing"] = repairing
	out["repair_queue_depth"] = len(queued)
	out["watched_repositories"] = len(watched)
	return nil
}
