package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
)

// unknownID has the shape of a share ID, but no test creates it.
const unknownID = "AAAAAAAAAAAA"

// deleteCase is one DELETE: a pending share with its object uploaded, and the
// request to send, which starts out as the share's own path and token.
type deleteCase struct {
	h       *handler
	objs    *fakeObjects
	created api.CreateShareResponse
	path    string
	token   string
}

// isLive reports whether the store still serves the share: it has a row that
// hasn't expired.
func isLive(h *handler, shareID string) bool {
	s, ok := h.store.(*fakeStore).shares[shareID]
	return ok && s.ExpiresAt.After(h.now())
}

func TestDeleteShare(t *testing.T) {
	errDown := errors.New("connection refused")

	tests := []struct {
		name       string
		setup      func(t *testing.T, c *deleteCase)
		wantStatus int
		wantCode   string
		wantObject bool // the object is still there afterwards
		wantLive   bool // and the share can still be used
	}{
		{
			name:       "no token",
			setup:      func(_ *testing.T, c *deleteCase) { c.token = "" },
			wantStatus: http.StatusUnauthorized, wantCode: api.CodeMissingDeleteToken, wantObject: true, wantLive: true,
		},
		{
			name:       "wrong token",
			setup:      func(_ *testing.T, c *deleteCase) { c.token = "wrong" },
			wantStatus: http.StatusForbidden, wantCode: api.CodeInvalidDeleteToken, wantObject: true, wantLive: true,
		},
		{
			name:       "another share's token",
			setup:      func(t *testing.T, c *deleteCase) { c.token = createUploaded(t, c.h, c.objs).DeleteToken },
			wantStatus: http.StatusForbidden, wantCode: api.CodeInvalidDeleteToken, wantObject: true, wantLive: true,
		},
		{
			name:       "malformed ID",
			setup:      func(_ *testing.T, c *deleteCase) { c.path = "/v1/shares/nope" },
			wantStatus: http.StatusNotFound, wantCode: api.CodeShareNotFound, wantObject: true, wantLive: true,
		},
		{
			name:       "unknown ID",
			setup:      func(_ *testing.T, c *deleteCase) { c.path = "/v1/shares/" + unknownID },
			wantStatus: http.StatusNotFound, wantCode: api.CodeShareNotFound, wantObject: true, wantLive: true,
		},
		{
			// The sweep removes it; until then it looks like an unknown share.
			name: "expired share",
			setup: func(_ *testing.T, c *deleteCase) {
				later := time.Now().Add(7 * time.Hour)
				c.h.now = func() time.Time { return later }
			},
			wantStatus: http.StatusNotFound, wantCode: api.CodeShareNotFound, wantObject: true,
		},
		{
			name:       "pending share",
			wantStatus: http.StatusNoContent,
		},
		{
			name: "active share",
			setup: func(t *testing.T, c *deleteCase) {
				rec := do(t, newMux(c.h), http.MethodPost, c.path+"/complete", c.token, nil)
				if rec.Code != http.StatusNoContent {
					t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
				}
			},
			wantStatus: http.StatusNoContent,
		},
		{
			// Nothing has changed, so the client can simply try again.
			name: "object can't be deleted",
			setup: func(_ *testing.T, c *deleteCase) {
				c.objs.deleteErr["stashes/"+c.created.ShareID] = errDown
			},
			wantStatus: http.StatusInternalServerError, wantCode: api.CodeInternal, wantObject: true, wantLive: true,
		},
		{
			name: "share expired meanwhile",
			setup: func(_ *testing.T, c *deleteCase) {
				c.h.store.(*fakeStore).beforeRevoke = func(f *fakeStore) {
					s := f.shares[c.created.ShareID]
					s.ExpiresAt = f.now()
					f.shares[c.created.ShareID] = s
				}
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name: "row removed by the sweep meanwhile",
			setup: func(_ *testing.T, c *deleteCase) {
				c.h.store.(*fakeStore).beforeRevoke = func(f *fakeStore) { delete(f.shares, c.created.ShareID) }
			},
			wantStatus: http.StatusNoContent,
		},
		{
			// The object is gone and the share isn't ended: a retry ends it.
			name: "share can't be ended",
			setup: func(_ *testing.T, c *deleteCase) {
				c.h.store.(*fakeStore).beforeRevoke = func(f *fakeStore) { f.err = errDown }
			},
			wantStatus: http.StatusInternalServerError, wantCode: api.CodeInternal, wantLive: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, objs := newTestHandler()
			created := createUploaded(t, h, objs)
			c := &deleteCase{h: h, objs: objs, created: created, path: "/v1/shares/" + created.ShareID, token: created.DeleteToken}
			if tt.setup != nil {
				tt.setup(t, c)
			}

			rec := do(t, newMux(h), http.MethodDelete, c.path, c.token, nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			} else if rec.Body.Len() != 0 {
				t.Errorf("204 with a body: %q", rec.Body)
			}

			if _, hasObject := objs.sizes["stashes/"+created.ShareID]; hasObject != tt.wantObject {
				t.Errorf("object kept = %v, want %v", hasObject, tt.wantObject)
			}
			if live := isLive(h, created.ShareID); live != tt.wantLive {
				t.Errorf("share live = %v, want %v", live, tt.wantLive)
			}
		})
	}
}

