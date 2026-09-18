package config

import (
	"os"
	"testing"
)

// Every config.json key must be reachable from an environment variable,
// because Choreo deploys from environment alone — a value that exists only in
// the JSON file is a value that cannot be changed where it matters.
//
// This is a table of the mapping rather than a test of one field: adding a
// config key without an override is the mistake it exists to catch, and that
// mistake already happened once with server.requestTimeoutSeconds.
func TestEveryConfigKeyHasAnEnvOverride(t *testing.T) {
	tests := []struct {
		env   string
		value string
		read  func(*Config) string
	}{
		{"PLG_ENTITY_BASE_URL", "http://entity.test:9000", func(c *Config) string { return c.Entity.BaseURL }},
		{"PLG_ENTITY_OAUTH_TOKEN_URL", "https://sts.test/token", func(c *Config) string { return c.Entity.OAuth.TokenURL }},
		{"PLG_ENTITY_OAUTH_CLIENT_ID", "cid", func(c *Config) string { return c.Entity.OAuth.ClientID }},
		{"PLG_ENTITY_OAUTH_CLIENT_SECRET", "csec", func(c *Config) string { return c.Entity.OAuth.ClientSecret }},
		{"PLG_ENTITY_SCOPES", "read", func(c *Config) string { return c.Entity.OAuth.Scope }},
		{"PLG_ENTITY_TIMEOUT_SECONDS", "31", func(c *Config) string { return itoa(c.Entity.TimeoutSeconds) }},
		{"PLG_SERVER_PORT", "9111", func(c *Config) string { return itoa(c.Server.Port) }},
		{"PLG_SERVER_REQUEST_TIMEOUT_SECONDS", "43", func(c *Config) string { return itoa(c.Server.RequestTimeoutSeconds) }},
		{"PLG_ALLOWED_ORIGINS", "https://a.test", func(c *Config) string { return c.Server.AllowedOrigins[0] }},
		{"PLG_INGEST_SHARED_SECRET", "s3cret", func(c *Config) string { return c.Ingest.SharedSecret }},
		{"PLG_SOURCE_MAP_PATH", "/mnt/config/source-map.json", func(c *Config) string { return c.Ingest.SourceMapPath }},
		{"PLG_QUEUE_CONSUME_URL", "http://q.test/consume", func(c *Config) string { return c.Queue.ConsumeURL }},
		{"PLG_QUEUE_POLL_INTERVAL_SECONDS", "11", func(c *Config) string { return itoa(c.Queue.PollIntervalSeconds) }},
		{"PLG_QUEUE_LONG_POLL_SECONDS", "12", func(c *Config) string { return itoa(c.Queue.LongPollSeconds) }},
		{"PLG_QUEUE_BATCH_SIZE", "13", func(c *Config) string { return itoa(c.Queue.BatchSize) }},
		{"PLG_QUEUE_DRAIN_MAX_BATCHES", "14", func(c *Config) string { return itoa(c.Queue.DrainMaxBatches) }},
		{"PLG_QUEUE_REQUEST_TIMEOUT_SECONDS", "77", func(c *Config) string { return itoa(c.Queue.RequestTimeoutSeconds) }},
		{"PLG_QUEUE_AUTH_HEADER", "X-Key", func(c *Config) string { return c.Queue.AuthHeader }},
		{"PLG_QUEUE_AUTH_TOKEN", "tok", func(c *Config) string { return c.Queue.AuthToken }},
		{"PLG_QUEUE_OAUTH_TOKEN_URL", "http://idp.test/token", func(c *Config) string { return c.Queue.OAuth.TokenURL }},
		{"PLG_QUEUE_OAUTH_CLIENT_ID", "cid", func(c *Config) string { return c.Queue.OAuth.ClientID }},
		{"PLG_QUEUE_OAUTH_CLIENT_SECRET", "csec", func(c *Config) string { return c.Queue.OAuth.ClientSecret }},
		{"PLG_QUEUE_OAUTH_SCOPE", "read", func(c *Config) string { return c.Queue.OAuth.Scope }},
		{"PLG_LOG_LEVEL", "WARN", func(c *Config) string { return c.Logging.Level }},
		{"PLG_LOG_FORMAT", "json", func(c *Config) string { return c.Logging.Format }},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			c := &Config{}
			applyEnvOverrides(c)
			if got := tc.read(c); got != tc.value {
				t.Errorf("%s=%q did not reach the config: got %q", tc.env, tc.value, got)
			}
		})
	}
}

// Booleans and lists take their own paths through applyEnvOverrides, so they
// are asserted separately rather than squeezed into the string table above.
func TestBooleanAndListOverrides(t *testing.T) {
	t.Run("PLG_QUEUE_ENABLED", func(t *testing.T) {
		for _, v := range []string{"true", "1"} {
			t.Setenv("PLG_QUEUE_ENABLED", v)
			c := &Config{}
			applyEnvOverrides(c)
			if !c.Queue.Enabled {
				t.Errorf("PLG_QUEUE_ENABLED=%q did not enable the queue", v)
			}
		}
		t.Setenv("PLG_QUEUE_ENABLED", "false")
		c := &Config{Queue: QueueConfig{Enabled: true}}
		applyEnvOverrides(c)
		if c.Queue.Enabled {
			t.Error(`PLG_QUEUE_ENABLED="false" did not disable the queue`)
		}
	})

	t.Run("PLG_QUEUE_EVENT_TYPES", func(t *testing.T) {
		t.Setenv("PLG_QUEUE_EVENT_TYPES", "a, b ,c")
		c := &Config{}
		applyEnvOverrides(c)
		if len(c.Queue.EventTypes) != 3 || c.Queue.EventTypes[1] != "b" {
			t.Errorf("list not split and trimmed: %#v", c.Queue.EventTypes)
		}
	})
}

// An unset variable must leave the file's value alone — otherwise every
// unspecified variable would silently zero a configured field.
func TestUnsetEnvLeavesTheFileValue(t *testing.T) {
	os.Unsetenv("PLG_SERVER_REQUEST_TIMEOUT_SECONDS")
	c := &Config{}
	c.Server.RequestTimeoutSeconds = 15
	applyEnvOverrides(c)
	if c.Server.RequestTimeoutSeconds != 15 {
		t.Errorf("an unset variable overwrote the configured value: %d", c.Server.RequestTimeoutSeconds)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
