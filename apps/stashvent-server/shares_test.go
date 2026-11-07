package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/storage"
)

// fakeStore keeps shares in memory. now stands in for the database clock:
// it stamps created_at and decides what is expired or pending for too long.
// order records insertion, standing in for the row ID.
type fakeStore struct {
	shares map[string]storage.Share
	order  []string
	now    func() time.Time
	// beforeActivate, if set, runs at the start of ActivateShare, to stand in
	// for something else changing the share while /complete is in flight.
	beforeActivate func(*fakeStore)
	// beforeRevoke does the same for RevokeShare and DELETE.
	beforeRevoke func(*fakeStore)
	// beforeCreate does the same for CreateShare.
	beforeCreate func(*fakeStore)
	// err, if set, is returned by the calls the handlers make: the database
	// is down.
	err error
	// revokeCtxErr is the state of the context the last RevokeShare ran on.
	revokeCtxErr error

	keys   []storage.APIKey
	hashes map[string]int64 // key hash → key ID
	usage  map[usageDay]*storage.KeyUsage
}

type usageDay struct {
	keyID int64
	day   string // YYYY-MM-DD
}

func newFakeStore(now func() time.Time) *fakeStore {
	return &fakeStore{
		shares: map[string]storage.Share{},
		now:    now,
		hashes: map[string]int64{},
		usage:  map[usageDay]*storage.KeyUsage{},
	}
}

func (f *fakeStore) add(s storage.Share) {
	f.shares[s.ShareID] = s
	f.order = append(f.order, s.ShareID)
}

// CreateShare counts a keyed share toward its key's quota, like the real
// query: a refused one is neither counted nor inserted.
func (f *fakeStore) CreateShare(_ context.Context, n storage.NewShare, quota storage.Quota) error {
	if f.beforeCreate != nil {
		f.beforeCreate(f)
	}
	if f.err != nil {
		return f.err
	}
	if n.APIKeyID != 0 {
		if k, err := f.GetAPIKey(context.Background(), n.APIKeyID); err != nil || k.RevokedAt != nil {
			return storage.ErrAPIKeyNotFound
		}
		d := quota.Day.UTC()
		k := usageDay{n.APIKeyID, d.Format(time.DateOnly)}
		u := f.usage[k]
		if u == nil {
			u = &storage.KeyUsage{APIKeyID: n.APIKeyID, Day: time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)}
		}
		if quota.SharesPerDay != 0 && u.Shares >= quota.SharesPerDay {
			return storage.ErrQuotaExceeded
		}
		u.Shares++
		u.Bytes += n.Size
		f.usage[k] = u
	}
	f.add(storage.Share{NewShare: n, Status: storage.StatusPending, CreatedAt: f.now()})
	return nil
}

func (f *fakeStore) LookupShare(ctx context.Context, id string, maxPulls int) (storage.Share, error) {
	s, err := f.GetActiveShare(ctx, id)
	if err != nil {
		return storage.Share{}, err
	}
	if maxPulls != 0 && s.Lookups >= maxPulls {
		return storage.Share{}, storage.ErrPullLimitReached
	}
	s.Lookups++
	f.shares[id] = s
	return s, nil
}

func (f *fakeStore) StoredBytes(context.Context) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	var n int64
	for _, s := range f.shares {
		if s.ExpiresAt.After(f.now()) {
			n += s.Size
		}
	}
	return n, nil
}

func (f *fakeStore) CreateAPIKey(_ context.Context, keyHash []byte, label string) (storage.APIKey, error) {
	if f.err != nil {
		return storage.APIKey{}, f.err
	}
	k := storage.APIKey{ID: int64(len(f.keys) + 1), Label: label, CreatedAt: f.now()}
	f.keys = append(f.keys, k)
	f.hashes[string(keyHash)] = k.ID
	return k, nil
}

func (f *fakeStore) GetActiveAPIKey(ctx context.Context, keyHash []byte) (storage.APIKey, error) {
	if f.err != nil {
		return storage.APIKey{}, f.err
	}
	k, err := f.GetAPIKey(ctx, f.hashes[string(keyHash)])
	if err != nil || k.RevokedAt != nil {
		return storage.APIKey{}, storage.ErrAPIKeyNotFound
	}
	return k, nil
}

