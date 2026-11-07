package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
)

// newKeyHandler returns a test handler with REQUIRE_API_KEY on.
func newKeyHandler() (*handler, *fakeObjects) {
	h, objs := newTestHandler()
	h.abuse.requireAPIKey = true
	return h, objs
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d (%s)", rec.Code, status, rec.Body)
		return
	}
	if got := errorCode(t, rec); got != code {
		t.Errorf("code = %q, want %q", got, code)
	}
}

func TestCreateShareWithAPIKey(t *testing.T) {
	h, objs := newKeyHandler()
	mux := newMux(h)
	key, k := issueKey(t, h, "alice")
	revoked, rk := issueKey(t, h, "revoked")
	banned, bk := issueKey(t, h, "banned")
	store := h.store.(*fakeStore)
	if _, err := store.RevokeAPIKey(t.Context(), rk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BanAPIKey(t.Context(), bk.ID); err != nil {
		t.Fatal(err)
	}
	unknown, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	// One key in 62 already ends in the replacement character.
	mistyped := key[:len(key)-1] + "X"
	if mistyped == key {
		mistyped = key[:len(key)-1] + "Y"
	}

	refused := []struct {
		name   string
		header string
		code   string
	}{
		{"no key", "", api.CodeMissingAPIKey},
		{"not Bearer", "Basic " + key, api.CodeInvalidAPIKey},
		{"Bearer without a key", "Bearer", api.CodeInvalidAPIKey},
		{"no sv_ prefix", "Bearer " + key[len(apikey.Prefix):], api.CodeInvalidAPIKey},
		{"wrong checksum", "Bearer " + mistyped, api.CodeInvalidAPIKey},
		{"unknown key", "Bearer " + unknown, api.CodeInvalidAPIKey},
		{"revoked key", "Bearer " + revoked, api.CodeInvalidAPIKey},
		{"banned key", "Bearer " + banned, api.CodeInvalidAPIKey},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			req := newJSONRequest(t, http.MethodPost, "/v1/shares", validCreate())
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			wantError(t, serve(mux, req), http.StatusUnauthorized, tt.code)
		})
	}
	if n := len(store.shares); n != 0 {
		t.Fatalf("%d shares created by refused requests", n)
	}

	t.Run("a valid key creates a share recorded with it", func(t *testing.T) {
		created := createWithKey(t, h, objs, key)
		if got := store.shares[created.ShareID].APIKeyID; got != k.ID {
			t.Errorf("share's key = %d, want %d", got, k.ID)
		}
		// lower-case scheme, as RFC 9110 allows
		req := newJSONRequest(t, http.MethodPost, "/v1/shares", validCreate())
		req.Header.Set("Authorization", "bearer "+key)
		if rec := serve(mux, req); rec.Code != http.StatusCreated {
			t.Errorf("lower-case bearer: %d (%s)", rec.Code, rec.Body)
		}
	})

	// The store re-checks the key in the transaction that inserts the share,
	// so a ban that lands while the request runs can't miss the share.
	t.Run("a key revoked while the request runs", func(t *testing.T) {
		late, lk := issueKey(t, h, "revoked-late")
		before := len(store.shares)
		store.beforeCreate = func(f *fakeStore) { _, _ = f.RevokeAPIKey(t.Context(), lk.ID) }
		defer func() { store.beforeCreate = nil }()
		wantError(t, doKey(t, mux, http.MethodPost, "/v1/shares", "", late, validCreate()), http.StatusUnauthorized, api.CodeInvalidAPIKey)
		if len(store.shares) != before {
			t.Error("a share was created with a key revoked meanwhile")
		}
	})

	t.Run("the key is checked before the body", func(t *testing.T) {
		wantError(t, do(t, mux, http.MethodPost, "/v1/shares", "", "{"), http.StatusUnauthorized, api.CodeMissingAPIKey)
	})
}

func newJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// With keys off, Authorization is never read: any value, or none, is fine,
// and nothing is recorded.
func TestKeysOffIgnoresAuthorization(t *testing.T) {
	h, objs := newTestHandler()
	mux := newMux(h)
	for _, header := range []string{"", "Bearer nonsense", "Bearer sv_" + strings.Repeat("a", 49)} {
		req := newJSONRequest(t, http.MethodPost, "/v1/shares", validCreate())
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := serve(mux, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("Authorization %q: %d (%s)", header, rec.Code, rec.Body)
		}
	}
	for id, s := range h.store.(*fakeStore).shares {
		if s.APIKeyID != 0 {
			t.Errorf("share %s recorded key %d", id, s.APIKeyID)
		}
	}

	// DELETE with only a key is a delete without a token.
	created := createUploaded(t, h, objs)
	key, _ := issueKey(t, h, "unused")
	wantError(t, doKey(t, mux, http.MethodDelete, "/v1/shares/"+created.ShareID, "", key, nil),
		http.StatusUnauthorized, api.CodeMissingDeleteToken)
}

