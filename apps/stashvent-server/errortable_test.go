package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stashvent/stashvent/internal/api"
)

// errorCase is what one row of TestErrorTable starts from: a pending share
// with its object uploaded.
type errorCase struct {
	h       *handler
	objs    *fakeObjects
	created api.CreateShareResponse
	path    string // of that share
}

func (c errorCase) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, newMux(c.h), method, path, token, body)
}

func (c errorCase) complete(t *testing.T) {
	t.Helper()
	if rec := c.do(t, http.MethodPost, c.path+"/complete", c.created.DeleteToken, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("complete = %d (%s)", rec.Code, rec.Body)
	}
}

// At least one case for every row of the error table in the stashvent-api
// skill. Each must have the documented status, code, and JSON shape.
func TestErrorTable(t *testing.T) {
	const dbError = "connection refused: postgres://stashvent@db"
	tooLarge := validCreate()
	tooLarge.Size = 1001

	tests := []struct {
		name       string
		wantStatus int
		wantCode   string
		request    func(t *testing.T, c errorCase) *httptest.ResponseRecorder
	}{
		{"create with malformed JSON", http.StatusBadRequest, api.CodeInvalidRequest, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodPost, "/v1/shares", "", "{")
		}},
		{"complete without a token", http.StatusUnauthorized, api.CodeMissingDeleteToken, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodPost, c.path+"/complete", "", nil)
		}},
		{"delete without a token", http.StatusUnauthorized, api.CodeMissingDeleteToken, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodDelete, c.path, "", nil)
		}},
		{"complete with a wrong token", http.StatusForbidden, api.CodeInvalidDeleteToken, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodPost, c.path+"/complete", "wrong", nil)
		}},
		{"delete with a wrong token", http.StatusForbidden, api.CodeInvalidDeleteToken, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodDelete, c.path, "wrong", nil)
		}},
		{"get an unknown share", http.StatusNotFound, api.CodeShareNotFound, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodGet, "/v1/shares/"+unknownID, "", nil)
		}},
		{"get a pending share", http.StatusNotFound, api.CodeShareNotFound, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodGet, c.path, "", nil)
		}},
		{"get an expired share", http.StatusNotFound, api.CodeShareNotFound, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.complete(t)
			later := time.Now().Add(7 * time.Hour)
			c.h.now = func() time.Time { return later }
			return c.do(t, http.MethodGet, c.path, "", nil)
		}},
		{"complete an unknown share", http.StatusNotFound, api.CodeShareNotFound, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodPost, "/v1/shares/"+unknownID+"/complete", "token", nil)
		}},
		{"delete an unknown share", http.StatusNotFound, api.CodeShareNotFound, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodDelete, "/v1/shares/"+unknownID, "token", nil)
		}},
		{"complete an active share", http.StatusConflict, api.CodeShareNotPending, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.complete(t)
			return c.do(t, http.MethodPost, c.path+"/complete", c.created.DeleteToken, nil)
		}},
		{"create above the size limit", http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodPost, "/v1/shares", "", tooLarge)
		}},
		{"complete with the wrong size uploaded", http.StatusUnprocessableEntity, api.CodeSizeMismatch, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.objs.sizes["stashes/"+c.created.ShareID] = 99
			return c.do(t, http.MethodPost, c.path+"/complete", c.created.DeleteToken, nil)
		}},
		{"complete with nothing uploaded", http.StatusUnprocessableEntity, api.CodeSizeMismatch, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			delete(c.objs.sizes, "stashes/"+c.created.ShareID)
			return c.do(t, http.MethodPost, c.path+"/complete", c.created.DeleteToken, nil)
		}},
		{"create over the rate limit", http.StatusTooManyRequests, api.CodeRateLimited, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			limits := testLimits
			limits.create = rateLimit{perMinute: 1, burst: 1}
			c.h.limits = newRateLimits(limits, nil, c.h.now)
			if rec := c.do(t, http.MethodPost, "/v1/shares", "", validCreate()); rec.Code != http.StatusCreated {
				t.Fatalf("first create = %d (%s)", rec.Code, rec.Body)
			}
			return c.do(t, http.MethodPost, "/v1/shares", "", validCreate())
		}},
		{"create without an API key", http.StatusUnauthorized, api.CodeMissingAPIKey, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.h.abuse.requireAPIKey = true
			return c.do(t, http.MethodPost, "/v1/shares", "", validCreate())
		}},
		{"create with a malformed API key", http.StatusUnauthorized, api.CodeInvalidAPIKey, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.h.abuse.requireAPIKey = true
			return doKey(t, newMux(c.h), http.MethodPost, "/v1/shares", "", "sv_wrong", validCreate())
		}},
		{"delete with a key that didn't create the share", http.StatusForbidden, api.CodeShareNotOwned, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.h.abuse.requireAPIKey = true
			key, _ := issueKey(t, c.h, "other")
			return doKey(t, newMux(c.h), http.MethodDelete, c.path, "", key, nil)
		}},
		{"create past the key's daily quota", http.StatusTooManyRequests, api.CodeQuotaExceeded, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.h.abuse.requireAPIKey = true
			c.h.abuse.sharesPerDay = 1
			key, _ := issueKey(t, c.h, "busy")
			if rec := doKey(t, newMux(c.h), http.MethodPost, "/v1/shares", "", key, validCreate()); rec.Code != http.StatusCreated {
				t.Fatalf("first create = %d (%s)", rec.Code, rec.Body)
			}
			return doKey(t, newMux(c.h), http.MethodPost, "/v1/shares", "", key, validCreate())
		}},
		{"create with the storage cap reached", http.StatusServiceUnavailable, api.CodeStorageFull, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.h.abuse.maxStored = validCreate().Size // the pending share fills it
			return c.do(t, http.MethodPost, "/v1/shares", "", validCreate())
		}},
		{"get past the pull cap", http.StatusGone, api.CodePullLimitReached, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.complete(t)
			c.h.abuse.maxPulls = 1
			if rec := c.do(t, http.MethodGet, c.path, "", nil); rec.Code != http.StatusOK {
				t.Fatalf("first get = %d (%s)", rec.Code, rec.Body)
			}
			return c.do(t, http.MethodGet, c.path, "", nil)
		}},
		{"summary of a pending share", http.StatusNotFound, api.CodeShareNotFound, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			return c.do(t, http.MethodGet, c.path+"/summary", "", nil)
		}},
		{"summary past the pull cap", http.StatusGone, api.CodePullLimitReached, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.complete(t)
			c.h.abuse.maxPulls = 1
			if rec := c.do(t, http.MethodGet, c.path, "", nil); rec.Code != http.StatusOK {
				t.Fatalf("first get = %d (%s)", rec.Code, rec.Body)
			}
			return c.do(t, http.MethodGet, c.path+"/summary", "", nil)
		}},
		{"create with the database down", http.StatusInternalServerError, api.CodeInternal, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.h.store.(*fakeStore).err = errors.New(dbError)
			return c.do(t, http.MethodPost, "/v1/shares", "", validCreate())
		}},
		{"get with the database down", http.StatusInternalServerError, api.CodeInternal, func(t *testing.T, c errorCase) *httptest.ResponseRecorder {
			c.h.store.(*fakeStore).err = errors.New(dbError)
			return c.do(t, http.MethodGet, c.path, "", nil)
		}},
	}

	covered := map[string]bool{}
	for _, tt := range tests {
		covered[tt.wantCode] = true
		t.Run(tt.name, func(t *testing.T) {
			h, objs := newTestHandler()
			created := createUploaded(t, h, objs)
			rec := tt.request(t, errorCase{h: h, objs: objs, created: created, path: "/v1/shares/" + created.ShareID})

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			var body api.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding %q: %v", rec.Body, err)
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("no message")
			}
			// What went wrong inside goes to the logs only.
			if strings.Contains(rec.Body.String(), "postgres") {
				t.Errorf("response carries the internal error: %s", rec.Body)
			}
			if rec.Header().Get("X-Request-Id") == "" {
				t.Error("no X-Request-Id header")
			}
			if retry := rec.Header().Get("Retry-After"); (retry != "") != (tt.wantStatus == http.StatusTooManyRequests) {
				t.Errorf("Retry-After = %q on a %d", retry, tt.wantStatus)
			}
		})
	}

	allCodes := []string{
		api.CodeInvalidRequest, api.CodeMissingDeleteToken, api.CodeInvalidDeleteToken,
		api.CodeShareNotFound, api.CodeShareNotPending, api.CodePayloadTooLarge,
		api.CodeSizeMismatch, api.CodeRateLimited, api.CodeInternal,
		api.CodeMissingAPIKey, api.CodeInvalidAPIKey, api.CodeShareNotOwned,
		api.CodeQuotaExceeded, api.CodeStorageFull, api.CodePullLimitReached,
	}
	for _, code := range allCodes {
		if !covered[code] {
			t.Errorf("no case for error code %q", code)
		}
	}
}