// After a DELETE the share is gone for everyone, its creator included.
func TestDeletedShareIsNotFound(t *testing.T) {
	h, objs := newTestHandler()
	mux := newMux(h)
	created := createUploaded(t, h, objs)
	path := "/v1/shares/" + created.ShareID

	steps := []struct {
		name       string
		method     string
		path       string
		token      string
		wantStatus int
	}{
		{"complete", http.MethodPost, path + "/complete", created.DeleteToken, http.StatusNoContent},
		{"get", http.MethodGet, path, "", http.StatusOK},
		{"delete", http.MethodDelete, path, created.DeleteToken, http.StatusNoContent},
		{"get after delete", http.MethodGet, path, "", http.StatusNotFound},
		{"complete after delete", http.MethodPost, path + "/complete", created.DeleteToken, http.StatusNotFound},
		{"delete again", http.MethodDelete, path, created.DeleteToken, http.StatusNotFound},
	}
	for _, s := range steps {
		rec := do(t, mux, s.method, s.path, s.token, nil)
		if rec.Code != s.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
		if s.wantStatus == http.StatusNotFound {
			if code := errorCode(t, rec); code != api.CodeShareNotFound {
				t.Errorf("%s: code = %q, want %q", s.name, code, api.CodeShareNotFound)
			}
		}
	}
}

// A client that disconnects after the object is deleted must not stop the
// share from being ended too.
func TestDeleteFinishesAfterDisconnect(t *testing.T) {
	h, objs := newTestHandler()
	created := createUploaded(t, h, objs)
	store := h.store.(*fakeStore)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodDelete, "/v1/shares/"+created.ShareID, nil)
	req.Header.Set(api.DeleteTokenHeader, created.DeleteToken)
	// The object is gone by the time the store is asked to end the share.
	store.beforeRevoke = func(*fakeStore) { cancel() }

	rec := serve(newMux(h), req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body)
	}
	if store.revokeCtxErr != nil {
		t.Errorf("the share was ended on the request's context: %v", store.revokeCtxErr)
	}
	if isLive(h, created.ShareID) {
		t.Error("share still live after its object was deleted")
	}
}

// Deleting an object doesn't cancel the upload URL that wrote it, so the
// sender can put the object back after a DELETE. The row is kept until that
// URL can no longer be used, and then the sweep removes both.
func TestUploadAfterDeleteIsSwept(t *testing.T) {
	h, objs := newTestHandler()
	createdAt := time.Now()
	now := createdAt
	h.now = func() time.Time { return now }
	store := h.store.(*fakeStore)
	sw := &sweeper{store: store, objects: objs, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	created := createUploaded(t, h, objs)
	key := "stashes/" + created.ShareID
	urlDead := uploadURLTTL + uploadGrace // after the share was created

	if rec := do(t, newMux(h), http.MethodDelete, "/v1/shares/"+created.ShareID, created.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", rec.Code, rec.Body)
	}
	if _, hasObject := objs.sizes[key]; hasObject {
		t.Fatal("object kept by the delete")
	}

	steps := []struct {
		name     string
		at       time.Duration // since the share was created
		upload   bool          // the sender uses the upload URL again first
		wantKept bool          // the row, and the object if there is one
	}{
		{"a sweep right after the delete keeps the row", time.Minute, false, true},
		{"an upload while the URL still works stays for now", uploadURLTTL - time.Minute, true, true},
		{"still kept just before the URL is surely dead", urlDead - time.Second, false, true},
		{"then the sweep removes the upload and the row", urlDead, false, false},
	}
	uploaded := false
	for _, s := range steps {
		now = createdAt.Add(s.at)
		if s.upload {
			objs.sizes[key] = validCreate().Size
			uploaded = true
		}
		// No request brings the share back, whatever was uploaded.
		for _, req := range []struct{ method, path, token string }{
			{http.MethodGet, "/v1/shares/" + created.ShareID, ""},
			{http.MethodPost, "/v1/shares/" + created.ShareID + "/complete", created.DeleteToken},
		} {
			if rec := do(t, newMux(h), req.method, req.path, req.token, nil); rec.Code != http.StatusNotFound {
				t.Fatalf("%s: %s = %d, want 404", s.name, req.method, rec.Code)
			}
		}

		sw.sweep(t.Context())

		_, hasRow := store.shares[created.ShareID]
		_, hasObject := objs.sizes[key]
		if hasRow != s.wantKept || hasObject != (s.wantKept && uploaded) {
			t.Fatalf("%s: row kept = %v, object kept = %v, want row %v and object %v",
				s.name, hasRow, hasObject, s.wantKept, s.wantKept && uploaded)
		}
	}
}
