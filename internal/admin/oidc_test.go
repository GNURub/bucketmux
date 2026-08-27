package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gnurub/bucketmux/internal/config"
	secretcrypto "github.com/gnurub/bucketmux/internal/crypto"
)

func TestOIDCLoginUsesDiscoveryStateNonceAndPKCE(t *testing.T) {
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer server.Close()
	issuer = server.URL
	secrets, err := secretcrypto.NewSecretBox("oidc-test-master")
	if err != nil {
		t.Fatal(err)
	}
	auth := newOIDCAuth(config.OIDCConfig{Enabled: true, IssuerURL: issuer, ClientID: "bucketmux", ClientSecret: "secret", RedirectURL: "https://bucketmux.example/admin/oidc/callback", Scopes: []string{"openid", "email"}, SessionHours: 8}, secrets)
	request := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	response := httptest.NewRecorder()
	auth.login(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body.String())
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state", "nonce", "code_challenge"} {
		if location.Query().Get(name) == "" {
			t.Fatalf("OIDC redirect missing %s: %s", name, location.String())
		}
	}
	if location.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method=%q", location.Query().Get("code_challenge_method"))
	}
	if len(response.Result().Cookies()) != 1 || response.Result().Cookies()[0].Name != oidcFlowCookie || !response.Result().Cookies()[0].HttpOnly {
		t.Fatalf("flow cookies=%+v", response.Result().Cookies())
	}
}

func TestOIDCEncryptedSessionAndGroupAuthorization(t *testing.T) {
	secrets, _ := secretcrypto.NewSecretBox("oidc-session-master")
	auth := newOIDCAuth(config.OIDCConfig{Enabled: true, AllowedGroups: []string{"storage-admins"}, SessionHours: 8}, secrets)
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	response := httptest.NewRecorder()
	if err := auth.setEncryptedCookie(response, request, oidcSessionCookie, oidcSession{Subject: "user-1", Email: "admin@example.com", Groups: []string{"storage-admins"}, Expires: time.Now().UTC().Add(time.Hour)}, "/admin", time.Hour); err != nil {
		t.Fatal(err)
	}
	cookie := response.Result().Cookies()[0]
	if strings.Contains(cookie.Value, "admin@example.com") {
		t.Fatal("session cookie exposes plaintext claims")
	}
	authorizedRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	authorizedRequest.AddCookie(cookie)
	if !auth.authorized(authorizedRequest) {
		t.Fatal("expected encrypted OIDC session to authorize")
	}

	deniedResponse := httptest.NewRecorder()
	_ = auth.setEncryptedCookie(deniedResponse, request, oidcSessionCookie, oidcSession{Subject: "user-2", Groups: []string{"developers"}, Expires: time.Now().UTC().Add(time.Hour)}, "/admin", time.Hour)
	deniedRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	deniedRequest.AddCookie(deniedResponse.Result().Cookies()[0])
	if auth.authorized(deniedRequest) {
		t.Fatal("unexpected authorization for disallowed OIDC group")
	}
}
