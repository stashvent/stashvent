package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
)

// The summary is for a page about a share: it must not use up pulls, sign a
// download URL, or carry anything that helps download the share.
func TestShareSummary(t *testing.T) {
	h, objs := newTestHandler()
	h.abuse.maxPulls = 2
	mux := newMux(h)
	created := createUploaded(t, h, objs)
	path := "/v1/shares/" + created.ShareID
	summary := path + "/summary"

	// Pending: not found, like GET.
	wantError(t, do(t, mux, http.MethodGet, summary, "", nil), http.StatusNotFound, api.CodeShareNotFound)

	if rec := do(t, mux, http.MethodPost, path+"/complete", created.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}

	// Active: more summaries than the pull cap, and none of them counts.
	for i := range 5 {
		rec := do(t, mux, http.MethodGet, summary, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("summary %d = %d (%s)", i+1, rec.Code, rec.Body)
		}
		if i > 0 {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"created_at", "expires_at", "share_id", "size"}; !slices.Equal(keys, want) {
			t.Errorf("fields = %v, want %v", keys, want)
		}
		var got api.ShareSummaryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ShareID != created.ShareID || got.Size != validCreate().Size || !got.ExpiresAt.Equal(created.ExpiresAt) {
			t.Errorf("summary = %+v, created %+v", got, created)
		}
		if got.CreatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
			t.Errorf("created_at = %v, want a UTC time", got.CreatedAt)
		}
	}
	if objs.gets != 0 {
		t.Errorf("%d download URLs signed, want 0", objs.gets)
	}
	if n := h.store.(*fakeStore).shares[created.ShareID].Lookups; n != 0 {
		t.Errorf("lookups = %d, want 0", n)
	}

	// The pulls still all work; once they're used up the summary says so.
	for i := range 2 {
		if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusOK {
			t.Fatalf("pull %d = %d (%s)", i+1, rec.Code, rec.Body)
		}
	}
	rec := do(t, mux, http.MethodGet, summary, "", nil)
	wantError(t, rec, http.StatusGone, api.CodePullLimitReached)
	if retry := rec.Header().Get("Retry-After"); retry != "" {
		t.Errorf("Retry-After = %q on a permanent answer", retry)
	}

	t.Run("not found", func(t *testing.T) {
		h, objs := newTestHandler()
		mux := newMux(h)
		created := createUploaded(t, h, objs)
		path := "/v1/shares/" + created.ShareID
		do(t, mux, http.MethodPost, path+"/complete", created.DeleteToken, nil)

		for _, p := range []string{"/v1/shares/" + unknownID + "/summary", "/v1/shares/short/summary"} {
			wantError(t, do(t, mux, http.MethodGet, p, "", nil), http.StatusNotFound, api.CodeShareNotFound)
		}

		later := time.Now().Add(7 * time.Hour)
		h.now = func() time.Time { return later }
		wantError(t, do(t, mux, http.MethodGet, path+"/summary", "", nil), http.StatusNotFound, api.CodeShareNotFound)
	})

	t.Run("deleted", func(t *testing.T) {
		h, objs := newTestHandler()
		mux := newMux(h)
		created := createUploaded(t, h, objs)
		path := "/v1/shares/" + created.ShareID
		do(t, mux, http.MethodPost, path+"/complete", created.DeleteToken, nil)
		if rec := do(t, mux, http.MethodDelete, path, created.DeleteToken, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("delete = %d (%s)", rec.Code, rec.Body)
		}
		wantError(t, do(t, mux, http.MethodGet, path+"/summary", "", nil), http.StatusNotFound, api.CodeShareNotFound)
	})

	t.Run("no cap", func(t *testing.T) {
		h, objs := newTestHandler()
		mux := newMux(h)
		created := createUploaded(t, h, objs)
		path := "/v1/shares/" + created.ShareID
		do(t, mux, http.MethodPost, path+"/complete", created.DeleteToken, nil)
		for range 3 {
			do(t, mux, http.MethodGet, path, "", nil)
		}
		if rec := do(t, mux, http.MethodGet, path+"/summary", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("summary = %d (%s)", rec.Code, rec.Body)
		}
	})

	t.Run("database down", func(t *testing.T) {
		h, objs := newTestHandler()
		mux := newMux(h)
		created := createUploaded(t, h, objs)
		h.store.(*fakeStore).err = errors.New("connection refused")
		wantError(t, do(t, mux, http.MethodGet, "/v1/shares/"+created.ShareID+"/summary", "", nil),
			http.StatusInternalServerError, api.CodeInternal)
	})
}
