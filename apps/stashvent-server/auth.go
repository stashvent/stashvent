package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/storage"
)

// errInvalidAPIKey means a request sent a key the server won't accept:
// malformed, unknown, revoked, or banned. They get one answer, so a probe
// can't tell them apart.
var errInvalidAPIKey = errors.New("invalid api key")

// requestKey returns the API key a request was sent with
// (Authorization: Bearer sv_…). sent is false when there is no Authorization
// header. A key that can't be used is errInvalidAPIKey; a malformed one is
// refused without a database lookup. Only call it when the server requires
// keys: otherwise Authorization is ignored.
func (h *handler) requestKey(r *http.Request) (key storage.APIKey, sent bool, err error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return storage.APIKey{}, false, nil
	}
	scheme, token, ok := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || !apikey.Valid(token) {
		return storage.APIKey{}, true, errInvalidAPIKey
	}
	// Looked up by its hash: a key is a 256-bit random secret, so knowing how
	// the lookup works doesn't help anyone guess one.
	key, err = h.keys.GetActiveAPIKey(r.Context(), apikey.Hash(token))
	if errors.Is(err, storage.ErrAPIKeyNotFound) {
		return storage.APIKey{}, true, errInvalidAPIKey
	}
	if err != nil {
		return storage.APIKey{}, true, err
	}
	return key, true, nil
}

func writeInvalidKey(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, api.CodeInvalidAPIKey, "API key not accepted")
}