func TestKeyCreateRateLimit(t *testing.T) {
	h, _ := newKeyHandler()
	limits := testLimits
	limits.keyCreate = rateLimit{perMinute: 1, burst: 2}
	h.limits = newRateLimits(limits, nil, h.now)
	mux := newMux(h)
	alice, _ := issueKey(t, h, "alice")
	bob, _ := issueKey(t, h, "bob")

	for i := range 2 {
		if rec := doKey(t, mux, http.MethodPost, "/v1/shares", "", alice, validCreate()); rec.Code != http.StatusCreated {
			t.Fatalf("create %d = %d (%s)", i, rec.Code, rec.Body)
		}
	}
	rec := doKey(t, mux, http.MethodPost, "/v1/shares", "", alice, validCreate())
	wantError(t, rec, http.StatusTooManyRequests, api.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Each key has its own bucket, from the same address.
	if rec := doKey(t, mux, http.MethodPost, "/v1/shares", "", bob, validCreate()); rec.Code != http.StatusCreated {
		t.Errorf("another key: %d (%s)", rec.Code, rec.Body)
	}
}

func TestDailyQuota(t *testing.T) {
	h, _ := newKeyHandler()
	h.abuse.sharesPerDay = 2
	evening := time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return evening }
	mux := newMux(h)
	key, k := issueKey(t, h, "alice")

	for i := range 2 {
		if rec := doKey(t, mux, http.MethodPost, "/v1/shares", "", key, validCreate()); rec.Code != http.StatusCreated {
			t.Fatalf("create %d = %d (%s)", i, rec.Code, rec.Body)
		}
	}
	rec := doKey(t, mux, http.MethodPost, "/v1/shares", "", key, validCreate())
	wantError(t, rec, http.StatusTooManyRequests, api.CodeQuotaExceeded)
	if got, want := rec.Header().Get("Retry-After"), "14400"; got != want {
		t.Errorf("Retry-After = %q, want %q (seconds to UTC midnight)", got, want)
	}

	// A share counts when it is created: deleting one gives nothing back.
	store := h.store.(*fakeStore)
	if got := store.usage[usageDay{k.ID, "2026-10-17"}].Shares; got != 2 {
		t.Errorf("counted %d shares, want 2", got)
	}

	// The next UTC day starts at zero.
	h.now = func() time.Time { return evening.Add(4 * time.Hour) }
	if rec := doKey(t, mux, http.MethodPost, "/v1/shares", "", key, validCreate()); rec.Code != http.StatusCreated {
		t.Errorf("first create of the next day = %d (%s)", rec.Code, rec.Body)
	}
}

func TestUntilUTCMidnight(t *testing.T) {
	tests := []struct {
		now  time.Time
		want time.Duration
	}{
		{time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), 24 * time.Hour},
		{time.Date(2026, 10, 17, 23, 59, 59, 500e6, time.UTC), 500 * time.Millisecond},
		// 20:00 in New York is midnight UTC: a new day starts right away.
		{time.Date(2026, 10, 17, 19, 0, 0, 0, time.FixedZone("EDT", -4*60*60)), time.Hour},
		{time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), 12 * time.Hour},
	}
	for _, tt := range tests {
		if got := untilUTCMidnight(tt.now); got != tt.want {
			t.Errorf("untilUTCMidnight(%v) = %v, want %v", tt.now, got, tt.want)
		}
	}
}

