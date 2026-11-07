package git

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// run executes git with separate arguments (never through a shell) in dir and
// returns trimmed stdout. stderr is included in the error.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	var stdout bytes.Buffer
	if err := runTo(ctx, dir, &stdout, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runTo is run with stdout written to w as it is, for output that is large
// or where whitespace matters.
func runTo(ctx context.Context, dir string, w io.Writer, args ...string) error {
	// G204: args are passed as separate argv entries (no shell), and
	// user-supplied refs and SHAs are validated before they get here.
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		name := "git"
		for i := 0; i < len(args); i++ {
			if args[i] == "-c" { // skip `-c key=value` to name the subcommand
				i++
				continue
			}
			name += " " + args[i]
			break
		}
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RepoRoot returns the top-level directory of the repository containing dir.
func RepoRoot(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "rev-parse", "--show-toplevel")
}

// MinVersion is the oldest Git that push and pull support: the
// floor comes from `git stash show --include-untracked`.
const MinVersion = "2.32"

// VersionError reports a Git older than MinVersion.
type VersionError struct {
	Have string // the installed version, e.g. "2.25.1"
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("git %s is older than the minimum %s", e.Have, MinVersion)
}

// Version returns the version string reported by `git --version`, e.g.
// "2.32.0.windows.2".
func Version(ctx context.Context) (string, error) {
	out, err := run(ctx, "", "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(out, "git version "), nil
}

// CheckVersion returns the installed Git version, or a *VersionError when it
// is older than MinVersion. A version it can't parse passes, so unusual
// builds aren't blocked.
func CheckVersion(ctx context.Context) (string, error) {
	have, err := Version(ctx)
	if err != nil {
		return "", err
	}
	if !versionAtLeast(have, MinVersion) {
		return have, &VersionError{Have: have}
	}
	return have, nil
}

var versionRE = regexp.MustCompile(`^(\d+)\.(\d+)`)

// parseVersion reads the leading major.minor of a Git version string such as
// "2.32.0.windows.2" or "2.39.3 (Apple Git-145)".
func parseVersion(s string) (major, minor int, ok bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	return major, minor, err1 == nil && err2 == nil
}

// versionAtLeast reports whether have >= want; an unparseable have passes.
func versionAtLeast(have, want string) bool {
	hMajor, hMinor, ok := parseVersion(have)
	if !ok {
		return true
	}
	wMajor, wMinor, _ := parseVersion(want)
	return hMajor > wMajor || (hMajor == wMajor && hMinor >= wMinor)
}

// ObjectFormat returns the repository's hash algorithm ("sha1" or "sha256").
func ObjectFormat(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "rev-parse", "--show-object-format")
}

var (
	// ErrNoStash is returned when the repository has no stash entries.
	ErrNoStash = errors.New("no stash entries")
	// ErrStashNotFound is returned when a stash@{N} is beyond the stash list.
	ErrStashNotFound = errors.New("stash entry not found")
)

// Stash is one entry of `git stash list`.
type Stash struct {
	Ref     string // "stash@{N}"
	SHA     string // the stash commit
	Message string // e.g. "On main: wip"
}

// ListStashes returns the stash list, newest first. It is empty, not an
// error, when there are no stashes.
func ListStashes(ctx context.Context, dir string) ([]Stash, error) {
	out, err := run(ctx, dir, "stash", "list", "--format=%gd%x00%H%x00%gs")
	if err != nil {
		return nil, err
	}
	var stashes []Stash
	for line := range strings.Lines(out) {
		f := strings.SplitN(strings.TrimRight(line, "\r\n"), "\x00", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected stash list line %q", line)
		}
		stashes = append(stashes, Stash{Ref: f[0], SHA: f[1], Message: f[2]})
	}
	return stashes, nil
}

var stashRefRE = regexp.MustCompile(`^(?:stash@\{(\d+)\}|(\d+))$`)

// ParseStashRef accepts "stash@{N}" or a bare "N" (PowerShell mangles
// unquoted braces) and returns the canonical "stash@{N}". Anything else is
// rejected, so user input never reaches git as an option or arbitrary rev.
func ParseStashRef(s string) (string, error) {
	m := stashRefRE.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("%q is not a stash; use stash@{N} or N", s)
	}
	n, err := strconv.Atoi(m[1] + m[2]) // exactly one group matched
	if err != nil {
		return "", fmt.Errorf("%q is not a stash; use stash@{N} or N", s)
	}
	return fmt.Sprintf("stash@{%d}", n), nil
}

