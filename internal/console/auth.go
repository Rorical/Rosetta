package console

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie   = "rosetta_console"
	sessionLifetime = 12 * time.Hour
	minTokenBytes   = 24
	// A failed attempt holds one of loginSlots for this long, capping guesses
	// across all clients. Attempts queue instead of being refused, so a flood can
	// delay a sign-in but never lock out a correct token or an open session.
	failedLoginDelay = 250 * time.Millisecond
	loginSlots       = 4
)

// Auth holds one shared operator token and in-memory sessions. Restarting the
// console signs everyone out, which is acceptable for a handful of operators.
type Auth struct {
	digest [sha256.Size]byte

	mu       sync.Mutex
	sessions map[string]time.Time
}

func NewAuth(token string) (*Auth, error) {
	if len(token) < minTokenBytes {
		return nil, fmt.Errorf("operator token must be at least %d bytes", minTokenBytes)
	}
	return &Auth{digest: sha256.Sum256([]byte(token)), sessions: make(map[string]time.Time)}, nil
}

// Login returns a new session ID.
func (a *Auth) Login(token string, now time.Time) (string, error) {
	digest := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(digest[:], a.digest[:]) != 1 {
		return "", errors.New("invalid token")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	identifier := make([]byte, 32)
	if _, err := rand.Read(identifier); err != nil {
		return "", fmt.Errorf("generate session: %w", err)
	}
	session := hex.EncodeToString(identifier)
	for id, expires := range a.sessions {
		if now.After(expires) {
			delete(a.sessions, id)
		}
	}
	a.sessions[session] = now.Add(sessionLifetime)
	return session, nil
}

func (a *Auth) Valid(session string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.sessions[session]
	return ok && now.Before(expires)
}

func (a *Auth) Logout(session string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, session)
}

func sessionFrom(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// allowedHost blocks DNS rebinding: a hostile name resolving to 127.0.0.1 would
// otherwise let a page in the operator's browser reach this server.
func allowedHost(host string, public map[string]bool) bool {
	name := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		name = parsed
	}
	name = strings.Trim(name, "[]")
	if name == "localhost" || public[name] {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// sameOrigin rejects cross-site writes. SameSite=Strict already withholds the
// cookie; this also stops requests from pages on a sibling localhost port.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host == r.Host
}