func (f *fakeStore) GetAPIKey(_ context.Context, id int64) (storage.APIKey, error) {
	if id < 1 || id > int64(len(f.keys)) {
		return storage.APIKey{}, storage.ErrAPIKeyNotFound
	}
	return f.keys[id-1], nil
}

func (f *fakeStore) ListAPIKeys(context.Context) ([]storage.APIKey, error) {
	return slices.Clone(f.keys), nil
}

func (f *fakeStore) RevokeAPIKey(ctx context.Context, id int64) (storage.APIKey, error) {
	k, err := f.GetAPIKey(ctx, id)
	if err != nil {
		return k, err
	}
	if k.RevokedAt == nil {
		now := f.now()
		k.RevokedAt = &now
	}
	f.keys[id-1] = k
	return k, nil
}

func (f *fakeStore) BanAPIKey(ctx context.Context, id int64) (storage.APIKey, error) {
	k, err := f.RevokeAPIKey(ctx, id)
	if err != nil {
		return k, err
	}
	if k.BannedAt == nil {
		now := f.now()
		k.BannedAt = &now
	}
	f.keys[id-1] = k
	return k, nil
}

func (f *fakeStore) ListLiveSharesByKey(_ context.Context, keyID int64) ([]storage.ExpiredShare, error) {
	var out []storage.ExpiredShare
	for i, id := range f.order {
		s, ok := f.shares[id]
		if ok && s.APIKeyID == keyID && s.ExpiresAt.After(f.now()) {
			out = append(out, storage.ExpiredShare{ID: int64(i + 1), ShareID: s.ShareID, StorageKey: s.StorageKey})
		}
	}
	return out, nil
}

func (f *fakeStore) ShareAPIKeyID(_ context.Context, shareID string) (int64, error) {
	s, ok := f.shares[shareID]
	if !ok {
		return 0, storage.ErrShareNotFound
	}
	return s.APIKeyID, nil
}

func (f *fakeStore) ListKeyUsage(_ context.Context, since time.Time, keyID int64) ([]storage.KeyUsage, error) {
	from := since.UTC().Format(time.DateOnly)
	var out []storage.KeyUsage
	for k, u := range f.usage {
		if k.day >= from && (keyID == 0 || k.keyID == keyID) {
			out = append(out, *u)
		}
	}
	slices.SortFunc(out, func(a, b storage.KeyUsage) int {
		return cmp.Or(cmp.Compare(a.APIKeyID, b.APIKeyID), a.Day.Compare(b.Day))
	})
	return out, nil
}

// RevokeShare moves the share's expiry to now and keeps the row, like the
// real query.
func (f *fakeStore) RevokeShare(ctx context.Context, id string) error {
	if f.beforeRevoke != nil {
		f.beforeRevoke(f)
	}
	f.revokeCtxErr = ctx.Err()
	s, err := f.GetShare(ctx, id)
	if err != nil {
		return err
	}
	s.ExpiresAt = f.now()
	f.shares[id] = s
	return nil
}

func (f *fakeStore) DeleteShare(_ context.Context, id string) error {
	if _, ok := f.shares[id]; !ok {
		return storage.ErrShareNotFound
	}
	delete(f.shares, id)
	return nil
}

func (f *fakeStore) ListExpired(_ context.Context, minAge, pendingAge time.Duration, afterID int64, limit int) ([]storage.ExpiredShare, error) {
	now := f.now()
	pendingBefore := now.Add(-pendingAge)
	var out []storage.ExpiredShare
	for i, shareID := range f.order {
		id := int64(i + 1)
		s, ok := f.shares[shareID]
		if !ok || id <= afterID || len(out) == limit || s.CreatedAt.After(now.Add(-minAge)) {
			continue
		}
		stalePending := s.Status == storage.StatusPending && !s.CreatedAt.After(pendingBefore)
		if !s.ExpiresAt.After(now) || stalePending {
			out = append(out, storage.ExpiredShare{ID: id, ShareID: s.ShareID, StorageKey: s.StorageKey})
		}
	}
	return out, nil
}