// FindStash returns the stash entry ref (as returned by ParseStashRef).
func FindStash(ctx context.Context, dir, ref string) (Stash, error) {
	stashes, err := ListStashes(ctx, dir)
	if err != nil {
		return Stash{}, err
	}
	if len(stashes) == 0 {
		return Stash{}, ErrNoStash
	}
	for _, s := range stashes {
		if s.Ref == ref {
			return s, nil
		}
	}
	return Stash{}, ErrStashNotFound
}

// StashBranch returns the branch a stash was made on, parsed from its message
// ("WIP on main: …" or "On main: …"), or "" when detached or unknown.
func StashBranch(message string) string {
	for _, prefix := range []string{"WIP on ", "On "} {
		rest, ok := strings.CutPrefix(message, prefix)
		if !ok {
			continue
		}
		// Ref names can't contain ':', so the first one ends the branch.
		branch, _, ok := strings.Cut(rest, ":")
		if !ok || branch == "(no branch)" {
			return ""
		}
		return branch
	}
	return ""
}

// FileCount returns how many files a stash touches, including untracked ones.
func FileCount(ctx context.Context, dir, sha string) (int, error) {
	out, err := run(ctx, dir, "stash", "show", "--name-only", "-z", "--include-untracked", sha)
	if err != nil {
		return 0, err
	}
	names := map[string]bool{}
	for name := range strings.SplitSeq(out, "\x00") {
		if name != "" {
			names[name] = true
		}
	}
	return len(names), nil
}

// patchConfig and patchFlags pin the patch format, so the user's config and
// attributes (colours, an external diff program, textconv filters, prefixes,
// paths relative to a subdirectory, submodule logs) can't change it or run
// anything.
//
// patchConfig goes before the subcommand and fixes the a/ and b/ prefixes.
// It is config, not --src-prefix and --dst-prefix: given those two options,
// `git stash show` in Git 2.55 (seen on Ubuntu) prints the paths with no
// prefix at all. Git older than 2.45 ignores the last two keys.
var patchConfig = []string{
	"-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false",
	"-c", "diff.srcPrefix=a/", "-c", "diff.dstPrefix=b/",
}

// patchFlags go after the subcommand.
//
//   - --text: a file that Git or .gitattributes calls binary (`-diff`, UTF-16
//     text) would otherwise be one "Binary files differ" line with nothing in
//     it to scan.
//   - -U0 with --inter-hunk-context=0: only changed lines, one hunk per
//     change, which keeps reported line numbers exact.
var patchFlags = []string{
	"--text", "-U0", "--inter-hunk-context=0",
	"--no-color", "--no-ext-diff", "--no-textconv",
	"--no-relative", "--submodule=short",
}

// patchArgs builds a patch command: the pinned config, the subcommand with
// its own arguments, the pinned flags, then the revisions.
func patchArgs(subcommand []string, revs ...string) []string {
	return slices.Concat(patchConfig, subcommand, patchFlags, revs)
}

// MaxPatchBytes is the most patch text StashPatch reads. The patch is held in
// memory and scanned line by line, so a stash with a huge untracked file must
// fail here, not after gigabytes of work. Most stashes over it are also over
// the 5 MB payload limit, but text compresses well, so some fit: push packages
// the stash to tell which before it reports the scan's failure.
const MaxPatchBytes = 32 << 20

// ErrPatchTooLarge is returned by StashPatch for more than MaxPatchBytes.
var ErrPatchTooLarge = errors.New("stash diff is too large")

// Patch is everything a stash adds on top of its base commit, as Git patches
// for secret scanning.
type Patch struct {
	// Worktree is the working tree and untracked files: what
	// `git stash show -p --include-untracked` prints.
	Worktree string
	// Index is the staged changes. The bundle carries them, so a change that
	// was staged and then undone in the working tree is still shared, though
	// it is not in Worktree.
	Index string
}

// StashPatch returns the Patch of stash commit sha.
func StashPatch(ctx context.Context, dir, sha string) (Patch, error) {
	return stashPatch(ctx, dir, sha, MaxPatchBytes)
}