func TestStorageCap(t *testing.T) {
	h, objs := newTestHandler()
	h.abuse.maxStored = 250 // validCreate() declares 100 bytes
	mux := newMux(h)

	pending := createUploaded(t, h, objs)
	active := createUploaded(t, h, objs)
	if rec := do(t, mux, http.MethodPost, "/v1/shares/"+active.ShareID+"/complete", active.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}

	// 200 stored, pending included: another 100 would pass the cap.
	rec := do(t, mux, http.MethodPost, "/v1/shares", "", validCreate())
	wantError(t, rec, http.StatusServiceUnavailable, api.CodeStorageFull)
	small := validCreate()
	small.Size = 50
	if rec := do(t, mux, http.MethodPost, "/v1/shares", "", small); rec.Code != http.StatusCreated {
		t.Fatalf("a share that fits: %d (%s)", rec.Code, rec.Body)
	}

	// A deleted share stops counting at once.
	if rec := do(t, mux, http.MethodDelete, "/v1/shares/"+pending.ShareID, pending.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, mux, http.MethodPost, "/v1/shares", "", validCreate()); rec.Code != http.StatusCreated {
		t.Errorf("after a delete: %d (%s)", rec.Code, rec.Body)
	}
}

func TestPullCap(t *testing.T) {
	h, objs := newTestHandler()
	h.abuse.maxPulls = 3
	mux := newMux(h)
	created := createUploaded(t, h, objs)
	path := "/v1/shares/" + created.ShareID

	// A pending share is not found, and the lookup doesn't count.
	wantError(t, do(t, mux, http.MethodGet, path, "", nil), http.StatusNotFound, api.CodeShareNotFound)
	if rec := do(t, mux, http.MethodPost, path+"/complete", created.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}
	for i := range 3 {
		if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("pull %d = %d (%s)", i+1, rec.Code, rec.Body)
		}
	}
	rec := do(t, mux, http.MethodGet, path, "", nil)
	wantError(t, rec, http.StatusGone, api.CodePullLimitReached)
	if retry := rec.Header().Get("Retry-After"); retry != "" {
		t.Errorf("Retry-After = %q on a permanent answer", retry)
	}
	// An unknown share stays 404.
	wantError(t, do(t, mux, http.MethodGet, "/v1/shares/"+unknownID, "", nil), http.StatusNotFound, api.CodeShareNotFound)

	t.Run("zero is no cap", func(t *testing.T) {
		h, objs := newTestHandler()
		mux := newMux(h)
		created := createUploaded(t, h, objs)
		path := "/v1/shares/" + created.ShareID
		do(t, mux, http.MethodPost, path+"/complete", created.DeleteToken, nil)
		for i := range 100 {
			if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusOK {
				t.Fatalf("pull %d = %d", i+1, rec.Code)
			}
		}
	})
}

