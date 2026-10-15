// okta-restore-web is a small web server that lets suspended Tailscale users
// restore their own access by authenticating through Okta (OIDC/OAuth2).
//
// Flow:
//  1. User visits the page and clicks "Sign in with Okta"
//  2. Okta handles authentication, including any MFA your policy enforces
//  3. On success, Okta returns a verified email address via the userinfo endpoint
//  4. The server matches that email against suspended Tailscale accounts
//  5. If suspended, the account is restored and the user sees a confirmation
//
// Identity guarantee: the email comes directly from Okta after a successful
// authentication challenge — the user never types or supplies their own email.
//
// Environment variables:
//
//	TAILSCALE_API_KEY    Tailscale API key with users read+write
//	TAILSCALE_TAILNET    tailnet domain or "-" (default: "-")
//	OKTA_DOMAIN          your Okta domain, e.g. yourcompany.okta.com
//	OKTA_CLIENT_ID       OIDC application client ID
//	OKTA_CLIENT_SECRET   OIDC application client secret
//	OKTA_REDIRECT_URI    full callback URL, e.g. https://restore.yourcompany.com/auth/callback
//	SESSION_SECRET       random string ≥32 chars for signing state tokens
//	PORT                 HTTP listen port (default: 8080)
package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"golang.org/x/oauth2"

	"github.com/tailscale/seat-manager/internal/tsapi"
)

const stateCookieName = "ts_restore_state"

func main() {
	oktaDomain := mustEnv("OKTA_DOMAIN")

	s := &server{
		ts: tsapi.New(mustEnv("TAILSCALE_API_KEY"), envOr("TAILSCALE_TAILNET", "-")),
		oauth: &oauth2.Config{
			ClientID:     mustEnv("OKTA_CLIENT_ID"),
			ClientSecret: mustEnv("OKTA_CLIENT_SECRET"),
			RedirectURL:  mustEnv("OKTA_REDIRECT_URI"),
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint: oauth2.Endpoint{
				AuthURL:  fmt.Sprintf("https://%s/oauth2/default/v1/authorize", oktaDomain),
				TokenURL: fmt.Sprintf("https://%s/oauth2/default/v1/token", oktaDomain),
			},
		},
		userInfoURL:   fmt.Sprintf("https://%s/oauth2/default/v1/userinfo", oktaDomain),
		sessionSecret: []byte(mustEnv("SESSION_SECRET")),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)

	port := envOr("PORT", "8080")
	log.Printf("okta-restore-web listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, securityHeaders(mux)))
}

type server struct {
	ts            *tsapi.Client
	oauth         *oauth2.Config
	userInfoURL   string
	sessionSecret []byte
}

// handleIndex renders the landing page with a Sign in with Okta button.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	renderPage(w, http.StatusOK, indexTmpl, nil)
}

// handleLogin generates a CSRF state token, stores it in a signed cookie,
// and redirects the user to Okta for authentication.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err := s.newSignedState()
	if err != nil {
		log.Printf("generateState: %v", err)
		renderError(w, "Internal error. Please try again.")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/",
		MaxAge:   300, // 5 minutes — long enough to complete the Okta flow
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})

	http.Redirect(w, r, s.oauth.AuthCodeURL(state), http.StatusFound)
}

// handleCallback receives the Okta redirect, validates the state, exchanges
// the code for tokens, fetches the user's email, and restores them if suspended.
func (s *server) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Validate CSRF state — the cookie and the query param must match and the
	// cookie value must have been signed by us.
	cookie, err := r.Cookie(stateCookieName)
	if err != nil {
		renderError(w, "Session expired or missing. Please start over.")
		return
	}
	if !s.validState(r.FormValue("state"), cookie.Value) {
		renderError(w, "Invalid session state. Please start over.")
		return
	}

	// Clear the state cookie immediately — it's single-use.
	http.SetCookie(w, &http.Cookie{
		Name:   stateCookieName,
		Path:   "/",
		MaxAge: -1,
	})

	// Exchange the authorisation code for tokens.
	token, err := s.oauth.Exchange(ctx, r.FormValue("code"))
	if err != nil {
		log.Printf("oauth exchange: %v", err)
		renderError(w, "Okta authentication failed. Please try again.")
		return
	}

	// Fetch the verified email from Okta's userinfo endpoint.
	// The email comes from Okta — the user never supplies it themselves.
	email, err := s.oktaEmail(ctx, token.AccessToken)
	if err != nil {
		log.Printf("okta userinfo: %v", err)
		renderError(w, "Could not retrieve your email from Okta. Please contact your IT admin.")
		return
	}

	// Look up the Tailscale account for this email.
	tsUser, err := s.findUser(ctx, email)
	if err != nil {
		log.Printf("tailscale listUsers: %v", err)
		renderError(w, "Could not reach the Tailscale API. Please try again later.")
		return
	}

	safeEmail := template.HTMLEscapeString(email)

	if tsUser == nil {
		renderResult(w, resultData{
			Icon:    "❓",
			Heading: "No Tailscale account found",
			Body:    template.HTML(fmt.Sprintf("There is no Tailscale account for <strong>%s</strong>. If you believe this is an error, contact your IT admin.", safeEmail)),
		})
		return
	}

	switch tsUser.Status {
	case "suspended":
		if err := s.ts.RestoreUser(ctx, tsUser.ID); err != nil {
			log.Printf("RestoreUser(%s): %v", email, err)
			renderResult(w, resultData{
				Icon:    "❌",
				Heading: "Restore failed",
				Body:    "We were unable to restore your account. Please contact your IT admin.",
			})
			return
		}
		log.Printf("restored %s (%s) via Okta self-service", email, tsUser.ID)
		renderResult(w, resultData{
			Icon:    "✅",
			Heading: "Access restored",
			Body:    template.HTML(fmt.Sprintf("Your Tailscale account (<strong>%s</strong>) is active again. Open the Tailscale app and reconnect.", safeEmail)),
		})

	case "active", "idle":
		renderResult(w, resultData{
			Icon:    "🟢",
			Heading: "Your account is already active",
			Body:    template.HTML(fmt.Sprintf("Your Tailscale account (<strong>%s</strong>) is not suspended. If you're having trouble connecting, try restarting the Tailscale client.", safeEmail)),
		})

	default:
		renderResult(w, resultData{
			Icon:    "⚠️",
			Heading: "Account status: " + tsUser.Status,
			Body:    "Please contact your IT admin for assistance.",
		})
	}
}

