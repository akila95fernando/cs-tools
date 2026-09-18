// Package queue consumes registration events from the webhook-queue service.
//
// The registration source publishes to that service; the portal polls it. The portal is a
// client here, not a listener — which is what lets the internet-facing surface
// live in one small service that does nothing but hold events.
package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/config"
)

// maxResponseBytes bounds one consume response, so a misconfigured URL pointing
// at something enormous cannot exhaust memory.
const maxResponseBytes = 16 << 20 // 16 MiB

// Event is one queued event, as the queue service returns it.
//
// Payload is the body the source published, kept raw: the portal resolves it
// registration only once it has decided the event is one.
type Event struct {
	ID         string            `json:"id"`
	EventType  string            `json:"event_type"`
	ReceivedAt time.Time         `json:"received_at"`
	Source     string            `json:"source"`
	Headers    map[string]string `json:"headers"`
	Payload    json.RawMessage   `json:"payload"`
}

// ConsumeResponse is the queue's reply. Remaining is how many events are still
// waiting, which is what lets the poller drain a backlog instead of taking one
// batch per interval.
type ConsumeResponse struct {
	Count     int     `json:"count"`
	Events    []Event `json:"events"`
	Remaining int     `json:"remaining"`
}

// Client calls the queue's consume endpoint.
//
// Consume is the only queue operation the portal uses. Peek, purge and health
// exist on the service but are deliberately not called: peeking would show
// events the portal is about to take anyway, and purging is destructive with no
// portal-side reason to reach for it.
type Client struct {
	url        string
	authHeader string
	authToken  string
	tokens     *tokenSource
	batchSize  int
	longPoll   time.Duration
	http       *http.Client
}

// NewClient builds a queue client from configuration.
func NewClient(cfg config.QueueConfig) *Client {
	timeout := time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	return &Client{
		url:        cfg.ConsumeURL,
		authHeader: cfg.AuthHeader,
		authToken:  cfg.AuthToken,
		// nil when the grant is not configured, which is what keeps an
		// unprotected local queue working with no settings at all.
		tokens:    newTokenSource(cfg.OAuth, timeout),
		batchSize: cfg.BatchSize,
		longPoll:  time.Duration(cfg.LongPollSeconds) * time.Second,
		http:      &http.Client{Timeout: timeout},
	}
}

// Consume removes up to batchSize events from the head of the queue.
//
// Consuming deletes: whatever this returns is no longer on the queue, and no
// second consumer will see it. That is the queue's contract, and it is why the
// caller must account for every event it receives.
//
// One retry, and only for a rejected token. A 401 can mean the cached token was
// revoked or rotated while the cache still believed in it, and the fix is to
// fetch a new one — but if the fresh token is rejected too, the credentials are
// wrong and retrying would just hammer the authorisation server.
func (c *Client) Consume(ctx context.Context) (*ConsumeResponse, error) {
	resp, status, err := c.consumeOnce(ctx)
	if err == nil {
		return resp, nil
	}
	if c.tokens == nil || (status != http.StatusUnauthorized && status != http.StatusForbidden) {
		return nil, err
	}

	c.tokens.Invalidate()
	resp, _, retryErr := c.consumeOnce(ctx)
	if retryErr != nil {
		return nil, fmt.Errorf("%w (a fresh token was rejected too)", retryErr)
	}
	return resp, nil
}

// consumeOnce makes one call, returning the HTTP status alongside the error so
// the caller can tell a rejected token from a queue that is simply down.
func (c *Client) consumeOnce(ctx context.Context) (*ConsumeResponse, int, error) {
	endpoint, err := url.Parse(c.url)
	if err != nil {
		return nil, 0, fmt.Errorf("parse consume url: %w", err)
	}
	q := endpoint.Query()
	q.Set("count", strconv.Itoa(c.batchSize))
	if c.longPoll > 0 {
		q.Set("wait", c.longPoll.String())
	}
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build consume request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.authHeader != "" && c.authToken != "" {
		req.Header.Set(c.authHeader, c.authToken)
	}
	if c.tokens != nil {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			// Reported as its own failure rather than folded into a queue error:
			// "the authorisation server said no" and "the queue said no" have
			// different fixes, and the poller logs whichever it is.
			return nil, 0, fmt.Errorf("obtain queue access token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("call queue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read queue response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("queue returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var out ConsumeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode queue response: %w", err)
	}
	return &out, resp.StatusCode, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
