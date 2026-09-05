package api

import (
	"net/http"

	"github.com/guygrigsby/autophage/internal/resolution"
)

func (h *handlers) getAttempt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.d.Store.GetCaseByAttempt(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, a := range c.Attempts() {
		if a.ID == id {
			out := attemptJSON(a)
			out["case"] = map[string]any{"repository": c.Repository(), "number": c.Number()}
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	writeErr(w, resolution.Refused("attempt not on its case"))
}

// stop cancels a running attempt through the daemon's Stop hook; the runner
// records Aborted{OperatorStop}. A second stop, or a stop on an ended
// attempt, is a conflict.
func (h *handlers) stop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.d.Store.GetCaseByAttempt(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	a := c.OpenAttempt()
	if a == nil || a.ID != id {
		writeErr(w, resolution.Refused("attempt %s is not open", id))
		return
	}
	if h.d.Stop == nil || !h.d.Stop(id) {
		writeErr(w, resolution.Refused("attempt %s is not running in this daemon", id))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"attempt_id": id, "case": map[string]any{"repository": c.Repository(), "number": c.Number()}, "state": c.State()})
}
