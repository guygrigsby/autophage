package api

import (
	"encoding/json"
	"net/http"

	"github.com/guygrigsby/jess/ledger"

	"github.com/guygrigsby/autophage/internal/store"
)

// why renders the jess ledger chain for the attempt's run. Conformist to
// jess/ledger: the chain is returned in its own shape.
func (h *handlers) why(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.d.Store.GetCaseByAttempt(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var runID string
	for _, a := range c.Attempts() {
		if a.ID == id && a.Run != nil {
			runID = a.Run.RunID
		}
	}
	if runID == "" {
		writeErr(w, store.ErrNotFound)
		return
	}
	if h.d.Ledger == nil {
		writeErr(w, store.ErrNotFound)
		return
	}
	chain, err := h.d.Ledger.Chain(runID)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		AttemptID string       `json:"attempt_id"`
		RunID     string       `json:"run_id"`
		Chain     ledger.Chain `json:"chain"`
	}{id, runID, chain})
}