func (f *fakeStore) GetShare(_ context.Context, id string) (storage.Share, error) {
	if f.err != nil {
		return storage.Share{}, f.err
	}
	s, ok := f.shares[id]
	if !ok || !s.ExpiresAt.After(f.now()) {
		return storage.Share{}, storage.ErrShareNotFound
	}
	return s, nil
}

func (f *fakeStore) GetActiveShare(ctx context.Context, id string) (storage.Share, error) {
	s, err := f.GetShare(ctx, id)
	if err != nil {
		return storage.Share{}, err
	}
	if s.Status != storage.StatusActive {
		return storage.Share{}, storage.ErrShareNotFound
	}
	return s, nil
}

func (f *fakeStore) ActivateShare(ctx context.Context, id string, pendingTTL time.Duration) error {
	if f.beforeActivate != nil {
		f.beforeActivate(f)
	}
	s, err := f.GetShare(ctx, id)
	if err != nil {
		return err
	}
	if s.Status != storage.StatusPending || !s.CreatedAt.After(f.now().Add(-pendingTTL)) {
		return storage.ErrShareNotFound
	}
	s.Status = storage.StatusActive
	f.shares[id] = s
	return nil
}

type fakeObjects struct {
	sizes     map[string]int64
	deleteErr map[string]error // by key
	deletes   int              // Delete calls, failed ones included
	gets      int              // PresignGet calls
}

// presignedMarker is in every fake presigned URL, as the signature is in a
// real one, so a test can tell whether a URL reached the logs.
const presignedMarker = "X-Amz-Signature=fake-signature"

func (f *fakeObjects) PresignPut(_ context.Context, key string, _ int64, _ time.Duration) (string, error) {
	return "http://storage.test/" + key + "?X-Amz-Expires=900&" + presignedMarker, nil
}

func (f *fakeObjects) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	f.gets++
	return "http://storage.test/" + key + "?X-Amz-Expires=300&" + presignedMarker, nil
}

func (f *fakeObjects) Size(_ context.Context, key string) (int64, error) {
	n, ok := f.sizes[key]
	if !ok {
		return 0, storage.ErrObjectNotFound
	}
	return n, nil
}

func (f *fakeObjects) Delete(_ context.Context, key string) error {
	f.deletes++
	if err := f.deleteErr[key]; err != nil {
		return err
	}
	delete(f.sizes, key)
	return nil
}

// testLimits are far above what any test sends by accident; tests of the
// limits set their own.
var testLimits = rateLimitConfig{
	create:    rateLimit{perMinute: 6000, burst: 1000},
	lookup:    rateLimit{perMinute: 6000, burst: 1000},
	other:     rateLimit{perMinute: 6000, burst: 1000},
	keyCreate: rateLimit{perMinute: 6000, burst: 1000},
}

// newTestHandler returns a handler with keys off and no caps, on fakes.
func newTestHandler() (*handler, *fakeObjects) {
	objs := &fakeObjects{sizes: map[string]int64{}, deleteErr: map[string]error{}}
	h := &handler{
		objects:    objs,
		maxPayload: 1000,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:        time.Now,
		ready:      &readiness{},
	}
	// The store and the limiters follow the handler's clock, so a test that
	// replaces h.now moves them all.
	clock := func() time.Time { return h.now() }
	store := newFakeStore(clock)
	h.store, h.keys = store, store
	h.limits = newRateLimits(testLimits, nil, clock)
	return h, objs
}

// issueKey adds an API key to the handler's fake store and returns it.
func issueKey(t *testing.T, h *handler, label string) (string, storage.APIKey) {
	t.Helper()
	key, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	k, err := h.store.(*fakeStore).CreateAPIKey(t.Context(), apikey.Hash(key), label)
	if err != nil {
		t.Fatal(err)
	}
	return key, k
}