func stashPatch(ctx context.Context, dir, sha string, limit int) (Patch, error) {
	if !shaRE.MatchString(sha) {
		return Patch{}, fmt.Errorf("invalid stash commit %q", sha)
	}
	show := patchArgs([]string{"stash", "show", "-p", "--include-untracked"}, sha)
	worktree, err := patchOutput(ctx, dir, limit, show...)
	if err != nil {
		return Patch{}, err
	}
	diff := patchArgs([]string{"diff"}, sha+"^1", sha+"^2", "--")
	index, err := patchOutput(ctx, dir, limit-len(worktree), diff...)
	if err != nil {
		return Patch{}, err
	}
	return Patch{Worktree: worktree, Index: index}, nil
}

// patchOutput returns git's untrimmed stdout, or ErrPatchTooLarge once it
// passes limit bytes; git is then stopped instead of being read to the end.
func patchOutput(ctx context.Context, dir string, limit int, args ...string) (string, error) {
	out := &cappedBuffer{limit: limit}
	if err := runTo(ctx, dir, out, args...); err != nil {
		if out.full {
			return "", ErrPatchTooLarge
		}
		return "", err
	}
	return out.buf.String(), nil
}

// cappedBuffer is a buffer whose Write fails once it would pass limit bytes.
// The failed write makes os/exec close git's stdout, which ends git.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	full  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		c.full = true
		return 0, ErrPatchTooLarge
	}
	return c.buf.Write(p)
}

