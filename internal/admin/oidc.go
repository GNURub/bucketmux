package admin

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gnurub/bucketmux/internal/config"
	secretcrypto "github.com/gnurub/bucketmux/internal/crypto"
	"golang.org/x/oauth2"
)

const oidcFlowCookie = "bucketmux_oidc_flow"
const oidcSessionCookie = "bucketmux_admin_session"

type oidcAuth struct {
	config   config.OIDCConfig
	secrets  *secretcrypto.SecretBox
	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

type oidcFlow struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Expires  time.Time `json:"expires"`
}

type oidcSession struct {
	Subject string    `json:"subject"`
	Email   string    `json:"email"`
	Name    string    `json:"name"`
	Groups  []string  `json:"groups"`
	Expires time.Time `json:"expires"`
}

func newOIDCAuth(cfg config.OIDCConfig, secrets *secretcrypto.SecretBox) *oidcAuth {
	return &oidcAuth{config: cfg, secrets: secrets}
}

func (a *oidcAuth) enabled() bool { return a != nil && a.config.Enabled }

func (a *oidcAuth) initialize(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil {
		return nil
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(discoveryCtx, a.config.IssuerURL)
	if err != nil {
		return fmt.Errorf("discover OIDC provider: %w", err)
	}
	a.provider = provider
	a.verifier = provider.Verifier(&oidc.Config{ClientID: a.config.ClientID})
	a.oauth = oauth2.Config{ClientID: a.config.ClientID, ClientSecret: a.config.ClientSecret, RedirectURL: a.config.RedirectURL, Endpoint: provider.Endpoint(), Scopes: a.config.Scopes}
	return nil
}

func (a *oidcAuth) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "OIDC login requires GET")
		return
	}
	if err := a.initialize(r.Context()); err != nil {
		writeProblem(w, http.StatusBadGateway, "oidc-discovery-failed", err.Error())
		return
	}
	flow := oidcFlow{State: secureRandom(24), Nonce: secureRandom(24), Verifier: oauth2.GenerateVerifier(), Expires: time.Now().UTC().Add(10 * time.Minute)}
	if err := a.setEncryptedCookie(w, r, oidcFlowCookie, flow, "/admin/oidc/callback", 10*time.Minute); err != nil {
		writeProblem(w, http.StatusInternalServerError, "oidc-state-failed", err.Error())
		return
	}
	redirect := a.oauth.AuthCodeURL(flow.State, oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier))
	http.Redirect(w, r, redirect, http.StatusFound)
}

func (a *oidcAuth) callback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "OIDC callback requires GET")
		return
	}
	if err := a.initialize(r.Context()); err != nil {
		writeProblem(w, http.StatusBadGateway, "oidc-discovery-failed", err.Error())
		return
	}
	var flow oidcFlow
	if err := a.readEncryptedCookie(r, oidcFlowCookie, &flow); err != nil || flow.Expires.Before(time.Now().UTC()) || flow.State != r.URL.Query().Get("state") {
		writeProblem(w, http.StatusForbidden, "oidc-state-invalid", "OIDC state is missing, expired or invalid")
		return
	}
	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		writeProblem(w, http.StatusForbidden, "oidc-exchange-failed", err.Error())
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		writeProblem(w, http.StatusForbidden, "oidc-token-missing", "OIDC provider did not return an ID token")
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil || idToken.Nonce != flow.Nonce {
		writeProblem(w, http.StatusForbidden, "oidc-token-invalid", "OIDC ID token verification failed")
		return
	}
	var claims struct {
		Subject string   `json:"sub"`
		Email   string   `json:"email"`
		Name    string   `json:"name"`
		Groups  []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Subject == "" {
		writeProblem(w, http.StatusForbidden, "oidc-claims-invalid", "OIDC subject claim is required")
		return
	}
	if !groupsAllowed(claims.Groups, a.config.AllowedGroups) {
		writeProblem(w, http.StatusForbidden, "oidc-group-denied", "OIDC identity does not belong to an allowed group")
		return
	}
	expires := time.Now().UTC().Add(time.Duration(a.config.SessionHours) * time.Hour)
	if idToken.Expiry.Before(expires) {
		expires = idToken.Expiry
	}
	session := oidcSession{Subject: claims.Subject, Email: claims.Email, Name: claims.Name, Groups: claims.Groups, Expires: expires}
	if err := a.setEncryptedCookie(w, r, oidcSessionCookie, session, "/admin", time.Until(expires)); err != nil {
		writeProblem(w, http.StatusInternalServerError, "oidc-session-failed", err.Error())
		return
	}
	a.clearCookie(w, r, oidcFlowCookie, "/admin/oidc/callback")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *oidcAuth) authorized(r *http.Request) bool {
	if !a.enabled() {
		return false
	}
	var session oidcSession
	return a.readEncryptedCookie(r, oidcSessionCookie, &session) == nil && session.Subject != "" && session.Expires.After(time.Now().UTC()) && groupsAllowed(session.Groups, a.config.AllowedGroups)
}

func (a *oidcAuth) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeProblem(w, http.StatusMethodNotAllowed, "method-not-allowed", "logout requires GET or POST")
		return
	}
	a.clearCookie(w, r, oidcSessionCookie, "/admin")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *oidcAuth) setEncryptedCookie(w http.ResponseWriter, r *http.Request, name string, value any, path string, duration time.Duration) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encrypted, err := a.secrets.Encrypt(string(encoded))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: encrypted, Path: path, MaxAge: int(duration.Seconds()), HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode})
	return nil
}

func (a *oidcAuth) readEncryptedCookie(r *http.Request, name string, target any) error {
	cookie, err := r.Cookie(name)
	if err != nil {
		return err
	}
	plain, err := a.secrets.Decrypt(cookie.Value)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(plain), target)
}

func (a *oidcAuth) clearCookie(w http.ResponseWriter, r *http.Request, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteLaxMode})
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func groupsAllowed(identityGroups, allowedGroups []string) bool {
	if len(allowedGroups) == 0 {
		return true
	}
	for _, group := range identityGroups {
		if slices.Contains(allowedGroups, group) {
			return true
		}
	}
	return false
}

func secureRandom(size int) string {
	buffer := make([]byte, size)
	_, _ = cryptorand.Read(buffer)
	return base64.RawURLEncoding.EncodeToString(buffer)
}
