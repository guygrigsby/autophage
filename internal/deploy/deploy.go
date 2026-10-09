// Package deploy serves POST /webhook/deploy: the repository's release
// webhook. A published release from the pinned repository is verified,
// downloaded and installed over the running binaries (copy-then-rename, like
// make install), and the daemon restarts itself when idle so the new binary
// takes over. An attempt in flight defers the restart, not the install: the
// file swap is atomic and invisible to the running process.
//
// The trust story: the delivery's HMAC (AUTOPHAGE_DEPLOY_WEBHOOK_SECRET)
// pins the payload, the payload's repository must equal Repo, and every
// asset URL must live under the pinned repository's releases. A secret
// leak is host code execution, so this secret is never shared with the
// App webhook secret.
package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v88/github"
)

// maxDeployBody bounds the release payload. Release events carry an asset
// list, not the assets; a megabyte is generous.
const maxDeployBody = 1 << 20

// Handler is the deploy webhook. Construct with an explicit Secret and Repo;
// an empty Repo means the caller should not route it at all.
type Handler struct {
	// Repo is the only repository releases are accepted from, owner/name.
	Repo string
	// Secret verifies X-Hub-Signature-256, shared only with GitHub.
	Secret []byte
	// Version is the release tag this process was built from. A delivery
	// of exactly it is a redelivery and a no-op.
	Version string
	// InstallDir is where the binaries are renamed into.
	InstallDir string
	// Client downloads release assets.
	Client *http.Client
	// Idle reports whether no attempt and no repair round is running.
	Idle func() bool
	// Restart hands the process over to the new binary (the daemon
	// SIGTERMs itself; systemd's Restart=always brings it back).
	Restart func()
	// AssetBase is where release downloads must live, Repo-relative. It
	// defaults to https://github.com/ and exists for tests.
	AssetBase string
	// Logf defaults to log.Printf.
	Logf func(string, ...any)

	mu        sync.Mutex
	installed string // tag already on disk, "" when none
	pending   bool   // installed, waiting for an idle moment to restart
}

