// Package config loads runtime configuration for the PLG CS portal backend.
//
// Configuration comes from a JSON file (default ./config.json, override with
// PLG_CONFIG_FILE or the -config flag). Every value can also be overridden by
// an environment variable, which is what a container deployment will use — the
// file is for local development.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// ServerConfig describes how the HTTP server is exposed.
type ServerConfig struct {
	Port int `json:"port"`
	// AllowedOrigins is the CORS allow-list for the local webapp dev server.
	// Empty disables CORS handling entirely.
	AllowedOrigins []string `json:"allowedOrigins"`
	// RequestTimeoutSeconds bounds every request. Zero means 15.
	RequestTimeoutSeconds int `json:"requestTimeoutSeconds"`
}

// IngestConfig covers how registrations reach the portal.
type IngestConfig struct {
	// SharedSecret is compared against the X-PLG-Webhook-Secret header on every
	// inbound registration call. Empty disables the check, which is for the
	// local demo only — a deployment without it accepts anyone's payload.
	SharedSecret string `json:"sharedSecret"`

	// SourceMapPath points at the file describing how the upstream source words
	// things: which key holds each of the portal's fields, what it calls each
	// platform, and which extra fields are worth storing.
	//
	// Its own file, so a source change is a one-line edit that goes nowhere near
	// database credentials — and so changing source needs no rebuild.
	SourceMapPath string `json:"sourceMapPath"`
}

// QueueConfig describes the webhook-queue service the portal polls for
// registration events.
//
// The source publishes to that service; the portal is a consumer, not a
// listener. Everything here is configurable because the queue moves between
// environments — a local process, then a Choreo-managed endpoint.
type QueueConfig struct {
	// Enabled starts the poller. False leaves the portal running with no queue
	// consumer at all, which is what the demo and the tests want.
	Enabled bool `json:"enabled"`

	// ConsumeURL is the queue's consume endpoint, including the base path —
	// e.g. https://queue.example.com/api/v1/queue/consume
	ConsumeURL string `json:"consumeUrl"`

	// PollIntervalSeconds is the gap between polls when the queue is empty.
	PollIntervalSeconds int `json:"pollIntervalSeconds"`

	// BatchSize is the `count` asked for on each call. The queue caps this at
	// its own MAX_CONSUME_BATCH, so asking for more than that quietly gets less.
	BatchSize int `json:"batchSize"`

	// LongPollSeconds is the `wait` parameter: the queue holds the request open
	// until an event arrives, and answers the instant one does.
	//
	// This, not PollIntervalSeconds, is what decides how quickly a registration
	// is noticed. The poller is only listening while a long poll is open, so the
	// fraction of time it is listening is roughly
	// LongPollSeconds / (LongPollSeconds + PollIntervalSeconds) — which is why
	// the defaults are a long wait and a short interval. The queue enforces its
	// own upper bound (30s by default).
	LongPollSeconds int `json:"longPollSeconds"`

	// DrainMaxBatches bounds one tick. When the queue reports events still
	// waiting, the poller keeps consuming rather than taking one batch per
	// interval — otherwise a backlog of 500 would take 10 intervals to clear.
	DrainMaxBatches int `json:"drainMaxBatches"`

	// RequestTimeoutSeconds bounds one HTTP call. It must exceed
	// LongPollSeconds, or every long poll times out client-side.
	RequestTimeoutSeconds int `json:"requestTimeoutSeconds"`

	// AuthHeader and AuthToken send a fixed header on every consume — an API key,
	// or a long-lived token. Empty means no header is sent. For a queue behind
	// OAuth2 use OAuth below instead: a static token cannot be refreshed.
	AuthHeader string `json:"authHeader"`
	AuthToken  string `json:"authToken"`

	// OAuth turns on the client-credentials grant. The portal fetches a bearer
	// token from the authorisation server, caches it until shortly before it
	// expires, and refetches when the queue rejects it.
	OAuth OAuth2Config `json:"oauth2"`

	// EventTypes optionally restricts which event types are treated as
	// registrations. Empty accepts every event.
	//
	// This matters because consuming deletes: if the queue also carries events
	// this portal does not handle, they still arrive here and are still removed
	// from the queue. Anything not accepted is written to plg_ingest_failure
	// rather than dropped, so a shared queue does not lose other consumers' work
	// silently.
	EventTypes []string `json:"eventTypes"`
}