// createWithKey creates a share with an API key and stores an object of the
// right size, like createUploaded.
func createWithKey(t *testing.T, h *handler, objs *fakeObjects, key string) api.CreateShareResponse {
	t.Helper()
	rec := doKey(t, newMux(h), http.MethodPost, "/v1/shares", "", key, validCreate())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body)
	}
	var created api.CreateShareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	objs.sizes["stashes/"+created.ShareID] = validCreate().Size
	return created
}

// createUploaded creates a share and stores an object of the right size, so
// the share is ready for /complete.
func createUploaded(t *testing.T, h *handler, objs *fakeObjects) api.CreateShareResponse {
	t.Helper()
	rec := do(t, newMux(h), http.MethodPost, "/v1/shares", "", validCreate())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body)
	}
	var created api.CreateShareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	objs.sizes["stashes/"+created.ShareID] = validCreate().Size
	return created
}

func validCreate() api.CreateShareRequest {
	return api.CreateShareRequest{
		Size:             100,
		ExpiresIn:        "6h",
		Salt:             make([]byte, crypto.SaltSize),
		IV:               make([]byte, crypto.NonceSize),
		PayloadFormat:    api.PayloadFormatV1,
		PackagingMethod:  api.PackagingStashRef,
		BundleVersion:    2,
		ObjectFormat:     "sha1",
		SenderGitVersion: "2.32.0",
	}
}

func do(t *testing.T, mux http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doKey(t, mux, method, path, token, "", body)
}

// doKey is do with an API key as well, sent as Authorization: Bearer.
func doKey(t *testing.T, mux http.Handler, method, path, token, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = bytes.NewBufferString(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, r)
	if token != "" {
		req.Header.Set(api.DeleteTokenHeader, token)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return serve(mux, req)
}

// serve runs one request through mux.
func serve(mux http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e api.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decoding error body %q: %v", rec.Body.String(), err)
	}
	return e.Error.Code
}

func TestCreateShareValidation(t *testing.T) {
	mutate := func(f func(*api.CreateShareRequest)) api.CreateShareRequest {
		req := validCreate()
		f(&req)
		return req
	}
	tests := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"malformed JSON", "{", http.StatusBadRequest, api.CodeInvalidRequest},
		{"body over 16 KB", `{"sender_git_version":"` + strings.Repeat("9", maxRequestBody) + `"}`, http.StatusBadRequest, api.CodeInvalidRequest},
		{"bad expiry", mutate(func(r *api.CreateShareRequest) { r.ExpiresIn = "2h" }), http.StatusBadRequest, api.CodeInvalidRequest},
		{"zero size", mutate(func(r *api.CreateShareRequest) { r.Size = 0 }), http.StatusBadRequest, api.CodeInvalidRequest},
		{"size of only a GCM tag", mutate(func(r *api.CreateShareRequest) { r.Size = crypto.TagSize }), http.StatusBadRequest, api.CodeInvalidRequest},
		{"too large", mutate(func(r *api.CreateShareRequest) { r.Size = 1001 }), http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge},
		{"short salt", mutate(func(r *api.CreateShareRequest) { r.Salt = r.Salt[:8] }), http.StatusBadRequest, api.CodeInvalidRequest},
		{"long iv", mutate(func(r *api.CreateShareRequest) { r.IV = make([]byte, 16) }), http.StatusBadRequest, api.CodeInvalidRequest},
		{"unknown payload format", mutate(func(r *api.CreateShareRequest) { r.PayloadFormat = "stashvent-v9" }), http.StatusBadRequest, api.CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestHandler()
			rec := do(t, newMux(h), http.MethodPost, "/v1/shares", "", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if code := errorCode(t, rec); code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

func TestShareLifecycle(t *testing.T) {
	h, objs := newTestHandler()
	mux := newMux(h)

	rec := do(t, mux, http.MethodPost, "/v1/shares", "", validCreate())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body)
	}
	var created api.CreateShareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !crypto.ValidShareID(created.ShareID) || created.DeleteToken == "" {
		t.Fatalf("bad create response %+v", created)
	}
	getPath := "/v1/shares/" + created.ShareID
	completePath := getPath + "/complete"

	steps := []struct {
		name       string
		method     string
		path       string
		token      string
		setup      func()
		wantStatus int
		wantCode   string
	}{
		{"pending share is not found", http.MethodGet, getPath, "", nil, http.StatusNotFound, api.CodeShareNotFound},
		{"malformed ID", http.MethodGet, "/v1/shares/nope", "", nil, http.StatusNotFound, api.CodeShareNotFound},
		{"complete without token", http.MethodPost, completePath, "", nil, http.StatusUnauthorized, api.CodeMissingDeleteToken},
		{"complete with wrong token", http.MethodPost, completePath, "wrong", nil, http.StatusForbidden, api.CodeInvalidDeleteToken},
		{"complete before upload", http.MethodPost, completePath, created.DeleteToken, nil, http.StatusUnprocessableEntity, api.CodeSizeMismatch},
		{"complete with wrong size", http.MethodPost, completePath, created.DeleteToken,
			func() { objs.sizes["stashes/"+created.ShareID] = 99 }, http.StatusUnprocessableEntity, api.CodeSizeMismatch},
		{"complete", http.MethodPost, completePath, created.DeleteToken,
			func() { objs.sizes["stashes/"+created.ShareID] = 100 }, http.StatusNoContent, ""},
		{"complete twice", http.MethodPost, completePath, created.DeleteToken, nil, http.StatusConflict, api.CodeShareNotPending},
		{"get active share", http.MethodGet, getPath, "", nil, http.StatusOK, ""},
	}
	for _, s := range steps {
		if s.setup != nil {
			s.setup()
		}
		rec := do(t, mux, s.method, s.path, s.token, nil)
		if rec.Code != s.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
		if s.wantCode != "" {
			if code := errorCode(t, rec); code != s.wantCode {
				t.Fatalf("%s: code = %q, want %q", s.name, code, s.wantCode)
			}
		}
	}

	if _, ok := objs.sizes["stashes/"+created.ShareID]; !ok {
		t.Error("object missing after completion")
	}
}

