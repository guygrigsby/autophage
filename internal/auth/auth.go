// Package auth implements a single-token loopback auth model: the CLI mints a
// token over the loopback-only mint endpoint, appd persists its hash and
// validates bearer tokens against it. This is starter code each app is meant
// to customize, which is why it lives in the template rather than in perch.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const hashFile = "cli-token.hash"

// Mint generates a new random token, stores its SHA-256 hash (hex) at
// <dir>/cli-token.hash (0600), and returns the plaintext token once.
func Mint(dir string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	hexsum := base64.RawURLEncoding.EncodeToString(sum[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, hashFile), []byte(hexsum), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// Validate reports whether token matches the stored hash. A missing hash file
// is not an error: it returns (false, nil).
func Validate(dir, token string) (bool, error) {
	stored, err := os.ReadFile(filepath.Join(dir, hashFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256([]byte(token))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare(stored, []byte(want)) == 1, nil
}

// Middleware rejects requests whose bearer token fails Validate with 401.
func Middleware(dir string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ok, err := Validate(dir, token)
		if err != nil {
			http.Error(w, "auth check failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsLoopback reports whether remoteAddr (host:port, as in http.Request.RemoteAddr)
// is a loopback address.
func IsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
