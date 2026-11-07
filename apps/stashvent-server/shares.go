package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/storage"
)

const (
	maxRequestBody = 16 << 10 // JSON bodies carry metadata only
	uploadURLTTL   = 15 * time.Minute
	// downloadURLTTL is short because a presigned GET can be reused until it
	// expires, and the server never sees those downloads. S3 checks expiry
	// when a request starts, so a slow download still finishes.
	downloadURLTTL = 60 * time.Second
	// revokeTimeout bounds the row update in DELETE, which no longer follows
	// the request's context once the object is gone.
	revokeTimeout = 5 * time.Second
)

// shareStore is the metadata store the handlers need (internal/storage/postgres).
type shareStore interface {
	CreateShare(ctx context.Context, n storage.NewShare, quota storage.Quota) error
	GetShare(ctx context.Context, shareID string) (storage.Share, error)
	GetActiveShare(ctx context.Context, shareID string) (storage.Share, error)
	LookupShare(ctx context.Context, shareID string, maxPulls int) (storage.Share, error)
	StoredBytes(ctx context.Context) (int64, error)
	ActivateShare(ctx context.Context, shareID string, pendingTTL time.Duration) error
	RevokeShare(ctx context.Context, shareID string) error
}

// keyStore looks up senders' API keys (internal/storage/postgres).
type keyStore interface {
	GetActiveAPIKey(ctx context.Context, keyHash []byte) (storage.APIKey, error)
}

// objectStore is the payload storage the handlers need (storage.Objects).
type objectStore interface {
	PresignPut(ctx context.Context, key string, size int64, ttl time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Size(ctx context.Context, key string) (int64, error)
	Delete(ctx context.Context, key string) error
}

type handler struct {
	store      shareStore
	keys       keyStore
	objects    objectStore
	maxPayload int64
	abuse      abuseConfig
	logger     *slog.Logger
	now        func() time.Time
	limits     *rateLimits
	ready      *readiness
}

func (h *handler) createShare(w http.ResponseWriter, r *http.Request) {
	// With keys on, the sender is known before anything else is looked at.
	// With keys off, Authorization is never read.
	var keyID int64
	if h.abuse.requireAPIKey {
		key, sent, err := h.requestKey(r)
		switch {
		case !sent:
			writeError(w, http.StatusUnauthorized, api.CodeMissingAPIKey, "API key required")
			return
		case errors.Is(err, errInvalidAPIKey):
			writeInvalidKey(w)
			return
		case err != nil:
			h.internalError(w, r, err)
			return
		}
		if wait, ok := h.limits.keyCreate.allow(key.ID); !ok {
			writeRateLimited(w, wait)
			return
		}
		keyID = key.ID
	}

	var req api.CreateShareRequest
	if err := json.NewDecoder(http.MaxBytesReader(baseWriter(w), r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "Malformed JSON body")
		return
	}

	ttl, ok := api.ExpiresIn[req.ExpiresIn]
	switch {
	case !ok:
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "expires_in must be one of 1h, 6h, 24h, 7d")
		return
	case req.Size <= crypto.TagSize: // an encrypted payload is more than its GCM tag
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "size is too small for an encrypted stash")
		return
	case req.Size > h.maxPayload:
		writeError(w, http.StatusRequestEntityTooLarge, api.CodePayloadTooLarge, "Payload exceeds the maximum size")
		return
	case len(req.Salt) != crypto.SaltSize:
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "salt must be 16 bytes")
		return
	case len(req.IV) != crypto.NonceSize:
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "iv must be 12 bytes")
		return
	case req.PayloadFormat != api.PayloadFormatV1:
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "Unknown payload_format")
		return
	case req.PackagingMethod != api.PackagingStashRef:
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "Unknown packaging_method")
		return
	case req.BundleVersion <= 0 || req.ObjectFormat == "" || req.SenderGitVersion == "":
		writeError(w, http.StatusBadRequest, api.CodeInvalidRequest, "Missing bundle format fields")
		return
	}

	// A coarse cap: concurrent creates can each pass it, overshooting by a few
	// shares. Pending shares count, so a burst of pushes can't all pass
	// before any upload finishes.
	if h.abuse.maxStored > 0 {
		stored, err := h.store.StoredBytes(r.Context())
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		if stored+req.Size > h.abuse.maxStored {
			writeError(w, http.StatusServiceUnavailable, api.CodeStorageFull, "The server's storage is full")
			return
		}
	}

	shareID, err := crypto.NewShareID()
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	token, err := crypto.NewDeleteToken()
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	now := h.now().UTC()
	share := storage.NewShare{
		ShareID:          shareID,
		StorageKey:       "stashes/" + shareID,
		Size:             req.Size,
		Salt:             req.Salt,
		IV:               req.IV,
		DeleteTokenHash:  crypto.HashToken(token),
		PayloadFormat:    req.PayloadFormat,
		PackagingMethod:  req.PackagingMethod,
		BundleVersion:    req.BundleVersion,
		ObjectFormat:     req.ObjectFormat,
		SenderGitVersion: req.SenderGitVersion,
		ExpiresAt:        now.Add(ttl),
		APIKeyID:         keyID,
	}
	// A share counts toward the key's quota when it is created, completed or
	// not, so abandoned shares can't be used to get around it.
	err = h.store.CreateShare(r.Context(), share, storage.Quota{Day: now, SharesPerDay: h.abuse.sharesPerDay})
	if errors.Is(err, storage.ErrQuotaExceeded) {
		setRetryAfter(w, untilUTCMidnight(now))
		writeError(w, http.StatusTooManyRequests, api.CodeQuotaExceeded, "This API key has created its shares for today")
		return
	}
	if errors.Is(err, storage.ErrAPIKeyNotFound) { // revoked or banned while this request ran
		writeInvalidKey(w)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	uploadURL, err := h.objects.PresignPut(r.Context(), share.StorageKey, share.Size, uploadURLTTL)
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, api.CreateShareResponse{
		ShareID:         shareID,
		DeleteToken:     token,
		UploadURL:       uploadURL,
		UploadExpiresAt: now.Add(uploadURLTTL),
		ExpiresAt:       share.ExpiresAt,
	})
}

