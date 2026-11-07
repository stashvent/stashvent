package api

import "time"

// Payload and packaging identifiers recorded with every share.
const (
	PayloadFormatV1   = "stashvent-v1"
	PackagingStashRef = "stash-ref"
)

// Allowed values for CreateShareRequest.ExpiresIn.
var ExpiresIn = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

// DefaultExpiresIn is applied by the CLI when the user passes no --expires.
const DefaultExpiresIn = "6h"

// DefaultMaxPayloadBytes is the largest encrypted payload a server accepts
// unless its MAX_PAYLOAD_BYTES sets less. It is also the most the CLI sends or
// downloads: the CLI can't read a server's limit, so it checks against this
// before it creates a share, and a server set higher gains nothing. The
// server's own check (413 payload_too_large) stays the authority below it.
const DefaultMaxPayloadBytes = 5 << 20

// DeleteTokenHeader carries the delete token on /complete and DELETE. It is
// not Authorization, which carries the sender's API key
// ("Authorization: Bearer sv_…") on POST /v1/shares and DELETE, so a request
// can have both.
const DeleteTokenHeader = "X-Delete-Token"

// CreateShareRequest is the body of POST /v1/shares. Salt and IV are []byte,
// so encoding/json carries them as standard base64.
type CreateShareRequest struct {
	Size             int64  `json:"size"`
	ExpiresIn        string `json:"expires_in"`
	Salt             []byte `json:"salt"`
	IV               []byte `json:"iv"`
	PayloadFormat    string `json:"payload_format"`
	PackagingMethod  string `json:"packaging_method"`
	BundleVersion    int    `json:"bundle_version"`
	ObjectFormat     string `json:"object_format"`
	SenderGitVersion string `json:"sender_git_version"`
}

// CreateShareResponse is the 201 body of POST /v1/shares. DeleteToken is
// returned once and never again.
type CreateShareResponse struct {
	ShareID         string    `json:"share_id"`
	DeleteToken     string    `json:"delete_token"`
	UploadURL       string    `json:"upload_url"`
	UploadExpiresAt time.Time `json:"upload_expires_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	// UploadHeaders are headers the upload must carry because the server
	// signed them into UploadURL (server-side encryption, say); the client
	// sends each one as given. None today: it exists so that a server can
	// start signing headers without breaking the clients already installed.
	// It can only hold values the server knows: not a checksum of the
	// payload, which the server never sees and older clients don't send.
	UploadHeaders map[string]string `json:"upload_headers,omitempty"`
}

// GetShareResponse is the 200 body of GET /v1/shares/{id}.
type GetShareResponse struct {
	ShareID         string    `json:"share_id"`
	Size            int64     `json:"size"`
	Salt            []byte    `json:"salt"`
	IV              []byte    `json:"iv"`
	PayloadFormat   string    `json:"payload_format"`
	PackagingMethod string    `json:"packaging_method"`
	BundleVersion   int       `json:"bundle_version"`
	ObjectFormat    string    `json:"object_format"`
	DownloadURL     string    `json:"download_url"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// ShareSummaryResponse is the 200 body of GET /v1/shares/{id}/summary: what
// may be shown about a share without pulling it, e.g. on a page a share link
// opens. It has nothing that helps download the share (no download URL, salt,
// or IV), and the lookup doesn't count toward the pull cap.
type ShareSummaryResponse struct {
	ShareID   string    `json:"share_id"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody carries a stable machine-readable code and a human message.
// Clients switch on Code, never on Message.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes (see the stashvent-api skill for the status that goes with each).
const (
	CodeInvalidRequest     = "invalid_request"
	CodeMissingDeleteToken = "missing_delete_token"
	CodeInvalidDeleteToken = "invalid_delete_token"
	CodeShareNotFound      = "share_not_found"
	CodeShareNotPending    = "share_not_pending"
	CodePayloadTooLarge    = "payload_too_large"
	CodeSizeMismatch       = "size_mismatch"
	CodeRateLimited        = "rate_limited"
	CodeInternal           = "internal"

	// Added with API keys and the abuse controls.
	CodeMissingAPIKey    = "missing_api_key"
	CodeInvalidAPIKey    = "invalid_api_key"
	CodeShareNotOwned    = "share_not_owned"
	CodeQuotaExceeded    = "quota_exceeded"
	CodeStorageFull      = "storage_full"
	CodePullLimitReached = "pull_limit_reached"
)
