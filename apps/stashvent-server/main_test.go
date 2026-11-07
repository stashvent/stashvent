package main

import (
	"fmt"
	"strings"
	"testing"
)

// setRequiredEnv sets the variables loadConfig refuses to start without, and
// clears the optional ones this file tests, whatever the developer's shell
// or .env holds.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DATABASE_URL":           "postgres://stashvent@localhost/stashvent",
		"S3_ENDPOINT":            "http://localhost:8333",
		"S3_PUBLIC_ENDPOINT":     "http://localhost:8333",
		"S3_BUCKET":              "stashvent",
		"S3_ACCESS_KEY":          "key",
		"S3_SECRET_KEY":          "secret",
		"LOG_LEVEL":              "",
		"MAX_PAYLOAD_BYTES":      "",
		"TRUSTED_PROXIES":        "",
		"REQUIRE_API_KEY":        "",
		"API_KEY_SHARES_PER_DAY": "",
		"MAX_STORED_BYTES":       "",
		"MAX_PULLS_PER_SHARE":    "",
	} {
		t.Setenv(key, value)
	}
	for _, class := range []string{"CREATE", "LOOKUP", "OTHER", "KEY_CREATE"} {
		t.Setenv("RATE_LIMIT_"+class+"_PER_MINUTE", "")
		t.Setenv("RATE_LIMIT_"+class+"_BURST", "")
	}
}

func TestLoadConfigRateLimits(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setRequiredEnv(t)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		want := rateLimitConfig{
			create:    rateLimit{perMinute: 10, burst: 5},
			lookup:    rateLimit{perMinute: 30, burst: 10},
			other:     rateLimit{perMinute: 60, burst: 20},
			keyCreate: rateLimit{perMinute: 10, burst: 5},
		}
		if cfg.limits != want {
			t.Errorf("limits = %+v, want %+v", cfg.limits, want)
		}
		// X-Forwarded-For is believed from nobody unless configured.
		if len(cfg.trustedProxies) != 0 {
			t.Errorf("trustedProxies = %v, want none", cfg.trustedProxies)
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("RATE_LIMIT_CREATE_PER_MINUTE", "2")
		t.Setenv("RATE_LIMIT_CREATE_BURST", "1")
		t.Setenv("RATE_LIMIT_LOOKUP_PER_MINUTE", "300")
		t.Setenv("RATE_LIMIT_LOOKUP_BURST", "50")
		t.Setenv("RATE_LIMIT_OTHER_PER_MINUTE", "7")
		t.Setenv("RATE_LIMIT_OTHER_BURST", "3")
		t.Setenv("RATE_LIMIT_KEY_CREATE_PER_MINUTE", "4")
		t.Setenv("RATE_LIMIT_KEY_CREATE_BURST", "2")
		t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.5")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		want := rateLimitConfig{
			create:    rateLimit{perMinute: 2, burst: 1},
			lookup:    rateLimit{perMinute: 300, burst: 50},
			other:     rateLimit{perMinute: 7, burst: 3},
			keyCreate: rateLimit{perMinute: 4, burst: 2},
		}
		if cfg.limits != want {
			t.Errorf("limits = %+v, want %+v", cfg.limits, want)
		}
		if got, want := fmt.Sprint(cfg.trustedProxies), "[10.0.0.0/8 192.168.1.5/32]"; got != want {
			t.Errorf("trustedProxies = %s, want %s", got, want)
		}
	})

	// A value that can't be used stops the server instead of quietly running
	// with no limit or a different one.
	invalid := []struct {
		key   string
		value string
	}{
		{"RATE_LIMIT_CREATE_PER_MINUTE", "0"},
		{"RATE_LIMIT_CREATE_BURST", "-1"},
		{"RATE_LIMIT_LOOKUP_PER_MINUTE", "ten"},
		{"RATE_LIMIT_LOOKUP_BURST", "1.5"},
		{"RATE_LIMIT_OTHER_PER_MINUTE", "60/min"},
		{"RATE_LIMIT_OTHER_BURST", "0"},
		{"RATE_LIMIT_KEY_CREATE_PER_MINUTE", "0"},
		{"RATE_LIMIT_KEY_CREATE_BURST", "x"},
		{"TRUSTED_PROXIES", "10.0.0.1,proxy.internal"},
	}
	for _, tt := range invalid {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tt.key, tt.value)
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Errorf("err = %v, want one naming %s", err, tt.key)
			}
		})
	}
}

// MAX_PAYLOAD_BYTES can lower the limit, not raise it: the CLI never sends or
// downloads more than the default, so more would only admit shares no CLI can
// pull.
func TestLoadConfigMaxPayload(t *testing.T) {
	for value, want := range map[string]int64{"2097152": 2 << 20, "5242880": 5 << 20} {
		setRequiredEnv(t)
		t.Setenv("MAX_PAYLOAD_BYTES", value)
		cfg, err := loadConfig()
		if err != nil || cfg.maxPayload != want {
			t.Errorf("MAX_PAYLOAD_BYTES=%s: maxPayload = %d, err = %v; want %d", value, cfg.maxPayload, err, want)
		}
	}
	for _, value := range []string{"5242881", "0", "-1", "5MB"} {
		setRequiredEnv(t)
		t.Setenv("MAX_PAYLOAD_BYTES", value)
		if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "MAX_PAYLOAD_BYTES") {
			t.Errorf("MAX_PAYLOAD_BYTES=%s: err = %v, want one naming MAX_PAYLOAD_BYTES", value, err)
		}
	}
}

func TestLoadConfigAbuseControls(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setRequiredEnv(t)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		// Keys off and no storage cap, so a self-hoster is unaffected; the
		// quota and the pull cap have their decided values.
		want := abuseConfig{requireAPIKey: false, sharesPerDay: 10, maxStored: 0, maxPulls: 50}
		if cfg.abuse != want {
			t.Errorf("abuse = %+v, want %+v", cfg.abuse, want)
		}
		if cfg.maxPayload != 5<<20 {
			t.Errorf("maxPayload = %d, want 5 MB", cfg.maxPayload)
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("REQUIRE_API_KEY", "true")
		t.Setenv("API_KEY_SHARES_PER_DAY", "0")
		t.Setenv("MAX_STORED_BYTES", "10737418240")
		t.Setenv("MAX_PULLS_PER_SHARE", "0")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		want := abuseConfig{requireAPIKey: true, sharesPerDay: 0, maxStored: 10 << 30, maxPulls: 0}
		if cfg.abuse != want {
			t.Errorf("abuse = %+v, want %+v", cfg.abuse, want)
		}
	})

	invalid := []struct {
		key   string
		value string
	}{
		{"REQUIRE_API_KEY", "yes"},
		{"REQUIRE_API_KEY", "on"},
		{"API_KEY_SHARES_PER_DAY", "-1"},
		{"API_KEY_SHARES_PER_DAY", "ten"},
		{"MAX_STORED_BYTES", "-5"},
		{"MAX_STORED_BYTES", "10GB"},
		{"MAX_PULLS_PER_SHARE", "-1"},
		{"MAX_PULLS_PER_SHARE", "1.5"},
	}
	for _, tt := range invalid {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tt.key, tt.value)
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Errorf("err = %v, want one naming %s", err, tt.key)
			}
		})
	}
}
