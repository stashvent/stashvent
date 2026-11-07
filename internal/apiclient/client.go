// Package apiclient is the CLI's HTTP client for the /v1 API. It uses the
// shared types in internal/api, and never logs tokens or presigned URLs.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/crypto"
)

const (
	apiTimeout = 30 * time.Second
	// maxResponseBytes caps an API response body. The real ones are a few KB;
	// a pull talks to whichever server a link names, so it must not be able to
	// stream an endless one into memory.
	maxResponseBytes = 64 << 10
	transferTimeout  = 5 * time.Minute
)

// Error is a non-2xx API response. Callers switch on Code.
type Error struct {
	Status int
	Code   string
	// Server is the server that answered, as given to New.
	Server    string
	RequestID string
	// Message is the server's human-readable text, as sent: untrusted, and
	// only worth showing for a code the caller has no wording of its own for.
	Message string
	// RetryAfter is the response's Retry-After, 0 if it had none: how long
	// until a rate limit or a daily quota lets the request through.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("server returned %d (%s)", e.Status, e.Code)
}

// What a NetworkError was doing when it failed.
const (
	OpRequest  = "request"  // a call to the API server
	OpUpload   = "upload"   // the PUT to object storage
	OpDownload = "download" // the GET from object storage
)

// NetworkError means no HTTP answer came back: the host couldn't be reached,
// the connection broke, or the request timed out. Err never holds a
// presigned URL.
type NetworkError struct {
	Op     string // OpRequest, OpUpload, or OpDownload
	Server string // the API server this client talks to
	Err    error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("%s failed (server %s): %v", e.Op, e.Server, e.Err)
}

func (e *NetworkError) Unwrap() error { return e.Err }

// StorageError means object storage answered a transfer with something other
// than success, for example a download URL whose object was deleted.
type StorageError struct {
	Op     string // OpUpload or OpDownload
	Status int
}

func (e *StorageError) Error() string {
	return fmt.Sprintf("%s returned %d", e.Op, e.Status)
}

// Client talks to one StashVent server.
type Client struct {
	baseURL   string
	userAgent string
	api       *http.Client
	transfer  *http.Client
}

// New returns a Client for server (e.g. "http://localhost:8080").
func New(server, version string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(server, "/"),
		userAgent: "stashvent/" + version,
		api:       &http.Client{Timeout: apiTimeout, CheckRedirect: noRedirect},
		transfer:  &http.Client{Timeout: transferTimeout},
	}
}

// noRedirect makes the API client return a 3xx as it is. The API never
// redirects, and net/http would carry the delete token header along to
// another host: it only strips Authorization and cookies. Not following
// keeps the API key on its server too, whatever net/http strips.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// Only CreateShare and DeleteShare take an API key: the key is sent on push
// and delete and nowhere else. The caller decides whether this
// client's server is one the key may go to; "" sends none.

// CreateShare registers a pending share and returns the upload URL and
// delete token. apiKey, if not "", identifies the sender.
func (c *Client) CreateShare(ctx context.Context, req api.CreateShareRequest, apiKey string) (api.CreateShareResponse, error) {
	var resp api.CreateShareResponse
	err := c.do(ctx, http.MethodPost, "/v1/shares", credentials{apiKey: apiKey}, req, &resp)
	return resp, err
}

// CompleteShare marks an uploaded share active. Only the delete token can.
func (c *Client) CompleteShare(ctx context.Context, shareID, deleteToken string) error {
	return c.do(ctx, http.MethodPost, "/v1/shares/"+shareID+"/complete", credentials{deleteToken: deleteToken}, nil, nil)
}

// GetShare fetches share metadata and a download URL. It sends no
// credentials: the server is whatever host a link named.
func (c *Client) GetShare(ctx context.Context, shareID string) (api.GetShareResponse, error) {
	var resp api.GetShareResponse
	err := c.do(ctx, http.MethodGet, "/v1/shares/"+shareID, credentials{}, nil, &resp)
	return resp, err
}

// DeleteShare revokes a share, pending or active, with its delete token or
// the API key that created it (either may be ""). A share that is unknown,
// expired, or already deleted is an *Error with api.CodeShareNotFound.
func (c *Client) DeleteShare(ctx context.Context, shareID, deleteToken, apiKey string) error {
	return c.do(ctx, http.MethodDelete, "/v1/shares/"+shareID, credentials{deleteToken: deleteToken, apiKey: apiKey}, nil, nil)
}