// RepoHash identifies a repository independently of clones and remotes:
// the SHA-256 of the sorted root commits reachable from rev,
// joined with newlines. The sender passes the stash's base commit, the
// recipient HEAD.
func RepoHash(ctx context.Context, dir, rev string) (string, error) {
	out, err := run(ctx, dir, "rev-list", "--max-parents=0", rev, "--")
	if err != nil {
		return "", err
	}
	roots := strings.Fields(out)
	if len(roots) == 0 {
		return "", errors.New("no root commit")
	}
	slices.Sort(roots)
	sum := sha256.Sum256([]byte(strings.Join(roots, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// RepoName returns a display name for the repository: "owner/repo" from the
// origin remote, or the folder name when there is none. Hosts, users, and
// tokens in the remote URL are never kept.
func RepoName(ctx context.Context, dir string) string {
	if u, err := run(ctx, dir, "config", "--get", "remote.origin.url"); err == nil {
		if name := repoNameFromURL(u); name != "" {
			return name
		}
	}
	return filepath.Base(dir)
}

// repoNameFromURL keeps only the last two path segments of a remote URL
// (https, ssh, scp-style git@host:owner/repo, or a local path).
func repoNameFromURL(remote string) string {
	var path string
	switch {
	case strings.Contains(remote, "://"):
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		path = u.Path
	default:
		path = strings.ReplaceAll(remote, `\`, "/")
		// scp-style "user@host:owner/repo" (or a Windows drive "E:/…"):
		// the path is after the first ':' that comes before any '/'.
		if i := strings.Index(path, ":"); i >= 0 && !strings.Contains(path[:i], "/") {
			path = path[i+1:]
		}
	}
	var segs []string
	for s := range strings.SplitSeq(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return ""
	}
	segs[len(segs)-1] = strings.TrimSuffix(segs[len(segs)-1], ".git")
	if len(segs) > 2 {
		segs = segs[len(segs)-2:]
	}
	return strings.Join(segs, "/")
}

var shaRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// HasCommit reports whether sha (from another user's manifest) names a
// commit in this repository. Anything that isn't a full SHA returns false.
func HasCommit(ctx context.Context, dir, sha string) bool {
	if !shaRE.MatchString(sha) {
		return false
	}
	_, err := run(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// IsShallow reports whether dir is a shallow clone, whose root commits are
// cut-off points rather than the real start of history.
func IsShallow(ctx context.Context, dir string) bool {
	out, err := run(ctx, dir, "rev-parse", "--is-shallow-repository")
	return err == nil && out == "true"
}

// Head returns the commit HEAD points at.
func Head(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "rev-parse", "--verify", "HEAD")
}

// BundleInfo describes a bundle made by CreateStashBundle. Both values go into
// the encrypted manifest.
type BundleInfo struct {
	Ref  string // the refs/share/<random> ref the bundle carries
	Base string // the stash's base commit, which the bundle excludes
}

// CreateStashBundle writes stash s to a bundle at path, excluding its base
// commit (which the recipient already has). A temporary refs/share/<random>
// ref is created for the bundle and always removed.
func CreateStashBundle(ctx context.Context, dir string, s Stash, path string) (BundleInfo, error) {
	if !shaRE.MatchString(s.SHA) {
		return BundleInfo{}, fmt.Errorf("invalid stash commit %q", s.SHA)
	}
	base, err := run(ctx, dir, "rev-parse", "--verify", s.SHA+"^1")
	if err != nil {
		return BundleInfo{}, err
	}

	ref, err := tempRef()
	if err != nil {
		return BundleInfo{}, err
	}
	if _, err := run(ctx, dir, "update-ref", ref, s.SHA); err != nil {
		return BundleInfo{}, err
	}
	// Remove the temp ref on every exit path, even if ctx was cancelled.
	defer func() { _, _ = run(context.WithoutCancel(ctx), dir, "update-ref", "-d", ref) }()

	if _, err := run(ctx, dir, "bundle", "create", path, ref, "^"+base); err != nil {
		return BundleInfo{}, err
	}
	return BundleInfo{Ref: ref, Base: base}, nil
}

// SenderName returns git's user.name for dir, or "" when it isn't set.
func SenderName(ctx context.Context, dir string) string {
	name, err := run(ctx, dir, "config", "user.name")
	if err != nil {
		return "" // exit status 1 means unset; the name is optional
	}
	return name
}

// ImportResult says where an imported stash ended up.
type ImportResult struct {
	AlreadyStashed bool // the commit was already in the stash list; nothing was added
	Index          int  // its stash@{N} index
}

// ImportBundle verifies the bundle at path, fetches ref (named by the
// manifest) from it, and stores the fetched commit in the stash list with
// message, unless that commit is already there. The ref comes from another
// user, so it is validated first and must be the bundle's only head.
func ImportBundle(ctx context.Context, dir, path, ref, message string) (ImportResult, error) {
	if !strings.HasPrefix(ref, "refs/share/") {
		return ImportResult{}, fmt.Errorf("unexpected bundle ref %q", ref)
	}
	if _, err := run(ctx, dir, "check-ref-format", ref); err != nil {
		return ImportResult{}, fmt.Errorf("invalid bundle ref %q", ref)
	}

	if _, err := run(ctx, dir, "bundle", "verify", path); err != nil {
		return ImportResult{}, fmt.Errorf("bundle does not apply to this repository (try `git fetch`): %w", err)
	}

	heads, err := run(ctx, dir, "bundle", "list-heads", path)
	if err != nil {
		return ImportResult{}, err
	}
	if fields := strings.Fields(heads); len(fields) != 2 || fields[1] != ref {
		return ImportResult{}, fmt.Errorf("bundle heads %q don't match the manifest", heads)
	}

	if _, err := run(ctx, dir, "fetch", "--no-tags", path, ref); err != nil {
		return ImportResult{}, err
	}
	sha, err := run(ctx, dir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return ImportResult{}, err
	}

	// `git stash store` of the commit already at stash@{0} adds nothing, and
	// elsewhere it would add a duplicate; either way, report where it is.
	stashes, err := ListStashes(ctx, dir)
	if err != nil {
		return ImportResult{}, err
	}
	for i, s := range stashes {
		if s.SHA == sha {
			return ImportResult{AlreadyStashed: true, Index: i}, nil
		}
	}
	if _, err := run(ctx, dir, "stash", "store", "-m", message, sha); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Index: 0}, nil
}

// BundleVersion reads the format version from a bundle's first line
// ("# v2 git bundle" or "# v3 git bundle"; git writes v3 for sha256 repos).
func BundleVersion(path string) (int, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the caller's own temp bundle
	if err != nil {
		return 0, fmt.Errorf("opening bundle: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only; nothing to flush

	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("reading bundle header: %w", err)
	}
	switch strings.TrimSpace(line) {
	case "# v2 git bundle":
		return 2, nil
	case "# v3 git bundle":
		return 3, nil
	default:
		return 0, fmt.Errorf("unrecognized bundle header %q", strings.TrimSpace(line))
	}
}

func tempRef() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating temp ref name: %w", err)
	}
	return "refs/share/" + hex.EncodeToString(b), nil
}
