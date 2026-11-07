//go:build integration

// Run against the Docker stack: make up && make test-integration

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func testConfig() S3Config {
	return S3Config{
		Endpoint:       envOr("S3_ENDPOINT", "http://localhost:8333"),
		PublicEndpoint: envOr("S3_PUBLIC_ENDPOINT", "http://localhost:8333"),
		Region:         envOr("S3_REGION", "us-east-1"),
		Bucket:         envOr("S3_BUCKET", "stashvent"),
		UsePathStyle:   true,
		AccessKey:      envOr("S3_ACCESS_KEY", "stashvent-dev"),
		SecretKey:      envOr("S3_SECRET_KEY", "stashvent-dev-secret"),
	}
}

func testObjects() *Objects {
	return NewObjects(testConfig())
}

func TestPing(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*S3Config)
		wantErr bool
	}{
		{"the configured bucket", func(*S3Config) {}, false},
		{"a bucket that doesn't exist", func(c *S3Config) { c.Bucket = "stashvent-no-such-bucket" }, true},
		{"a wrong secret key", func(c *S3Config) { c.SecretKey = "wrong" }, true},
		{"nothing listening", func(c *S3Config) { c.Endpoint = "http://127.0.0.1:1" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.change(&cfg)
			// The server gives each readiness check this long.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			err := NewObjects(cfg).Ping(ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("Ping = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPresignedRoundTrip(t *testing.T) {
	ctx := t.Context()
	objs := testObjects()
	key := fmt.Sprintf("integration-test/%d", time.Now().UnixNano())
	body := []byte("encrypted-payload-stand-in")
	t.Cleanup(func() { _ = objs.Delete(context.Background(), key) })

	putURL, err := objs.PresignPut(ctx, key, int64(len(body)), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("wrong-size PUT is rejected", func(t *testing.T) {
		status := doPut(t, putURL, append(bytes.Clone(body), "-extra"...))
		if status == http.StatusOK {
			t.Fatal("wrong-size PUT was accepted; signed Content-Length is not enforced")
		}
	})

	t.Run("correct-size PUT is accepted", func(t *testing.T) {
		if status := doPut(t, putURL, body); status != http.StatusOK {
			t.Fatalf("PUT = %d, want 200", status)
		}
	})

	t.Run("Size reports the stored size", func(t *testing.T) {
		size, err := objs.Size(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if size != int64(len(body)) {
			t.Errorf("Size = %d, want %d", size, len(body))
		}
	})

	t.Run("presigned GET returns the bytes", func(t *testing.T) {
		getURL, err := objs.PresignGet(ctx, key, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
			t.Errorf("GET = %d %q, want 200 %q", resp.StatusCode, got, body)
		}
	})

	t.Run("Size of missing object is ErrObjectNotFound", func(t *testing.T) {
		if _, err := objs.Size(ctx, key+"-missing"); !errors.Is(err, ErrObjectNotFound) {
			t.Errorf("err = %v, want ErrObjectNotFound", err)
		}
	})
}

func doPut(t *testing.T, url string, body []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
