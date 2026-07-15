// Package auth implements login for the web UI via OpenID Connect
// (Authorization Code flow + PKCE) against an external identity provider -
// Keycloak in practice, but anything spec-compliant works since discovery
// is used instead of hardcoded endpoints. The Alertmanager webhook
// (/api/v1/alerts) is untouched by this package; it keeps its own bearer
// token check, since Alertmanager can't do a browser login.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Service wraps an OIDC provider/client for a single realm. Constructing
// it performs OIDC discovery (an HTTP round trip), so build it once at
// startup and reuse it - not per-request.
type Service struct {
	verifier      *oidc.IDTokenVerifier
	oauth2Config  oauth2.Config
	secureCookies bool
}

// New performs OIDC discovery against issuerURL (e.g.
// "https://keycloak.example.com/realms/alertory") and returns a Service
// ready to handle logins. redirectURL must exactly match a redirect URI
// registered on the Keycloak client.
func New(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL string) (*Service, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery against %s failed: %w", issuerURL, err)
	}

	return &Service{
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		oauth2Config: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		// Cookies (session + the short-lived login-handshake ones) are
		// only marked Secure when we know the app is actually served over
		// HTTPS - inferred from the redirect URL's scheme, since that's
		// the one piece of config guaranteed to reflect how the app is
		// really reached (vs. r.TLS, which is nil when TLS is terminated
		// by a reverse proxy in front of a plain-HTTP Go listener).
		secureCookies: strings.HasPrefix(redirectURL, "https://"),
	}, nil
}

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