// credentials are what a request may carry to prove who sent it.
type credentials struct {
	deleteToken string // X-Delete-Token
	apiKey      string // Authorization: Bearer
}

// Upload PUTs body to a presigned URL with the headers the server signed
// into it (api.CreateShareResponse.UploadHeaders). The URL signs the exact
// length. Any 2xx is success: S3-compatible stores answer 200, 201, or 204.
func (c *Client) Upload(ctx context.Context, uploadURL string, headers map[string]string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building upload request: %w", err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.transfer.Do(req)
	if err != nil {
		return &NetworkError{Op: OpUpload, Server: c.baseURL, Err: redactURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StorageError{Op: OpUpload, Status: resp.StatusCode}
	}
	return nil
}

// Download GETs a presigned URL and returns exactly size bytes. The body is
// held in memory and size comes from the server, so it must be one a share
// can have: more than the GCM tag, at most api.DefaultMaxPayloadBytes.
func (c *Client) Download(ctx context.Context, downloadURL string, size int64) ([]byte, error) {
	if size <= crypto.TagSize || size > api.DefaultMaxPayloadBytes {
		return nil, fmt.Errorf("refusing to download %d bytes: not a size a share can have", size)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building download request: %w", err)
	}
	resp, err := c.transfer.Do(req)
	if err != nil {
		return nil, &NetworkError{Op: OpDownload, Server: c.baseURL, Err: redactURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &StorageError{Op: OpDownload, Status: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, &NetworkError{Op: OpDownload, Server: c.baseURL, Err: redactURL(err)}
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("downloaded %d bytes, want %d", len(data), size)
	}
	return data, nil
}

func (c *Client) do(ctx context.Context, method, path string, creds credentials, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if creds.deleteToken != "" {
		req.Header.Set(api.DeleteTokenHeader, creds.deleteToken)
	}
	if creds.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+creds.apiKey)
	}

	resp, err := c.api.Do(req)
	if err != nil {
		return &NetworkError{Op: OpRequest, Server: c.baseURL, Err: redactURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		apiErr := &Error{
			Status:     resp.StatusCode,
			Server:     c.baseURL,
			RequestID:  resp.Header.Get("X-Request-Id"),
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
		var e api.ErrorResponse
		if json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&e) == nil {
			apiErr.Code = e.Error.Code
			apiErr.Message = e.Error.Message
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// retryAfter reads a Retry-After value in whole seconds, the only form the
// server sends; anything else is 0.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// redactURL strips the presigned URL (whose query string is a credential)
// from *url.Error messages.
func redactURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// CheckServerURL rejects plain http:// except for localhost (local dev).
func CheckServerURL(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid server URL %q", server)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host := u.Hostname(); host == "localhost" || net.ParseIP(host).IsLoopback() {
			return nil
		}
		return fmt.Errorf("refusing insecure http:// server %q; use https://", server)
	default:
		return fmt.Errorf("invalid server URL %q", server)
	}
}

// NormalizeServer checks a server URL (see CheckServerURL) and returns the
// one form it is used and stored in: scheme, lower-case host, optional port.
// A path, query, fragment, or user name is refused: a share link is the
// server plus "/<id>", and ParseLink reads it back that way.
func NormalizeServer(server string) (string, error) {
	if err := CheckServerURL(server); err != nil {
		return "", err
	}
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("invalid server URL %q", server)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		// The URL isn't repeated: it may hold a user name and password.
		return "", errors.New("the server URL must be only a scheme and a host, like https://stash.example.com")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// ParseLink accepts a share link (https://host/<id>) or a bare share ID. For
// a link, the server is the link's origin; for a bare ID, defaultServer.
func ParseLink(link, defaultServer string) (server, shareID string, err error) {
	// IDs longer than today's are accepted (crypto.ValidLinkShareID), so a
	// server can lengthen its IDs without breaking this CLI.
	if crypto.ValidLinkShareID(link) {
		return defaultServer, link, nil
	}
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("%q is not a share link or ID", link)
	}
	id := strings.Trim(u.Path, "/")
	if !crypto.ValidLinkShareID(id) {
		return "", "", fmt.Errorf("%q is not a share link or ID", link)
	}
	return u.Scheme + "://" + u.Host, id, nil
}