type releaseEvent struct {
	Action  string `json:"action"`
	Release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	} `json:"release"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (h *Handler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "post only"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDeployBody)
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if r.Header.Get("X-Hub-Signature-256") == "" || len(h.Secret) == 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	payload, err := gh.ValidatePayload(r, h.Secret)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	if gh.WebHookType(r) != "release" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "release events only"})
		return
	}
	var ev releaseEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.Release.TagName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	if ev.Action != "published" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "action": ev.Action})
		return
	}
	if ev.Repository.FullName != h.Repo {
		h.logf("deploy: refusing release from %s, pinned to %s", ev.Repository.FullName, h.Repo)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong repository"})
		return
	}

	tag := ev.Release.TagName
	if tag == h.Version {
		writeJSON(w, http.StatusOK, map[string]string{"status": "current", "version": tag})
		return
	}

	h.mu.Lock()
	if tag != h.installed {
		// A stale redelivery (GitHub retries one that timed out) must
		// not roll the host back behind what is already on disk.
		// Rolling back is a deliberate act: the runbook's manual
		// download-and-rename.
		if h.installed != "" && !tagNewer(tag, h.installed) {
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]string{"status": "older than installed", "installed": h.installed})
			return
		}
		h.install(r, w, tag, ev)
	}
	h.mu.Unlock()
	// A redelivery of a release already on disk (typical when the first
	// delivery deferred the restart) falls through to the cutover too:
	// the install is done, only the restart may remain.
	h.cutOver(w)
}

// install downloads and installs the release's binaries. The handler's
// mutex must be held: one install at a time, and h.installed is only set
// once every asset landed.
func (h *Handler) install(r *http.Request, w http.ResponseWriter, tag string, ev releaseEvent) {
	assets := map[string]string{}
	for _, a := range ev.Release.Assets {
		assets[a.Name] = a.BrowserDownloadURL
	}
	want := []string{"autophaged-" + runtime.GOOS + "-" + runtime.GOARCH, "autophage-" + runtime.GOOS + "-" + runtime.GOARCH}
	base := h.AssetBase
	if base == "" {
		base = "https://github.com/"
	}
	prefix := base + h.Repo + "/releases/download/"
	for _, name := range append([]string{"checksums.txt"}, want...) {
		url, ok := assets[name]
		if name == "autophage-"+runtime.GOOS+"-"+runtime.GOARCH && !ok {
			continue // the CLI asset is optional; the daemon is the one that matters
		}
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "release has no " + name})
			return
		}
		if !assetURLAllowed(url, prefix) {
			h.logf("deploy: asset %s points outside %s: %s", name, h.Repo, url)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "asset url outside the pinned repository"})
			return
		}
	}

	sums, err := h.fetchSums(r.Context(), assets["checksums.txt"])
	if err != nil {
		h.fail(w, "checksums", err)
		return
	}
	for _, name := range want {
		if _, ok := assets[name]; !ok {
			continue
		}
		sum, ok := sums[name]
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": name + " missing from checksums.txt"})
			return
		}
		dst := strings.TrimSuffix(name, "-"+runtime.GOOS+"-"+runtime.GOARCH)
		if err := h.fetchVerifyInstall(r.Context(), assets[name], sum, filepath.Join(h.InstallDir, dst)); err != nil {
			h.fail(w, name, err)
			return
		}
	}
	h.installed = tag
	h.logf("deploy: installed %s from release %s", strings.Join(binaryNames(want, assets), ", "), tag)
}

// tagNewer reports whether a sorts strictly after b as a vX.Y.Z tag.
// Anything unparsable is treated as newer: an unusual tag is an operator's
// deliberate act, not something to silently refuse.
func tagNewer(a, b string) bool {
	pa, oka := parseTag(a)
	pb, okb := parseTag(b)
	if !oka || !okb {
		return true
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseTag(t string) ([3]int, bool) {
	var out [3]int
	if !strings.HasPrefix(t, "v") {
		return out, false
	}
	parts := strings.Split(strings.TrimPrefix(t, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}

// assetURLAllowed accepts only the pinned repository's release-download
// prefix, with no dot segments that could walk the path to another
// repository on the same host.
func assetURLAllowed(url, prefix string) bool {
	if !strings.HasPrefix(url, prefix) {
		return false
	}
	rest := strings.TrimPrefix(url, prefix)
	return !strings.Contains(rest, "/../") && !strings.HasSuffix(rest, "/..") && !strings.Contains(rest, "../")
}

// cutOver answers the delivery and restarts if nothing is running. It must
// be called WITHOUT the mutex: Idle reads the schedulers' running sets,
// and the schedulers call MaybeRestart from inside their own locks, so
// evaluating Idle under this mutex could deadlock against them.
func (h *Handler) cutOver(w http.ResponseWriter) {
	if h.Idle == nil || h.Idle() {
		h.mu.Lock()
		h.pending = false
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "restarting"})
		// Asynchronous on purpose: Restart SIGTERMs the process, and the
		// graceful shutdown waits for this handler to finish answering.
		if h.Restart != nil {
			go h.Restart()
		}
		return
	}
	h.mu.Lock()
	h.pending = true
	h.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deferred until idle"})
}

// MaybeRestart cuts over after the last attempt or repair round ended. Wire
// it to the schedulers' on-done hook. Like cutOver it never evaluates Idle
// under the mutex: the caller holds a scheduler lock that Idle reads.
func (h *Handler) MaybeRestart() {
	h.mu.Lock()
	pending := h.pending
	h.mu.Unlock()
	if !pending || h.Idle == nil || !h.Idle() {
		return
	}
	h.mu.Lock()
	if !h.pending {
		h.mu.Unlock()
		return
	}
	h.pending = false
	h.mu.Unlock()
	h.logf("deploy: idle, restarting into the installed release")
	if h.Restart != nil {
		go h.Restart()
	}
}

func binaryNames(want []string, assets map[string]string) []string {
	out := make([]string, 0, len(want))
	for _, name := range want {
		if _, ok := assets[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	h.logf("deploy: %s: %v", what, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": what + ": " + err.Error()})
}

func (h *Handler) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (h *Handler) fetchSums(ctx context.Context, url string) (map[string]string, error) {
	body, err := h.get(ctx, url)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			sums[fields[1]] = fields[0]
		}
	}
	return sums, nil
}

// fetchVerifyInstall downloads the asset, checks it against the checksum
// from the release, and renames it over dst. The temp file lives in dst's
// directory so the rename is same-filesystem and atomic.
func (h *Handler) fetchVerifyInstall(ctx context.Context, url, sum, dst string) error {
	body, err := h.get(ctx, url)
	if err != nil {
		return err
	}
	got := sha256.Sum256(body)
	if hex.EncodeToString(got[:]) != sum {
		return fmt.Errorf("checksum mismatch")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".deploy-*")
	if err != nil {
		return err
	}
	// Best-effort cleanup: on the happy path the rename consumed the
	// temp file and there is nothing to remove.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (h *Handler) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	// Release binaries are tens of megabytes; bound at something no real
	// build will reach but a confused server cannot exceed.
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

func writeJSON(w http.ResponseWriter, code int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
