package storage

import (
	"errors"
	"time"
)

// ErrShareNotFound is returned when a share doesn't exist, has expired, or
// (for lookups that require it) isn't active.
var ErrShareNotFound = errors.New("share not found")

// ErrObjectNotFound is returned when an object is missing from storage.
var ErrObjectNotFound = errors.New("object not found")

// ErrQuotaExceeded is returned when an API key has created its daily quota
// of shares already.
var ErrQuotaExceeded = errors.New("daily share quota exceeded")

// ErrPullLimitReached is returned when an active share has been looked up the
// maximum number of times.
var ErrPullLimitReached = errors.New("pull limit reached")

// ErrAPIKeyNotFound is returned for an API key that is unknown or revoked
// (banned ones included): the server answers them all the same way.
var ErrAPIKeyNotFound = errors.New("api key not found")

// Share status values.
const (
	StatusPending = "pending"
	StatusActive  = "active"
)

// PendingTTL is how long a share may stay pending. Upload URLs expire well
// before this, so an older pending share was abandoned: it can no longer be
// completed, and the expiry sweep removes it a little later.
const PendingTTL = time.Hour

// ExpiredShare is what the expiry sweep needs to remove a share. ID is the
// internal row ID, used only to page through the list.
type ExpiredShare struct {
	ID         int64
	ShareID    string
	StorageKey string
}

// NewShare is the metadata recorded when a share is created. It never holds
// the password, derived key, raw delete token, or any repo details.
type NewShare struct {
	ShareID          string
	StorageKey       string
	Size             int64
	Salt             []byte
	IV               []byte
	DeleteTokenHash  []byte
	PayloadFormat    string
	PackagingMethod  string
	BundleVersion    int
	ObjectFormat     string
	SenderGitVersion string
	ExpiresAt        time.Time
	// APIKeyID is the internal ID of the key that created the share, 0 when
	// the server doesn't require keys.
	APIKeyID int64
}

// Share is a stored share row.
type Share struct {
	NewShare
	Status    string
	CreatedAt time.Time
	Lookups   int // GET /v1/shares/{id} calls that were answered
}

// PullsUsedUp reports whether the share has answered maxPulls lookups
// (0: no cap). It is the rule the LookupShare query applies in SQL, for
// callers that only read a share (the summary) and must answer the same.
func (s Share) PullsUsedUp(maxPulls int) bool {
	return maxPulls > 0 && s.Lookups >= maxPulls
}

// Quota is the daily share quota of the key creating a share.
type Quota struct {
	Day          time.Time // the UTC calendar day the share counts for
	SharesPerDay int       // 0: no quota
}

// APIKey is a sender's key, without the key: only its hash is ever stored.
// ID is internal: it is shown to the operator, never in the API.
type APIKey struct {
	ID        int64
	Label     string
	CreatedAt time.Time
	RevokedAt *time.Time // also set by a ban
	BannedAt  *time.Time
}

// KeyUsage is what one key created on one UTC day.
type KeyUsage struct {
	APIKeyID int64
	Day      time.Time
	Shares   int
	Bytes    int64 // declared sizes, completed or not
}