// untilUTCMidnight is how long from now until the next UTC day starts, when a
// key's daily quota does.
func untilUTCMidnight(now time.Time) time.Duration {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC).Sub(u)
}

// authorizedShare returns the unexpired share named in the path, in any
// status, once the request has proved it may change the share: with its
// delete token, or, when allowKey is set and the server requires keys, with
// the API key that created it. /complete passes false: a key can never
// activate an upload. If the request can't prove it,
// authorizedShare writes the error response and returns false.
//
// The order: malformed ID (404), no credential (401), a key-only request
// with an unusable key (401, before the lookup), the share (404), a matching
// token, then the key: unusable (401), the share's own (allowed), another
// key's (403 share_not_owned), and last a wrong token (403). A matching token
// wins over anything wrong with a key sent along, so a revoked key never
// stops a delete its token allows.
func (h *handler) authorizedShare(w http.ResponseWriter, r *http.Request, allowKey bool) (storage.Share, bool) {
	shareID := r.PathValue("id")
	if !crypto.ValidShareID(shareID) {
		writeNotFound(w)
		return storage.Share{}, false
	}
	token := r.Header.Get(api.DeleteTokenHeader)

	// With keys off, Authorization is never read.
	var key storage.APIKey
	var keySent bool
	var keyErr error
	if allowKey && h.abuse.requireAPIKey {
		key, keySent, keyErr = h.requestKey(r)
		if keyErr != nil && !errors.Is(keyErr, errInvalidAPIKey) {
			h.internalError(w, r, keyErr)
			return storage.Share{}, false
		}
	}
	switch {
	case token == "" && !keySent:
		msg := "Delete token required"
		if allowKey && h.abuse.requireAPIKey {
			msg = "Delete token or API key required"
		}
		writeError(w, http.StatusUnauthorized, api.CodeMissingDeleteToken, msg)
		return storage.Share{}, false
	case token == "" && keyErr != nil:
		writeInvalidKey(w)
		return storage.Share{}, false
	}

	share, err := h.store.GetShare(r.Context(), shareID)
	if errors.Is(err, storage.ErrShareNotFound) {
		writeNotFound(w)
		return storage.Share{}, false
	}
	if err != nil {
		h.internalError(w, r, err)
		return storage.Share{}, false
	}
	switch {
	case token != "" && subtle.ConstantTimeCompare(crypto.HashToken(token), share.DeleteTokenHash) == 1:
		return share, true
	case keySent && keyErr != nil:
		writeInvalidKey(w)
	case keySent && share.APIKeyID == key.ID:
		return share, true
	case keySent:
		writeError(w, http.StatusForbidden, api.CodeShareNotOwned, "This API key did not create the share")
	default:
		writeError(w, http.StatusForbidden, api.CodeInvalidDeleteToken, "Delete token does not match")
	}
	return storage.Share{}, false
}

