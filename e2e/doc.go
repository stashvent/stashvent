// Package e2e holds the end-to-end tests that run the built binaries,
// bin/stashvent and bin/stashvent-server, as separate processes against a
// server that is already running with API keys on.
//
// The tests are behind the e2e build tag. Run them with
//
//	make run-server-e2e   # in one terminal: the server with the e2e settings
//	make test-e2e         # in another: builds the binaries, runs the tests
//
// Each CLI process gets a "machine" of its own: a temp directory that is its
// home, config directory, and temp directory, so nothing of the developer's
// is read or written. The tests reach Postgres and object storage directly
// (DATABASE_URL, S3_*) only to check what the binaries did and to clean up.
package e2e