func TestCompleteAbandonedShare(t *testing.T) {
	h, objs := newTestHandler()
	created := createUploaded(t, h, objs)
	completePath := "/v1/shares/" + created.ShareID + "/complete"

	start := time.Now()
	tests := []struct {
		name       string
		age        time.Duration
		wantStatus int
	}{
		{"at PendingTTL", storage.PendingTTL, http.StatusNotFound},
		{"just before PendingTTL", storage.PendingTTL - time.Minute, http.StatusNoContent},
	}
	for _, tt := range tests {
		h.now = func() time.Time { return start.Add(tt.age) }
		rec := do(t, newMux(h), http.MethodPost, completePath, created.DeleteToken, nil)
		if rec.Code != tt.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", tt.name, rec.Code, tt.wantStatus, rec.Body)
		}
		if tt.wantStatus == http.StatusNotFound {
			if code := errorCode(t, rec); code != api.CodeShareNotFound {
				t.Errorf("%s: code = %q, want %q", tt.name, code, api.CodeShareNotFound)
			}
		}
	}
}

// The share changes between /complete's checks and its update.
func TestCompleteLosesRace(t *testing.T) {
	tests := []struct {
		name       string
		interfere  func(f *fakeStore, shareID string)
		wantStatus int
		wantCode   string
	}{
		{
			"another /complete activated it",
			func(f *fakeStore, shareID string) {
				s := f.shares[shareID]
				s.Status = storage.StatusActive
				f.shares[shareID] = s
			},
			http.StatusConflict, api.CodeShareNotPending,
		},
		{
			"the share was removed",
			func(f *fakeStore, shareID string) { delete(f.shares, shareID) },
			http.StatusNotFound, api.CodeShareNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, objs := newTestHandler()
			created := createUploaded(t, h, objs)
			h.store.(*fakeStore).beforeActivate = func(f *fakeStore) { tt.interfere(f, created.ShareID) }

			rec := do(t, newMux(h), http.MethodPost, "/v1/shares/"+created.ShareID+"/complete", created.DeleteToken, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if code := errorCode(t, rec); code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}
