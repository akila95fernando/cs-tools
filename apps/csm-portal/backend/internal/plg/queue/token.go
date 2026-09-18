package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/config"
)

// expiryMargin is how early a cached token is treated as spent.
//
// Without it a token that expires in half a second is handed to a request that
// takes longer than that, and the queue answers 401 for no good reason. A minute
// also absorbs modest clock skew between here and the authorisation server.
const expiryMargin = time.Minute

// fallbackLifetime is used when the server omits expires_in.
//
// The specification makes that field optional, and "no expiry stated" must not
// become "cache forever" — a revoked token would then be retried until restart.
const fallbackLifetime = 5 * time.Minute

// maxTokenResponseBytes bounds the token response.
const maxTokenResponseBytes = 1 << 20

// tokenSource fetches and caches an OAuth2 client-credentials token.
//
// Hand-rolled rather than pulled from a library, for two reasons: the
// client-credentials grant is one form POST and a JSON field, and the piece that
// actually matters here — refetching when the *queue* rejects a token the client
// still believes in — is not something a token library can do, because it never
// sees that response. Keeping both halves in one place makes the whole behaviour
// readable.
type tokenSource struct {
	tokenURL string
	clientID string
	secret   string
	scope    string
	http     *http.Client

	// The poller is a single goroutine today, but a token cache that is only
	// safe by accident is a trap for whoever adds the second caller.
	mu      sync.Mutex
	token   string
	expires time.Time
}

// newTokenSource builds a token source, or nil when the grant is not configured.
func newTokenSource(cfg config.OAuth2Config, timeout time.Duration) *tokenSource {
	if !cfg.Enabled() {
		return nil
	}
	return &tokenSource{
		tokenURL: cfg.TokenURL,
		clientID: cfg.ClientID,
		secret:   cfg.ClientSecret,
		scope:    cfg.Scope,
		http:     &http.Client{Timeout: timeout},
	}
}

// Token returns a cached token, fetching a new one when there is none or the one
// held is within expiryMargin of expiring.
func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.token != "" && time.Now().Add(expiryMargin).Before(t.expires) {
		return t.token, nil
	}
	return t.fetchLocked(ctx)
}

// Invalidate drops the cached token, so the next Token call fetches a fresh one.
//
// Called when the queue rejects a token the cache still considered valid — which
// happens after a revocation, a key rotation, or a clock further out than the
// margin allows.
func (t *tokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
	t.expires = time.Time{}
}

// fetchLocked performs the client-credentials exchange. The caller holds mu.
func (t *tokenSource) fetchLocked(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if t.scope != "" {
		form.Set("scope", t.scope)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// Credentials in the Authorization header rather than the body: RFC 6749
	// §2.3.1 prefers it, and it keeps them out of anything that logs form data.
	req.SetBasicAuth(t.clientID, t.secret)

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("call token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is quoted because an authorisation server explains itself
		// there — "invalid_client" versus "invalid_scope" are different fixes.
		return "", fmt.Errorf("token endpoint returned %d: %s",
			resp.StatusCode, truncate(strings.TrimSpace(string(body)), 200))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access_token")
	}
	// Anything other than Bearer would need a different Authorization scheme,
	// and guessing would produce a confusing 401 later instead of a clear error
	// now. An empty token_type is treated as Bearer, which is what servers that
	// omit it mean.
	if out.TokenType != "" && !strings.EqualFold(out.TokenType, "Bearer") {
		return "", fmt.Errorf("token endpoint returned token_type %q, which this portal cannot use",
			out.TokenType)
	}

	lifetime := fallbackLifetime
	if out.ExpiresIn > 0 {
		lifetime = time.Duration(out.ExpiresIn) * time.Second
	}
	t.token = out.AccessToken
	t.expires = time.Now().Add(lifetime)
	return t.token, nil
}