// OAuth2Config describes the client-credentials grant the portal uses to reach a
// protected queue.
//
// Client credentials rather than any interactive flow, because the portal acts
// as itself: there is no user behind a background poll and nothing to consent
// to.
type OAuth2Config struct {
	// TokenURL is the authorisation server's token endpoint.
	TokenURL string `json:"tokenUrl"`
	// ClientID and ClientSecret identify the portal. Sent as HTTP Basic
	// credentials, which is what RFC 6749 prefers and what Choreo's STS accepts.
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	// Scope is optional and passed through verbatim when set.
	Scope string `json:"scope"`
}

// Enabled reports whether the client-credentials grant is configured at all.
// Any one field being present counts, so a half-filled block is caught by
// validate rather than silently ignored.
func (o OAuth2Config) Enabled() bool {
	return o.TokenURL != "" || o.ClientID != "" || o.ClientSecret != ""
}

// validate refuses a half-configured grant, and refuses to put a client secret
// on the wire in plaintext.
func (o OAuth2Config) validate() error { return o.validateNamed("queue.oauth2") }

// validateNamed is validate with the configuration block's name in the message,
// so a failure says which grant is wrong when there is more than one.
func (o OAuth2Config) validateNamed(what string) error {
	if !o.Enabled() {
		return nil
	}

	var missing []string
	if o.TokenURL == "" {
		missing = append(missing, "tokenUrl")
	}
	if o.ClientID == "" {
		missing = append(missing, "clientId")
	}
	if o.ClientSecret == "" {
		missing = append(missing, "clientSecret")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s is missing %s — a client-credentials grant needs all three",
			what, strings.Join(missing, " and "))
	}

	u, err := url.Parse(o.TokenURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s.tokenUrl %q must be an absolute http(s) URL", what, o.TokenURL)
	}
	// A client secret over plaintext HTTP is readable by anything on the path.
	// Permitted against loopback, because that is how this gets tested.
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("%s.tokenUrl %q is plaintext http to a remote host — "+
			"the client secret would travel in the clear", what, o.TokenURL)
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Config is the fully resolved runtime configuration.
type Config struct {
	// Entity is where the data lives. The BFF has no database of its own — it
	// reaches entity-service over HTTP, so this is a base URL.
	Entity  EntityConfig  `json:"entity"`
	Server  ServerConfig  `json:"server"`
	Ingest  IngestConfig  `json:"ingest"`
	Queue   QueueConfig   `json:"queue"`
	Logging LoggingConfig `json:"logging"`
}