func (h *handler) completeShare(w http.ResponseWriter, r *http.Request) {
	share, ok := h.authorizedShare(w, r, false)
	if !ok {
		return
	}
	shareID := share.ShareID
	if share.Status != storage.StatusPending {
		writeError(w, http.StatusConflict, api.CodeShareNotPending, "Share is already complete")
		return
	}

	size, err := h.objects.Size(r.Context(), share.StorageKey)
	if errors.Is(err, storage.ErrObjectNotFound) {
		writeError(w, http.StatusUnprocessableEntity, api.CodeSizeMismatch, "Uploaded payload not found")
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if size != share.Size {
		if err := h.objects.Delete(r.Context(), share.StorageKey); err != nil {
			h.logger.ErrorContext(r.Context(), "deleting mismatched object", slog.String("share_id", shareID), slog.Any("error", err))
		}
		writeError(w, http.StatusUnprocessableEntity, api.CodeSizeMismatch, "Uploaded payload size does not match")
		return
	}

	// The store refuses a share that has been pending for PendingTTL or longer.
	// It measures that with the database clock, the one that stamped
	// created_at, so a clock difference between server and database can't
	// reject fresh shares or let the expiry sweep overlap a /complete.
	err = h.store.ActivateShare(r.Context(), shareID, storage.PendingTTL)
	if errors.Is(err, storage.ErrShareNotFound) {
		h.writeNotPending(w, r, shareID)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeNotPending answers a /complete that found nothing to activate, although
// the share was pending a moment ago: 409 if another /complete got there
// first, 404 if the share expired, was pending for too long, or was removed.
func (h *handler) writeNotPending(w http.ResponseWriter, r *http.Request, shareID string) {
	_, err := h.store.GetActiveShare(r.Context(), shareID)
	switch {
	case err == nil:
		writeError(w, http.StatusConflict, api.CodeShareNotPending, "Share is already complete")
	case errors.Is(err, storage.ErrShareNotFound):
		writeNotFound(w)
	default:
		h.internalError(w, r, err)
	}
}

func (h *handler) getShare(w http.ResponseWriter, r *http.Request) {
	shareID := r.PathValue("id")
	if !crypto.ValidShareID(shareID) {
		writeNotFound(w)
		return
	}

	// Every lookup that hands out a download URL counts toward the pull cap,
	// whoever makes it: the server never sees the password, so it can't tell
	// a real pull from a fake one.
	share, err := h.store.LookupShare(r.Context(), shareID, h.abuse.maxPulls)
	if errors.Is(err, storage.ErrShareNotFound) {
		writeNotFound(w)
		return
	}
	if errors.Is(err, storage.ErrPullLimitReached) {
		writePullLimitReached(w)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	downloadURL, err := h.objects.PresignGet(r.Context(), share.StorageKey, downloadURLTTL)
	if err != nil {
		h.internalError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, api.GetShareResponse{
		ShareID:         share.ShareID,
		Size:            share.Size,
		Salt:            share.Salt,
		IV:              share.IV,
		PayloadFormat:   share.PayloadFormat,
		PackagingMethod: share.PackagingMethod,
		BundleVersion:   share.BundleVersion,
		ObjectFormat:    share.ObjectFormat,
		DownloadURL:     downloadURL,
		CreatedAt:       share.CreatedAt.UTC(),
		ExpiresAt:       share.ExpiresAt.UTC(),
	})
}

// getShareSummary describes a share without pulling it (for a page a share
// link opens): what may be shown, nothing that helps download it. It doesn't
// count toward the pull cap, and otherwise answers like getShare: 404 for
// unknown, expired, and pending shares, 410 once the pulls are used up, so a
// caller doesn't offer a pull that would fail.
func (h *handler) getShareSummary(w http.ResponseWriter, r *http.Request) {
	shareID := r.PathValue("id")
	if !crypto.ValidShareID(shareID) {
		writeNotFound(w)
		return
	}

	share, err := h.store.GetActiveShare(r.Context(), shareID)
	if errors.Is(err, storage.ErrShareNotFound) {
		writeNotFound(w)
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if share.PullsUsedUp(h.abuse.maxPulls) {
		writePullLimitReached(w)
		return
	}

	writeJSON(w, http.StatusOK, api.ShareSummaryResponse{
		ShareID:   share.ShareID,
		Size:      share.Size,
		CreatedAt: share.CreatedAt.UTC(),
		ExpiresAt: share.ExpiresAt.UTC(),
	})
}

// deleteShare revokes a share, pending or active: it deletes the object, then
// ends the share, so every later request sees it as expired.
//
// The row is not deleted here. Deleting an object doesn't cancel the upload
// URL that wrote it, so the sender could still put the object back until
// that URL expires, and with the row gone nothing would ever remove it. The
// expiry sweep deletes the row, and the object once more, when the URL can no
// longer be used (uploadGrace).
func (h *handler) deleteShare(w http.ResponseWriter, r *http.Request) {
	share, ok := h.authorizedShare(w, r, true)
	if !ok {
		return
	}
	if err := endShare(r.Context(), h.store, h.objects, share.ShareID, share.StorageKey); err != nil {
		h.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// shareRevoker ends a share's row (internal/storage/postgres).
type shareRevoker interface {
	RevokeShare(ctx context.Context, shareID string) error
}

// endShare ends a share the way DELETE does: it deletes the object, then
// moves the share's expiry to now, so every endpoint answers 404 and the
// expiry sweep removes the row later (see deleteShare). If the object can't
// be deleted the share stays as it was, so the caller can retry. The row
// update is detached from ctx: a caller that goes away once the object is
// gone must not leave a live share pointing at it.
func endShare(ctx context.Context, store shareRevoker, objects objectDeleter, shareID, storageKey string) error {
	if err := objects.Delete(ctx, storageKey); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	// The share may have expired, or another DELETE ended it, meanwhile.
	if err := store.RevokeShare(ctx, shareID); err != nil && !errors.Is(err, storage.ErrShareNotFound) {
		return err
	}
	return nil
}

// internalError logs err (never tokens, URLs, or bodies) and returns a
// generic 500.
func (h *handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "request failed",
		slog.String("method", r.Method), slog.String("route", r.Pattern), slog.Any("error", err))
	writeError(w, http.StatusInternalServerError, api.CodeInternal, "Internal server error")
}

func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, api.CodeShareNotFound, "Share not found or expired")
}

// writePullLimitReached is permanent, so it has no Retry-After.
func writePullLimitReached(w http.ResponseWriter) {
	writeError(w, http.StatusGone, api.CodePullLimitReached, "This share has been pulled the maximum number of times")
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, api.ErrorResponse{Error: api.ErrorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // client disconnects are not actionable
}
