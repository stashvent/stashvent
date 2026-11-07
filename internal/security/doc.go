// Package security scans stash diffs for likely secrets before they are
// shared, using the gitleaks default rules.
//
// Share IDs and delete tokens are generated in internal/crypto.
package security