// EntityConfig points the BFF at entity-service.
//
// No credentials. entity-service has no authentication — its security is
// network placement — so there is nothing to configure. When the slice merges
// and sits behind the Choreo gateway this grows a client-credentials block, and
// nothing else here changes.
type EntityConfig struct {
	// BaseURL is entity-service's root, e.g. http://localhost:8200.
	//
	// Left empty, it is filled from what csm-portal already knows — see
	// EntityDefaults. PLG and csm-portal talk to the same entity-service, so
	// this only needs setting when they must differ.
	BaseURL string `json:"baseUrl"`

	// OAuth is the client-credentials grant used to reach entity-service.
	//
	// entity-service sits behind a gateway that requires a token, and PLG
	// authenticates as the same OAuth2 application as every other upstream
	// client in this backend. Normally inherited from csm-portal rather than
	// configured here — see EntityDefaults.
	OAuth OAuth2Config `json:"oauth2"`
	// TimeoutSeconds bounds one call. Defaults to 20.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// LoggingConfig is the one place logging is configured.
//
// Both fields exist because a deployment wants something different from a
// laptop. Locally, text at DEBUG is readable and greppable; in a platform that
// collects stdout into a searchable store, JSON at INFO means every line's
// correlation id, user and status are queryable fields rather than substrings
// somebody has to write a regex for.
type LoggingConfig struct {
	// Level is DEBUG, INFO, WARN or ERROR. Anything below it is dropped before
	// it is formatted, so a silenced DEBUG line costs almost nothing.
	Level string `json:"level"`

	// Format is "text" or "json".
	Format string `json:"format"`
}

// Load reads the JSON config file at path (a missing file is not an error —
// environment variables alone can supply everything), applies environment
// overrides, fills in defaults, and validates the result.
// EntityDefaults is what csm-portal already knows about entity-service, handed
// to PLG so it does not keep a second copy of settings describing one service.
//
// PLG's own PLG_* settings win where they are set; anything left empty is
// filled from here. In practice nothing needs setting: both halves of this
// process talk to the same entity-service as the same OAuth2 application.
type EntityDefaults struct {
	BaseURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scope        string
}

// Load reads the configuration with no inherited defaults. Used by tests; the
// server calls LoadWith so PLG inherits csm-portal's entity-service settings.
func Load(path string) (*Config, error) { return LoadWith(path, EntityDefaults{}) }

// LoadWith reads the configuration, filling anything PLG did not set from d.
func LoadWith(path string, d EntityDefaults) (*Config, error) {
	cfg := defaults()

	if path == "" {
		path = envOr("PLG_CONFIG_FILE", "config.json")
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied config path
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config file %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// Fall through: environment variables must supply the required values.
	default:
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	applyEnvOverrides(cfg)
	applyEntityDefaults(cfg, d)
	applyDefaults(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Entity: EntityConfig{TimeoutSeconds: 20},
		Server: ServerConfig{Port: 8100},
	}
}

// applyEntityDefaults inherits csm-portal's entity-service settings for every
// field PLG left empty. Called after the environment, so an explicit PLG_* value
// always wins.
func applyEntityDefaults(c *Config, d EntityDefaults) {
	if c.Entity.BaseURL == "" {
		c.Entity.BaseURL = d.BaseURL
	}
	if c.Entity.OAuth.TokenURL == "" {
		c.Entity.OAuth.TokenURL = d.TokenURL
	}
	if c.Entity.OAuth.ClientID == "" {
		c.Entity.OAuth.ClientID = d.ClientID
	}
	if c.Entity.OAuth.ClientSecret == "" {
		c.Entity.OAuth.ClientSecret = d.ClientSecret
	}
	if c.Entity.OAuth.Scope == "" {
		c.Entity.OAuth.Scope = d.Scope
	}
}

func applyDefaults(c *Config) {
	if c.Entity.TimeoutSeconds == 0 {
		c.Entity.TimeoutSeconds = 20
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8090
	}
	if c.Server.RequestTimeoutSeconds == 0 {
		c.Server.RequestTimeoutSeconds = 15
	}
	if c.Logging.Level == "" {
		// INFO, not ERROR: the access log and the poller's per-batch summary are
		// INFO, and they are the two things anyone actually wants when asking
		// what the portal has been doing.
		c.Logging.Level = "INFO"
	}
	if c.Logging.Format == "" {
		// Text by default so a laptop's terminal stays readable. Deployments set
		// json, which is what makes the fields queryable.
		c.Logging.Format = "text"
	}

	if c.Queue.PollIntervalSeconds == 0 {
		// Short, because longPollSeconds does the waiting. The queue holds each
		// request open, so this is only the gap between one long poll and the
		// next — a long interval here just means time spent not listening.
		c.Queue.PollIntervalSeconds = 2
	}
	if c.Queue.BatchSize == 0 {
		c.Queue.BatchSize = 50
	}
	if c.Queue.DrainMaxBatches == 0 {
		c.Queue.DrainMaxBatches = 20
	}
	if c.Queue.RequestTimeoutSeconds == 0 {
		// Comfortably past the long poll, so the queue decides when to answer
		// rather than the client giving up first.
		c.Queue.RequestTimeoutSeconds = c.Queue.LongPollSeconds + 30
	}
}

func applyEnvOverrides(c *Config) {
	setString(&c.Entity.BaseURL, "PLG_ENTITY_BASE_URL")
	// Overrides, for the unusual case where PLG must reach entity-service as a
	// different application than the rest of this backend. Normally unset.
	setString(&c.Entity.OAuth.TokenURL, "PLG_ENTITY_OAUTH_TOKEN_URL")
	setString(&c.Entity.OAuth.ClientID, "PLG_ENTITY_OAUTH_CLIENT_ID")
	setString(&c.Entity.OAuth.ClientSecret, "PLG_ENTITY_OAUTH_CLIENT_SECRET")
	setString(&c.Entity.OAuth.Scope, "PLG_ENTITY_SCOPES")
	setInt(&c.Entity.TimeoutSeconds, "PLG_ENTITY_TIMEOUT_SECONDS")
	setInt(&c.Server.Port, "PLG_SERVER_PORT")
	// The only config.json key that had no environment override. It mattered
	// because Choreo deploys from environment variables alone: the value was
	// reachable in a local config file and unreachable in the place it would
	// actually need tuning.
	setInt(&c.Server.RequestTimeoutSeconds, "PLG_SERVER_REQUEST_TIMEOUT_SECONDS")
	setString(&c.Ingest.SharedSecret, "PLG_INGEST_SHARED_SECRET")
	setString(&c.Ingest.SourceMapPath, "PLG_SOURCE_MAP_PATH")
	setString(&c.Queue.ConsumeURL, "PLG_QUEUE_CONSUME_URL")
	setInt(&c.Queue.PollIntervalSeconds, "PLG_QUEUE_POLL_INTERVAL_SECONDS")
	setInt(&c.Queue.BatchSize, "PLG_QUEUE_BATCH_SIZE")
	setInt(&c.Queue.LongPollSeconds, "PLG_QUEUE_LONG_POLL_SECONDS")
	setInt(&c.Queue.DrainMaxBatches, "PLG_QUEUE_DRAIN_MAX_BATCHES")
	setInt(&c.Queue.RequestTimeoutSeconds, "PLG_QUEUE_REQUEST_TIMEOUT_SECONDS")
	setString(&c.Queue.OAuth.TokenURL, "PLG_QUEUE_OAUTH_TOKEN_URL")
	setString(&c.Queue.OAuth.ClientID, "PLG_QUEUE_OAUTH_CLIENT_ID")
	setString(&c.Queue.OAuth.ClientSecret, "PLG_QUEUE_OAUTH_CLIENT_SECRET")
	setString(&c.Queue.OAuth.Scope, "PLG_QUEUE_OAUTH_SCOPE")
	setString(&c.Queue.AuthHeader, "PLG_QUEUE_AUTH_HEADER")
	setString(&c.Queue.AuthToken, "PLG_QUEUE_AUTH_TOKEN")

	if v := os.Getenv("PLG_QUEUE_ENABLED"); v != "" {
		c.Queue.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("PLG_QUEUE_EVENT_TYPES"); v != "" {
		c.Queue.EventTypes = splitList(v)
	}

	if v := os.Getenv("PLG_ALLOWED_ORIGINS"); v != "" {
		c.Server.AllowedOrigins = splitList(v)
	}
	setString(&c.Logging.Level, "PLG_LOG_LEVEL")
	setString(&c.Logging.Format, "PLG_LOG_FORMAT")

}

// splitList parses a comma-separated environment variable.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// Validate reports configuration that would fail at runtime in a confusing way.
func (c *Config) Validate() error {
	// The one setting a deployment cannot be without. A BFF that cannot reach
	// entity-service has no data at all, so this fails at startup rather than on
	// the first request.
	if strings.TrimSpace(c.Entity.BaseURL) == "" {
		return errors.New("entity.baseUrl is required — normally inherited from " +
			"CUSTOMER_ENTITY_BASE_URL, or set PLG_ENTITY_BASE_URL to override it")
	}
	if err := c.Entity.OAuth.validateNamed("entity.oauth2"); err != nil {
		return err
	}
	if !strings.HasPrefix(c.Entity.BaseURL, "http://") && !strings.HasPrefix(c.Entity.BaseURL, "https://") {
		return fmt.Errorf("entity.baseUrl %q must be an absolute http(s) URL", c.Entity.BaseURL)
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is out of range", c.Server.Port)
	}

	// Refused at startup rather than silently defaulted: a typo like "WARNING"
	// or "pretty" would otherwise leave the portal logging at a level nobody
	// chose, and the mistake only shows up when a log someone needed is absent.
	switch strings.ToUpper(c.Logging.Level) {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return fmt.Errorf("logging.level %q must be DEBUG, INFO, WARN or ERROR", c.Logging.Level)
	}
	switch strings.ToLower(c.Logging.Format) {
	case "text", "json":
	default:
		return fmt.Errorf("logging.format %q must be \"text\" or \"json\"", c.Logging.Format)
	}

	if c.Queue.Enabled {
		if c.Queue.ConsumeURL == "" {
			return errors.New("queue.consumeUrl is required when the queue poller is enabled " +
				"(config file `queue.consumeUrl` or PLG_QUEUE_CONSUME_URL)")
		}
		u, err := url.Parse(c.Queue.ConsumeURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("queue.consumeUrl %q must be an absolute http(s) URL", c.Queue.ConsumeURL)
		}
		if c.Queue.PollIntervalSeconds < 1 {
			return fmt.Errorf("queue.pollIntervalSeconds %d must be at least 1", c.Queue.PollIntervalSeconds)
		}
		if c.Queue.BatchSize < 1 {
			return fmt.Errorf("queue.batchSize %d must be at least 1", c.Queue.BatchSize)
		}
		// A client that gives up before the queue answers turns every long poll
		// into a timeout, and the backlog never clears.
		if c.Queue.RequestTimeoutSeconds <= c.Queue.LongPollSeconds {
			return fmt.Errorf(
				"queue.requestTimeoutSeconds (%d) must exceed longPollSeconds (%d)",
				c.Queue.RequestTimeoutSeconds, c.Queue.LongPollSeconds)
		}
		if c.Queue.AuthToken != "" && c.Queue.AuthHeader == "" {
			return errors.New("queue.authToken is set but authHeader is empty — " +
				"name the header the token belongs in, e.g. \"Authorization\"")
		}
		if err := c.Queue.OAuth.validate(); err != nil {
			return err
		}
		// Both would write Authorization, and one would silently win.
		if c.Queue.OAuth.Enabled() && strings.EqualFold(c.Queue.AuthHeader, "Authorization") {
			return errors.New("queue.oauth2 and queue.authHeader \"Authorization\" both set the " +
				"same header — use one or the other")
		}
	}
	return nil
}

// Redacted returns a copy safe to log.
//
// The BFF has no database, so the only secret here is the ingest shared
// secret.
func (c *Config) Redacted() Config {
	cp := *c
	if cp.Ingest.SharedSecret != "" {
		cp.Ingest.SharedSecret = "********"
	}
	if cp.Entity.OAuth.ClientSecret != "" {
		cp.Entity.OAuth.ClientSecret = "********"
	}
	if cp.Queue.AuthToken != "" {
		cp.Queue.AuthToken = "********"
	}
	if cp.Queue.OAuth.ClientSecret != "" {
		cp.Queue.OAuth.ClientSecret = "********"
	}
	return cp
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func setString(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func setInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}
