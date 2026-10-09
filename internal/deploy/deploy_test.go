package deploy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// assetServer serves a release's files and counts downloads of the daemon
// binary, so tests can assert the no-op paths never fetch.
type assetServer struct {
	mu        sync.Mutex
	downloads int
	files     map[string][]byte
}

func (s *assetServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "autophaged-"+runtime.GOOS+"-"+runtime.GOARCH) {
		s.downloads++
	}
	body, ok := s.files[filepath.Base(r.URL.Path)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(body)
}

func (s *assetServer) downloadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.downloads
}

// release is a minimal release-published webhook payload.
func release(tag string, urls map[string]string) []byte {
	type asset struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	}
	ev := struct {
		Action  string `json:"action"`
		Release struct {
			TagName string  `json:"tag_name"`
			Assets  []asset `json:"assets"`
		} `json:"release"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}{Action: "published"}
	ev.Release.TagName = tag
	for name, url := range urls {
		ev.Release.Assets = append(ev.Release.Assets, asset{Name: name, BrowserDownloadURL: url})
	}
	ev.Repository.FullName = "guygrigsby/autophage"
	b, _ := json.Marshal(ev)
	return b
}

func checksummed(files map[string][]byte) []byte {
	lines := make([]string, 0, len(files))
	for name, body := range files {
		sum := sha256.Sum256(body)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+name)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// fixture is one handler wired against one asset server and one install dir.
type fixture struct {
	h       *Handler
	assets  *assetServer
	srv     *httptest.Server
	dir     string
	restart chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	assets := &assetServer{files: map[string][]byte{}}
	srv := httptest.NewServer(assets)
	t.Cleanup(srv.Close)
	restart := make(chan struct{}, 4)
	h := &Handler{
		Repo:       "guygrigsby/autophage",
		AssetBase:  srv.URL + "/",
		Secret:     []byte("s3cr3t"),
		Version:    "v0.0.1",
		InstallDir: dir,
		Client:     srv.Client(),
		Idle:       func() bool { return true },
		Restart:    func() { restart <- struct{}{} },
		Logf:       func(string, ...any) {},
	}
	return &fixture{h: h, assets: assets, srv: srv, dir: dir, restart: restart}
}

// publish sets up the release assets and posts the signed event.
func (f *fixture) publish(t *testing.T, tag string, extra map[string][]byte, corruptChecksum bool) *httptest.ResponseRecorder {
	t.Helper()
	plat := runtime.GOOS + "-" + runtime.GOARCH
	files := map[string][]byte{
		"autophaged-" + plat: []byte("daemon " + tag),
		"autophage-" + plat:  []byte("cli " + tag),
	}
	for k, v := range extra {
		files[k] = v
	}
	f.assets.files = files
	cs := checksummed(files)
	if corruptChecksum {
		// Flip the first hex digit to a different digit; hardcoding
		// "0" is a no-op on platforms where the hash already leads
		// with 0 and the install silently succeeds.
		cs = []byte(string(cs)[:strings.Index(string(cs), " ")])
		if cs[0] == '0' {
			cs[0] = '1'
		} else {
			cs[0] = '0'
		}
	}
	f.assets.files["checksums.txt"] = cs
	urls := map[string]string{}
	for name := range files {
		urls[name] = f.srv.URL + "/guygrigsby/autophage/releases/download/" + tag + "/" + name
	}
	body := release(tag, urls)
	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/webhook/deploy", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Hub-Signature-256", sig)
	r.Header.Set("X-GitHub-Event", "release")
	r.Header.Set("X-GitHub-Delivery", "d-"+tag)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *fixture) post(t *testing.T, body []byte, sig, event string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/webhook/deploy", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	if event != "" {
		r.Header.Set("X-GitHub-Event", event)
	}
	r.Header.Set("X-GitHub-Delivery", "d-x")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (f *fixture) installedDaemon(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "autophaged"))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestSignatureRequired(t *testing.T) {
	f := newFixture(t)
	if w := f.post(t, release("v0.0.2", nil), "", "release"); w.Code != http.StatusUnauthorized {
		t.Errorf("no sig: %d", w.Code)
	}
	body := release("v0.0.2", nil)
	if w := f.post(t, body, "sha256="+strings.Repeat("0", 64), "release"); w.Code != http.StatusUnauthorized {
		t.Errorf("bad sig: %d", w.Code)
	}
}

func TestOnlyReleaseEvents(t *testing.T) {
	f := newFixture(t)
	body := release("v0.0.2", nil)
	if w := f.post(t, body, sign(body), "push"); w.Code != http.StatusBadRequest {
		t.Errorf("push event: %d", w.Code)
	}
}

func TestRepoMismatch(t *testing.T) {
	f := newFixture(t)
	body := release("v0.0.2", nil)
	// right signature, wrong repo inside the payload
	dec := map[string]any{}
	if err := json.Unmarshal(body, &dec); err != nil {
		t.Fatal(err)
	}
	dec["repository"].(map[string]any)["full_name"] = "evil/autophage"
	body2, _ := json.Marshal(dec)
	if w := f.post(t, body2, sign(body2), "release"); w.Code != http.StatusForbidden {
		t.Errorf("repo mismatch: %d", w.Code)
	}
	if f.assets.downloadCount() != 0 {
		t.Error("mismatched repo downloaded assets")
	}
}

func TestNonPublishActionsIgnored(t *testing.T) {
	f := newFixture(t)
	body := release("v0.0.2", nil)
	dec := map[string]any{}
	if err := json.Unmarshal(body, &dec); err != nil {
		t.Fatal(err)
	}
	dec["action"] = "edited"
	body2, _ := json.Marshal(dec)
	if w := f.post(t, body2, sign(body2), "release"); w.Code != http.StatusOK {
		t.Errorf("edited: %d", w.Code)
	}
	if f.assets.downloadCount() != 0 {
		t.Error("non-publish action downloaded assets")
	}
}

func TestCurrentVersionIsANoOp(t *testing.T) {
	f := newFixture(t)
	if w := f.publish(t, "v0.0.1", nil, false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "current") {
		t.Errorf("current: %d %s", w.Code, w.Body.String())
	}
	if f.assets.downloadCount() != 0 {
		t.Error("current version downloaded assets")
	}
}

func TestInstallsAndRestartsWhenIdle(t *testing.T) {
	f := newFixture(t)
	w := f.publish(t, "v0.0.2", nil, false)
	if w.Code != http.StatusOK {
		t.Fatalf("install: %d %s", w.Code, w.Body.String())
	}
	if got := f.installedDaemon(t); got != "daemon v0.0.2" {
		t.Errorf("daemon binary = %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(f.dir, "autophage")); err != nil || string(b) != "cli v0.0.2" {
		t.Errorf("cli binary = %q %v", b, err)
	}
	waitFor(t, f.restart, "idle deploy did not restart")
}

func TestDefersRestartWhileBusy(t *testing.T) {
	f := newFixture(t)
	f.h.Idle = func() bool { return false }
	if w := f.publish(t, "v0.0.2", nil, false); w.Code != http.StatusAccepted {
		t.Fatalf("defer: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-f.restart:
		t.Fatal("busy deploy restarted")
	default:
	}
	// The scheduler reports an attempt finished but another is still
	// running: still no restart.
	f.h.Idle = func() bool { return false }
	f.h.MaybeRestart()
	select {
	case <-f.restart:
		t.Fatal("restart with attempts running")
	default:
	}
	// The last attempt finishes: restart fires exactly once.
	var idle atomicBool
	f.h.Idle = idle.get
	idle.set(true)
	f.h.MaybeRestart()
	waitFor(t, f.restart, "no restart once idle")
	time.Sleep(50 * time.Millisecond)
	select {
	case <-f.restart:
		t.Fatal("second restart from one deferred deploy")
	default:
	}
}

// waitFor asserts something arrives on ch within a generous bound for an
// async send.
func waitFor(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(msg)
	}
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) set(v bool) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicBool) get() bool  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

func TestRedeliveryWhileDeferredDoesNotRedownload(t *testing.T) {
	f := newFixture(t)
	f.h.Idle = func() bool { return false }
	f.publish(t, "v0.0.2", nil, false)
	f.publish(t, "v0.0.2", nil, false)
	if n := f.assets.downloadCount(); n != 1 {
		t.Errorf("downloads = %d, want 1", n)
	}
}

func TestChecksumMismatchInstallsNothing(t *testing.T) {
	f := newFixture(t)
	if w := f.publish(t, "v0.0.2", nil, true); w.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt: %d %s", w.Code, w.Body.String())
	}
	if got := f.installedDaemon(t); got != "" {
		t.Errorf("daemon binary = %q, want untouched", got)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(f.dir)
	if len(ents) != 0 {
		t.Errorf("leftovers: %v", ents)
	}
}

func TestForeignAssetURLRejected(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "v0.0.2", nil, false)
	plat := runtime.GOOS + "-" + runtime.GOARCH
	// A release whose daemon asset claims a URL outside the pinned repo.
	// The URL check fires before any download, so contents do not matter.
	body := release("v0.0.3", map[string]string{
		"checksums.txt":      f.srv.URL + "/guygrigsby/autophage/releases/download/v0.0.3/checksums.txt",
		"autophaged-" + plat: f.srv.URL + "/other/repo/releases/download/v0.0.3/autophaged-" + plat,
		"autophage-" + plat:  f.srv.URL + "/guygrigsby/autophage/releases/download/v0.0.3/autophage-" + plat,
	})
	if w := f.post(t, body, sign(body), "release"); w.Code != http.StatusBadRequest {
		t.Errorf("foreign url: %d %s", w.Code, w.Body.String())
	}
	if got := f.installedDaemon(t); got != "daemon v0.0.2" {
		t.Errorf("daemon binary = %q, want v0.0.2 kept", got)
	}
}

func TestMissingDaemonAssetFails(t *testing.T) {
	f := newFixture(t)
	// darwin-only release: no linux asset for this host platform is
	// faked by renaming; simplest is a release with only checksums.
	f.assets.files = map[string][]byte{"checksums.txt": []byte("")}
	body := release("v0.0.2", map[string]string{
		"checksums.txt": f.srv.URL + "/guygrigsby/autophage/releases/download/v0.0.2/checksums.txt",
	})
	if w := f.post(t, body, sign(body), "release"); w.Code != http.StatusInternalServerError {
		t.Errorf("missing asset: %d %s", w.Code, w.Body.String())
	}
}

// A stale redelivery must not roll the host back: it arrived, it is signed,
// but a newer release is already on disk.
func TestStaleReleaseDoesNotDowngrade(t *testing.T) {
	f := newFixture(t)
	f.h.Idle = func() bool { return false }
	f.publish(t, "v0.0.3", nil, false)
	// v0.0.2 is not the running build (that is v0.0.1, the fixture's
	// Version) but is older than what is on disk.
	if w := f.publish(t, "v0.0.2", nil, false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "older") {
		t.Errorf("older: %d %s", w.Code, w.Body.String())
	}
	if got := f.installedDaemon(t); got != "daemon v0.0.3" {
		t.Errorf("daemon binary = %q, want v0.0.3 kept", got)
	}
}

func TestTagNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.0.2", "v0.0.1", true},
		{"v0.1.0", "v0.0.99", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.0.1", "v0.0.2", false},
		{"v0.0.1", "v0.0.1", false},
		{"nightly", "v9.9.9", true}, // unparsable treated as newer
	}
	for _, c := range cases {
		if got := tagNewer(c.a, c.b); got != c.want {
			t.Errorf("tagNewer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