// oktaEmail calls the Okta userinfo endpoint and returns the verified email.
func (s *server) oktaEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.userInfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("userinfo HTTP %d: %s", resp.StatusCode, body)
	}

	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode userinfo: %w", err)
	}
	if info.Email == "" {
		return "", fmt.Errorf("empty email in Okta userinfo response")
	}
	return info.Email, nil
}

// findUser returns the Tailscale user whose loginName matches email (case-insensitive).
// Returns nil, nil when no match is found.
func (s *server) findUser(ctx context.Context, email string) (*tsapi.User, error) {
	users, err := s.ts.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if strings.EqualFold(users[i].LoginName, email) {
			return &users[i], nil
		}
	}
	return nil, nil
}

// newSignedState generates a random nonce and returns it HMAC-signed so we can
// verify it wasn't tampered with when it comes back from Okta.
func (s *server) newSignedState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	nonce := base64.URLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(nonce))
	sig := base64.URLEncoding.EncodeToString(mac.Sum(nil))
	return nonce + "." + sig, nil
}

// validState verifies both that the state returned by Okta matches the cookie
// (preventing CSRF) and that the HMAC signature is correct (preventing forgery).
func (s *server) validState(fromOkta, fromCookie string) bool {
	if fromOkta == "" || fromCookie == "" || fromOkta != fromCookie {
		return false
	}
	parts := strings.SplitN(fromOkta, ".", 2)
	if len(parts) != 2 {
		return false
	}
	nonce, gotSig := parts[0], parts[1]
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(nonce))
	wantSig := base64.URLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(gotSig), []byte(wantSig))
}

// securityHeaders adds baseline HTTP security headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

// ── Templates ────────────────────────────────────────────────────────────────

type resultData struct {
	Icon    string
	Heading string
	Body    template.HTML
}

func renderPage(w http.ResponseWriter, code int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

func renderResult(w http.ResponseWriter, data resultData) {
	renderPage(w, http.StatusOK, resultTmpl, data)
}

func renderError(w http.ResponseWriter, msg string) {
	renderResult(w, resultData{
		Icon:    "❌",
		Heading: "Something went wrong",
		Body:    template.HTML(template.HTMLEscapeString(msg)),
	})
}

var indexTmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Restore Tailscale Access</title>
  <style>` + commonCSS + `</style>
</head>
<body>
  <div class="card">
    <div class="logo">🔒</div>
    <h1>Restore Tailscale Access</h1>
    <p>
      Your Tailscale account may have been suspended after a period of
      inactivity. Sign in with your company Okta account to verify your
      identity and restore access instantly.
    </p>
    <p class="note">
      You will be redirected to your company's Okta login page.
      Multi-factor authentication may be required.
    </p>
    <a href="/auth/login" class="btn">Sign in with Okta</a>
  </div>
  <footer>` + footer + `</footer>
</body>
</html>`))

var resultTmpl = template.Must(template.New("result").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Restore Tailscale Access</title>
  <style>` + commonCSS + `</style>
</head>
<body>
  <div class="card">
    <div class="logo">{{.Icon}}</div>
    <h1>{{.Heading}}</h1>
    <p>{{.Body}}</p>
    <a href="/" class="btn secondary">Start over</a>
  </div>
  <footer>` + footer + `</footer>
</body>
</html>`))

const footer = `Powered by <a href="https://tailscale.com">Tailscale</a> &middot; ` +
	`<span id="year"></span><script>document.getElementById('year').textContent=new Date().getFullYear()</script>`

const commonCSS = `
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: #f5f5f7;
    min-height: 100vh;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    padding: 24px;
  }
  .card {
    background: #fff;
    border-radius: 16px;
    box-shadow: 0 4px 24px rgba(0,0,0,0.08);
    padding: 48px 40px;
    max-width: 480px;
    width: 100%;
    text-align: center;
  }
  .logo { font-size: 48px; margin-bottom: 16px; }
  h1 { font-size: 24px; font-weight: 600; color: #1d1d1f; margin-bottom: 16px; }
  p { color: #555; line-height: 1.6; margin-bottom: 16px; }
  p.note { font-size: 13px; color: #888; }
  .btn {
    display: inline-block;
    margin-top: 8px;
    padding: 12px 28px;
    background: #3b5bdb;
    color: #fff;
    border-radius: 8px;
    text-decoration: none;
    font-weight: 600;
    font-size: 15px;
    transition: background 0.15s;
  }
  .btn:hover { background: #2f4ac7; }
  .btn.secondary { background: #6c757d; margin-left: 8px; }
  .btn.secondary:hover { background: #5a6268; }
  footer { margin-top: 32px; font-size: 12px; color: #aaa; }
  footer a { color: #aaa; }
`

// ── Helpers ───────────────────────────────────────────────────────────────────

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

