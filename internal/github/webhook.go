package github

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	gh "github.com/google/go-github/v88/github"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// WebhookHandler verifies, stores and acknowledges deliveries. Processing is
// asynchronous; the response is sent as soon as the row is committed.
func WebhookHandler(st *store.Store, secret []byte, clock resolution.Clock) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := gh.ValidatePayload(r, secret)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		event, id := gh.WebHookType(r), gh.DeliveryID(r)
		if event == "" || id == "" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		dup, err := st.StoreDelivery(r.Context(), store.Delivery{
			ID: id, Event: event, Action: actionOf(payload), SenderLogin: senderOf(payload), Payload: payload, ReceivedAt: clock.Now(),
		})
		if err != nil {
			log.Printf("webhook: store delivery %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "internal")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"delivery_id": id, "duplicate": dup})
	})
}

func writeError(w http.ResponseWriter, code int, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": kind})
}

// actionOf reads payload.action without committing to an event type; empty
// for events without one (ping).
func actionOf(payload []byte) string {
	var p struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.Action
}

// senderOf reads payload.sender.login; empty for ping.
func senderOf(payload []byte) string {
	var p struct {
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.Sender.Login
}

var errUnsubscribed = errors.New("unsubscribed")
