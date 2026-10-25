package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

const (
	// SessionCookieName is the name of the cookie that carries the session
	// token.
	SessionCookieName = "hmoob_session"
	// CSRFHeader is the header that carries the CSRF token of a modifying
	// request.
	CSRFHeader = "X-CSRF-Token"
)

// SessionFromRequest returns the session that the session cookie of the
// request identifies. A missing, unknown, expired or revoked cookie is
// reported as ErrInvalidSession.
func (s *Service) SessionFromRequest(ctx context.Context, r *http.Request) (Session, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, ErrInvalidSession
	}
	return s.Authenticate(ctx, cookie.Value)
}

// CheckCSRF reports whether the request carries the CSRF token of the
// session. The comparison takes the same time for every value.
func CheckCSRF(r *http.Request, session Session) bool {
	presented := strings.TrimSpace(r.Header.Get(CSRFHeader))
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(session.CSRF)) == 1
}