func TestDeleteWithAPIKey(t *testing.T) {
	type deleteSetup struct {
		h       *handler
		objs    *fakeObjects
		created api.CreateShareResponse // created with key
		key     string
	}
	tests := []struct {
		name       string
		token      func(s deleteSetup) string
		key        func(t *testing.T, s deleteSetup) string
		path       func(s deleteSetup) string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "the key that created it, without a token",
			key:        func(_ *testing.T, s deleteSetup) string { return s.key },
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "the delete token, without a key",
			token:      func(s deleteSetup) string { return s.created.DeleteToken },
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "the delete token and another key",
			token:      func(s deleteSetup) string { return s.created.DeleteToken },
			key:        func(t *testing.T, s deleteSetup) string { k, _ := issueKey(t, s.h, "bob"); return k },
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "another valid key",
			key:        func(t *testing.T, s deleteSetup) string { k, _ := issueKey(t, s.h, "bob"); return k },
			wantStatus: http.StatusForbidden, wantCode: api.CodeShareNotOwned,
		},
		{
			name:       "another key and a wrong token",
			token:      func(deleteSetup) string { return "wrong" },
			key:        func(t *testing.T, s deleteSetup) string { k, _ := issueKey(t, s.h, "bob"); return k },
			wantStatus: http.StatusForbidden, wantCode: api.CodeShareNotOwned,
		},
		{
			name:       "a wrong token and no key",
			token:      func(deleteSetup) string { return "wrong" },
			wantStatus: http.StatusForbidden, wantCode: api.CodeInvalidDeleteToken,
		},
		{
			name: "the creating key, revoked",
			key: func(t *testing.T, s deleteSetup) string {
				if _, err := s.h.store.(*fakeStore).RevokeAPIKey(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
				return s.key
			},
			wantStatus: http.StatusUnauthorized, wantCode: api.CodeInvalidAPIKey,
		},
		{
			name:       "a malformed key",
			key:        func(*testing.T, deleteSetup) string { return "not-a-key" },
			wantStatus: http.StatusUnauthorized, wantCode: api.CodeInvalidAPIKey,
		},
		{
			// The token is enough; a bad key sent along doesn't stop it.
			name:  "the delete token and the creating key, revoked",
			token: func(s deleteSetup) string { return s.created.DeleteToken },
			key: func(t *testing.T, s deleteSetup) string {
				if _, err := s.h.store.(*fakeStore).RevokeAPIKey(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
				return s.key
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "the delete token and a malformed key",
			token:      func(s deleteSetup) string { return s.created.DeleteToken },
			key:        func(*testing.T, deleteSetup) string { return "not-a-key" },
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "a wrong token and a malformed key",
			token:      func(deleteSetup) string { return "wrong" },
			key:        func(*testing.T, deleteSetup) string { return "not-a-key" },
			wantStatus: http.StatusUnauthorized, wantCode: api.CodeInvalidAPIKey,
		},
		{
			name:       "neither token nor key",
			wantStatus: http.StatusUnauthorized, wantCode: api.CodeMissingDeleteToken,
		},
		{
			name:       "the key, on an unknown share",
			key:        func(_ *testing.T, s deleteSetup) string { return s.key },
			path:       func(deleteSetup) string { return "/v1/shares/" + unknownID },
			wantStatus: http.StatusNotFound, wantCode: api.CodeShareNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, objs := newKeyHandler()
			key, _ := issueKey(t, h, "alice")
			s := deleteSetup{h: h, objs: objs, key: key, created: createWithKey(t, h, objs, key)}
			path := "/v1/shares/" + s.created.ShareID
			if tt.path != nil {
				path = tt.path(s)
			}
			var token, sendKey string
			if tt.token != nil {
				token = tt.token(s)
			}
			if tt.key != nil {
				sendKey = tt.key(t, s)
			}
			rec := doKey(t, newMux(h), http.MethodDelete, path, token, sendKey, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			}
			_, hasObject := objs.sizes["stashes/"+s.created.ShareID]
			if deleted := tt.wantStatus == http.StatusNoContent; hasObject == deleted || isLive(h, s.created.ShareID) == deleted {
				t.Errorf("object kept = %v, live = %v after %d", hasObject, isLive(h, s.created.ShareID), rec.Code)
			}
		})
	}

	t.Run("a share created without a key", func(t *testing.T) {
		h, objs := newTestHandler()
		created := createUploaded(t, h, objs) // keys were off then
		h.abuse.requireAPIKey = true
		key, _ := issueKey(t, h, "alice")
		wantError(t, doKey(t, newMux(h), http.MethodDelete, "/v1/shares/"+created.ShareID, "", key, nil),
			http.StatusForbidden, api.CodeShareNotOwned)
	})

	// Only DELETE accepts the key: it can never activate an upload.
	t.Run("complete with only the key", func(t *testing.T) {
		h, objs := newKeyHandler()
		key, _ := issueKey(t, h, "alice")
		created := createWithKey(t, h, objs, key)
		wantError(t, doKey(t, newMux(h), http.MethodPost, "/v1/shares/"+created.ShareID+"/complete", "", key, nil),
			http.StatusUnauthorized, api.CodeMissingDeleteToken)
	})
}

func TestLogAbuseControls(t *testing.T) {
	tests := []struct {
		name     string
		cfg      abuseConfig
		want     []string
		wantWarn bool
	}{
		{
			name:     "keys off",
			cfg:      abuseConfig{sharesPerDay: 10, maxPulls: 50},
			want:     []string{`"require_api_key":false`, `"max_stored":"no cap"`, `"max_pulls_per_share":"50"`},
			wantWarn: true,
		},
		{
			name: "keys on with a quota",
			cfg:  abuseConfig{requireAPIKey: true, sharesPerDay: 10, maxStored: 1 << 30, maxPulls: 0},
			want: []string{`"require_api_key":true`, `"quota":"10 shares/day per key"`, `"max_stored":"1073741824 bytes"`, `"max_pulls_per_share":"no cap"`},
		},
		{
			name: "keys on without a quota",
			cfg:  abuseConfig{requireAPIKey: true},
			want: []string{`"quota":"no daily quota"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logAbuseControls(newTestLogger(&logs, slog.LevelInfo), tt.cfg)
			for _, want := range tt.want {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log lacks %s:\n%s", want, &logs)
				}
			}
			if warned := strings.Contains(logs.String(), `"level":"WARN"`); warned != tt.wantWarn {
				t.Errorf("warned = %v, want %v:\n%s", warned, tt.wantWarn, &logs)
			}
		})
	}
}
