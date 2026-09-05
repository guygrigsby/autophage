package github

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
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

// TestWebhookRejectsOversizedBody proves the handler caps what it will read
// before verifying it: without the cap, anyone who knows the public Funnel
// path can make the daemon buffer an unbounded body in memory, signature or
// no signature, because the MAC is computed over the whole thing.
func TestWebhookRejectsOversizedBody(t *testing.T) {
	st := storetest.Open(t)
	h := WebhookHandler(st, secret, resolution.SystemClock{})
	body := bytes.Repeat([]byte("a"), maxWebhookBody+1)
	rec := post(t, h, "issues", "d-big", body, sign(body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body code = %d, want 400", rec.Code)
	}
	if pending, _ := st.UnprocessedDeliveries(t.Context()); len(pending) != 0 {
		t.Errorf("oversized delivery stored: %+v", pending)
	}
}

// TestWebhookRejectsNonJSONContentType proves the handler takes JSON only.
// GitHub can be configured to send form-encoded deliveries, where the body
// bytes the MAC covers are not the bytes we would parse as the payload.
func TestWebhookRejectsNonJSONContentType(t *testing.T) {
	st := storetest.Open(t)
	h := WebhookHandler(st, secret, resolution.SystemClock{})
	body := fixture(t, "issues_opened.json")
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "d-form")
	req.Header.Set("X-Hub-Signature-256", sign(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("form content type code = %d, want 400", rec.Code)
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", out["error"])
	}
}

// TestWebhookRejectsSHA1OnlySignature proves the SHA-1 header is not a
// fallback: go-github's ValidatePayload accepts X-Hub-Signature when the
// SHA-256 header is absent, and SHA-1 HMAC is the weaker scheme GitHub
// itself deprecates. The contract names X-Hub-Signature-256 alone.
func TestWebhookRejectsSHA1OnlySignature(t *testing.T) {
	st := storetest.Open(t)
	h := WebhookHandler(st, secret, resolution.SystemClock{})
	body := fixture(t, "issues_opened.json")
	m := hmac.New(sha1.New, secret)
	m.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "d-sha1")
	req.Header.Set("X-Hub-Signature", "sha1="+hex.EncodeToString(m.Sum(nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("sha1-only code = %d, want 401", rec.Code)
	}
	if pending, _ := st.UnprocessedDeliveries(t.Context()); len(pending) != 0 {
		t.Errorf("sha1-signed delivery stored: %+v", pending)
	}
}
