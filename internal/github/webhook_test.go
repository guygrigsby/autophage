package github

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/storetest"
)

var secret = []byte("s3cret")

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func post(t *testing.T, h http.Handler, event, delivery string, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookStoresVerifiedDelivery(t *testing.T) {
	st := storetest.Open(t)
	h := WebhookHandler(st, secret, resolution.SystemClock{})
	body := fixture(t, "issues_opened.json")
	rec := post(t, h, "issues", "d-1", body, sign(body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	var out struct {
		DeliveryID string `json:"delivery_id"`
		Duplicate  bool   `json:"duplicate"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.DeliveryID != "d-1" || out.Duplicate {
		t.Errorf("out = %+v", out)
	}
	rec = post(t, h, "issues", "d-1", body, sign(body))
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusAccepted || !out.Duplicate {
		t.Errorf("redelivery: code %d out %+v", rec.Code, out)
	}
	pending, _ := st.UnprocessedDeliveries(t.Context())
	if len(pending) != 1 || pending[0].Event != "issues" || pending[0].Action != "opened" || pending[0].SenderLogin != "guy" || !bytes.Equal(pending[0].Payload, body) {
		t.Errorf("stored = %+v", pending)
	}
}

func TestWebhookRejectsBadSignatureAndMissingHeaders(t *testing.T) {
	st := storetest.Open(t)
	h := WebhookHandler(st, secret, resolution.SystemClock{})
	body := fixture(t, "issues_opened.json")
	if rec := post(t, h, "issues", "d-2", body, "sha256=deadbeef"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad sig code = %d", rec.Code)
	}
	if rec := post(t, h, "issues", "d-3", body, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no sig code = %d", rec.Code)
	}
	if rec := post(t, h, "", "d-4", body, sign(body)); rec.Code != http.StatusBadRequest {
		t.Errorf("no event code = %d", rec.Code)
	}
	if rec := post(t, h, "issues", "", body, sign(body)); rec.Code != http.StatusBadRequest {
		t.Errorf("no delivery code = %d", rec.Code)
	}
	if pending, _ := st.UnprocessedDeliveries(t.Context()); len(pending) != 0 {
		t.Errorf("rejected deliveries stored: %+v", pending)
	}
}
