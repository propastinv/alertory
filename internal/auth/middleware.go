package auth

import (
	"context"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/db"
)

// Session is the authenticated identity attached to a request's context by
// RequireAuth.
type Session struct {
	Subject   string
	Email     string
	Name      string
	CSRFToken string
}

type contextKey int

const sessionContextKey contextKey = 0

// RequireAuth only lets a request through if it carries a valid,
// unexpired session cookie; otherwise it redirects to the OIDC login flow,
// preserving the originally requested path so the user lands back where
// they meant to go.
func RequireAuth(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			redirectToLogin(w, r)
			return
		}

		sess, err := db.GetWebSession(r.Context(), pool, cookie.Value)
		if err != nil || sess == nil {
			redirectToLogin(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), sessionContextKey, &Session{
			Subject:   sess.Subject,
			Email:     sess.Email,
			Name:      sess.Name,
			CSRFToken: sess.CSRFToken,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/auth/login?return_to="+url.QueryEscape(r.URL.Path), http.StatusFound)
}

// FromContext returns the authenticated session for a request, if any.
func FromContext(ctx context.Context) (*Session, bool) {
	sess, ok := ctx.Value(sessionContextKey).(*Session)
	return sess, ok
}

// CheckCSRF validates a submitted csrf_token form value against the
// current session's token. Call after r.ParseForm(). Every state-changing
// (POST) handler behind RequireAuth must call this, since a session
// cookie alone doesn't protect against cross-site form submission the way
// the old bearer-token-only webhook did.
func CheckCSRF(r *http.Request) bool {
	sess, ok := FromContext(r.Context())
	if !ok {
		return false
	}
	token := r.FormValue("csrf_token")
	return token != "" && token == sess.CSRFToken
}
