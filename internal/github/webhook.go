package github

import (
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"

	gh "github.com/google/go-github/v88/github"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// maxWebhookBody caps what the handler will read. This is the one endpoint
// the public internet reaches, and the MAC is computed over the whole body,
// so an uncapped read is an unauthenticated memory allocation. GitHub's own
// payload ceiling is 25 MB.
const maxWebhookBody = 25 << 20

// WebhookHandler verifies, stores and acknowledges deliveries. Processing is
// asynchronous; the response is sent as soon as the row is committed.
func WebhookHandler(st *store.Store, secret []byte, clock resolution.Clock) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
		// JSON only: the form-encoded delivery format wraps the payload in
		// a field, so the bytes the MAC covers would not be the bytes we
		// parse and store as the payload.
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		// Require SHA-256 explicitly. ValidatePayload falls back to the
		// deprecated SHA-1 header when this one is missing, which would let
		// a caller pick the weaker scheme.
		if r.Header.Get("X-Hub-Signature-256") == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		payload, err := gh.ValidatePayload(r, secret)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				// The taxonomy has no 413; an over-long body is a bad
				// request like any other malformed one.
				writeError(w, http.StatusBadRequest, "invalid_request")
				return
			}
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
