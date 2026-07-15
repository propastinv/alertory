package auth

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/propastinv/alertory/internal/db"
)

const (
	sessionCookieName  = "alertory_session"
	stateCookieName    = "alertory_oauth_state"
	nonceCookieName    = "alertory_oauth_nonce"
	verifierCookieName = "alertory_oauth_verifier"
	returnToCookieName = "alertory_oauth_return_to"

	sessionDuration = 24 * time.Hour
	handshakeMaxAge = 10 * time.Minute
)

func (s *Service) setTransientCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(handshakeMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   s.secureCookies,
		// Lax (not Strict): these cookies must survive the top-level GET
		// redirect Keycloak sends the browser back to us with, which is a
		// cross-site navigation from the browser's point of view.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// LoginHandler starts the OIDC authorization code + PKCE flow: generates
// state/nonce/PKCE verifier, stashes them in short-lived cookies, and
// redirects to the identity provider.
func (s *Service) LoginHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, err := randomToken(32)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		nonce, err := randomToken(32)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		verifier := oauth2.GenerateVerifier()

		s.setTransientCookie(w, stateCookieName, state)
		s.setTransientCookie(w, nonceCookieName, nonce)
		s.setTransientCookie(w, verifierCookieName, verifier)
		s.setTransientCookie(w, returnToCookieName, safeReturnTo(r.URL.Query().Get("return_to")))

		authURL := s.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
		http.Redirect(w, r, authURL, http.StatusFound)
	})
}

// CallbackHandler completes the flow: validates state/nonce, exchanges the
// authorization code (with the PKCE verifier), verifies the ID token, and
// opens a server-side session.
func (s *Service) CallbackHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		stateCookie, stateErr := r.Cookie(stateCookieName)
		nonceCookie, nonceErr := r.Cookie(nonceCookieName)
		verifierCookie, verifierErr := r.Cookie(verifierCookieName)

		returnTo := "/"
		if c, err := r.Cookie(returnToCookieName); err == nil {
			returnTo = safeReturnTo(c.Value)
		}

		// Clear the handshake cookies unconditionally - they're single use
		// regardless of how this request turns out.
		s.clearCookie(w, stateCookieName)
		s.clearCookie(w, nonceCookieName)
		s.clearCookie(w, verifierCookieName)
		s.clearCookie(w, returnToCookieName)

		if stateErr != nil || nonceErr != nil || verifierErr != nil {
			http.Error(w, "login session expired, please try again", http.StatusBadRequest)
			return
		}
		if stateCookie.Value == "" || r.URL.Query().Get("state") != stateCookie.Value {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			http.Error(w, "login failed: "+errMsg, http.StatusUnauthorized)
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}

		token, err := s.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(verifierCookie.Value))
		if err != nil {
			log.Printf("auth: code exchange failed: %v", err)
			http.Error(w, "login failed", http.StatusUnauthorized)
			return
		}

		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok {
			http.Error(w, "no id_token in token response", http.StatusUnauthorized)
			return
		}

		idToken, err := s.verifier.Verify(ctx, rawIDToken)
		if err != nil {
			log.Printf("auth: id_token verification failed: %v", err)
			http.Error(w, "login failed", http.StatusUnauthorized)
			return
		}
		// Verify() deliberately does not check nonce - that's on us.
		if idToken.Nonce != nonceCookie.Value {
			http.Error(w, "nonce mismatch", http.StatusUnauthorized)
			return
		}

		var claims struct {
			Email             string `json:"email"`
			Name              string `json:"name"`
			PreferredUsername string `json:"preferred_username"`
		}
		if err := idToken.Claims(&claims); err != nil {
			log.Printf("auth: failed to decode ID token claims: %v", err)
		}
		name := claims.Name
		if name == "" {
			name = claims.PreferredUsername
		}

		sessionID, err := randomToken(32)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		csrfToken, err := randomToken(32)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		expiresAt := time.Now().Add(sessionDuration)
		if err := db.CreateWebSession(ctx, pool, sessionID, idToken.Subject, claims.Email, name, csrfToken, expiresAt); err != nil {
			log.Printf("auth: failed to create session: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sessionID,
			Path:     "/",
			Expires:  expiresAt,
			HttpOnly: true,
			Secure:   s.secureCookies,
			SameSite: http.SameSiteLaxMode,
		})

		http.Redirect(w, r, returnTo, http.StatusFound)
	})
}

// LogoutHandler ends the local session. It does not perform a full
// Keycloak SSO logout - a user could still be logged into other apps
// sharing the same Keycloak session.
func (s *Service) LogoutHandler(pool *pgxpool.Pool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			if err := db.DeleteWebSession(r.Context(), pool, c.Value); err != nil {
				log.Printf("auth: failed to delete session on logout: %v", err)
			}
		}
		s.clearCookie(w, sessionCookieName)
		http.Redirect(w, r, "/", http.StatusFound)
	})
}

// safeReturnTo only allows a same-site relative path, so return_to can't
// be abused as an open redirect (e.g. "//evil.example.com" or
// "https://evil.example.com").
func safeReturnTo(path string) string {
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "://") {
		return "/"
	}
	return path
}
