//go:build integration

// Run against the Docker stack: make up && make test-integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/storage"
)

// issueIntegrationKey issues a key with the operator command on the real
// store, and removes the key row (and with it its usage) when the test ends.
func (s *integrationServer) issueKey(t *testing.T, op *operator, label string) string {
	t.Helper()
	var stdout bytes.Buffer
	op.stdout = &stdout
	if err := op.issue(t.Context(), label); err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(stdout.String())
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM api_keys WHERE label = $1`, label)
	})
	return key
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		t.Errorf("err = %v, want %s", err, code)
	}
}

// With keys on: the operator issues a key, the key pushes and deletes, the
// pull cap holds, and a ban ends the key's shares.
func TestAPIKeysOnDockerStack(t *testing.T) {
	ctx := t.Context()
	s := newIntegrationServerWith(t, abuseConfig{requireAPIKey: true, sharesPerDay: 10, maxPulls: 2})
	op := &operator{store: s.store, objects: s.objects, stdout: io.Discard, stderr: io.Discard, now: time.Now}
	label := "integration-" + rand.Text()

	_, err := s.client.CreateShare(ctx, api.CreateShareRequest{}, "")
	wantCode(t, err, api.CodeMissingAPIKey)

	s.apiKey = s.issueKey(t, op, label)
	payload := []byte("encrypted-payload-stand-in")
	upload := func(created api.CreateShareResponse) {
		t.Helper()
		if err := s.client.Upload(ctx, created.UploadURL, created.UploadHeaders, payload); err != nil {
			t.Fatal(err)
		}
		if err := s.client.CompleteShare(ctx, created.ShareID, created.DeleteToken); err != nil {
			t.Fatal(err)
		}
	}

	// Pull cap: two lookups, then 410.
	first := s.create(t, payload)
	upload(first)
	for range 2 {
		if _, err := s.client.GetShare(ctx, first.ShareID); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.client.GetShare(ctx, first.ShareID)
	wantCode(t, err, api.CodePullLimitReached)

	// The key deletes its share without the delete token.
	if err := s.client.DeleteShare(ctx, first.ShareID, "", s.apiKey); err != nil {
		t.Fatalf("delete with the key: %v", err)
	}
	if _, err := s.objects.Size(ctx, "stashes/"+first.ShareID); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("object after delete: err = %v, want ErrObjectNotFound", err)
	}

	// A ban ends the key's live shares and refuses the key.
	second := s.create(t, payload)
	upload(second)
	ids, err := s.store.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var keyID int64
	for _, k := range ids {
		if k.Label == label {
			keyID = k.ID
		}
	}
	if err := op.ban(ctx, keyID); err != nil {
		t.Fatal(err)
	}
	_, err = s.client.GetShare(ctx, second.ShareID)
	wantCode(t, err, api.CodeShareNotFound)
	if _, err := s.objects.Size(ctx, "stashes/"+second.ShareID); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("object after ban: err = %v, want ErrObjectNotFound", err)
	}
	_, err = s.client.CreateShare(ctx, api.CreateShareRequest{}, s.apiKey)
	wantCode(t, err, api.CodeInvalidAPIKey)

	// Both shares were counted, the deleted and the banned one.
	usage, err := s.store.ListKeyUsage(ctx, time.Now(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].Shares != 2 || usage[0].Bytes != 2*int64(len(payload)) {
		t.Errorf("usage = %+v, want 2 shares today", usage)
	}
}
