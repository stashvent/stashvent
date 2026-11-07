package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/git"
	"github.com/stashvent/stashvent/internal/payload"
	"github.com/stashvent/stashvent/internal/security"
	"github.com/stashvent/stashvent/internal/userfiles"
)

// Limits of the secret-scan warning.
const (
	maxFindingsShown = 20  // findings listed; the rest is a count
	maxPathLen       = 200 // runes of a file path
)

const (
	// maxStashesShown is how many stashes the picker lists.
	maxStashesShown = 20
	// maxPickAttempts is how many answers the picker takes before giving up.
	maxPickAttempts = 3
	// cleanupTimeout bounds the DELETE that removes a share whose push didn't
	// finish. It runs after Ctrl+C, so it must not hold the user up.
	cleanupTimeout = 5 * time.Second
)

// expiresLabel words each --expires value for the result.
var expiresLabel = map[string]string{
	"1h":  "1 hour",
	"6h":  "6 hours",
	"24h": "24 hours",
	"7d":  "7 days",
}

// passwordFlag is --password. It takes no value: a password on the command
// line ends up in the shell history and the process list. It still accepts
// whatever is attached to it (--password=…) and only notes that something
// was, so that no error message can repeat it.
type passwordFlag struct {
	set      bool
	hasValue bool
}

func (f *passwordFlag) String() string { return strconv.FormatBool(f.set) }

func (f *passwordFlag) Set(value string) error {
	f.set = true
	f.hasValue = value != "true" // "true" is what a bare --password passes
	return nil
}

func (f *passwordFlag) Type() string { return "bool" }

// IsBoolFlag lets the flag stand alone, without a value after it.
func (f *passwordFlag) IsBoolFlag() bool { return true }

type pushFlags struct {
	ignoreSecrets bool
	password      passwordFlag
	passwordStdin bool
	expires       string
}

// errPasswordOnCommandLine is deliberately silent about what was typed.
var errPasswordOnCommandLine = errors.New("a password is never taken from the command line: " +
	"--password asks for it, --password-stdin reads it from stdin")

// check validates the flags and returns the stash to share, "" for none
// named.
func (f *pushFlags) check(args []string) (ref string, err error) {
	if len(args) == 1 {
		ref, err = git.ParseStashRef(args[0])
	}
	switch {
	case f.password.hasValue, err != nil && (f.password.set || f.passwordStdin):
		// "--password=hunter2", or "--password hunter2", where the password
		// arrives as the stash argument. Neither may be repeated on screen.
		return "", usageError{errPasswordOnCommandLine}
	case err != nil:
		return "", usageError{err}
	case f.password.set && f.passwordStdin:
		return "", usageError{errors.New("use --password or --password-stdin, not both")}
	}
	if _, ok := api.ExpiresIn[f.expires]; !ok {
		return "", usageError{fmt.Errorf("--expires must be 1h, 6h, 24h, or 7d, not %q", f.expires)}
	}
	return ref, nil
}

func (a *app) pushCommand() *cobra.Command {
	var flags pushFlags
	cmd := &cobra.Command{
		Use:   "push [stash]",
		Short: "Encrypt and share a stash",
		Long: `Package a stash, encrypt it on this machine, and upload it.

Without an argument the newest stash (stash@{0}) is shared; with several
stashes and a terminal, you are asked which one. Name one as stash@{N} or
just N. The stash stays in your own stash list.

Before anything is encrypted or sent, the stash is scanned for likely secrets
(keys, tokens, passwords). If any are found you are asked whether to go on.

A password is generated and printed once, with the link. Send the two through
different channels. With --password you choose your own at a hidden prompt; a
password is never taken from the command line.

A server that requires an API key needs "stashvent login" first (or
STASHVENT_API_KEY). The key is sent to that server only.`,
		Example: `  # The newest stash, with a generated password
  stashvent push

  # stash@{2}, kept for a day instead of 6 hours
  stashvent push 2 --expires 24h

  # Choose the password yourself (hidden prompt)
  stashvent push --password

  # In a script: the password comes from stdin
  echo "$PASSWORD" | stashvent push --password-stdin`,
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.push(cmd.Context(), args, &flags)
		},
	}
	cmd.Flags().BoolVar(&flags.ignoreSecrets, "ignore-secrets", false, "share even if the secret scan finds something")
	password := cmd.Flags().VarPF(&flags.password, "password", "", "choose the password yourself, at a hidden prompt")
	password.NoOptDefVal = "true"
	cmd.Flags().BoolVar(&flags.passwordStdin, "password-stdin", false, "read a password of your own from stdin")
	cmd.Flags().StringVar(&flags.expires, "expires", api.DefaultExpiresIn, "how long the share lives: 1h, 6h, 24h, or 7d")
	return cmd
}

func (a *app) push(ctx context.Context, args []string, flags *pushFlags) error {
	ref, err := flags.check(args)
	if err != nil {
		return err
	}
	server, err := a.serverURL()
	if err != nil {
		return err
	}
	// The server decides where the work goes, and an environment variable
	// can change it, so anything but the default is said out loud.
	if server != defaultServer {
		a.info("Server: %s (from %s)", server, a.serverFrom)
	}
	// Read the history now: if it can't be used, the push stops before
	// anything leaves this machine.
	history, err := a.history()
	if err != nil {
		return err
	}
	if _, err := history.Load(a.now()); err != nil {
		return err
	}
	// The key goes to the server this push was sent to, the one just
	// resolved, and to no other.
	apiKey, keyFromEnv, err := a.apiKey(server)
	if err != nil {
		return err
	}
	if keyFromEnv {
		a.info("API key: from %s", apiKeyEnv)
	}

	// Stdin holds the password and nothing else, so it is read before
	// anything could ask a question there.
	var password string
	if flags.passwordStdin {
		if password, err = a.stdinPassword(ctx); err != nil {
			return err
		}
		if problem := passwordProblem(password); problem != "" {
			return userError{problem}
		}
	}

	gitVersion, err := git.CheckVersion(ctx)
	if err != nil {
		return err
	}
	cwd, err := a.workDir()
	if err != nil {
		return err
	}
	repo, err := git.RepoRoot(ctx, cwd)
	if err != nil {
		return userError{"Not inside a Git repository."}
	}
	objectFormat, err := git.ObjectFormat(ctx, repo)
	if err != nil {
		return err
	}

	entry, err := a.chooseStash(ctx, repo, ref)
	if err != nil {
		return err
	}

	// Packaging comes before the scan, quietly: it sends nothing, and a
	// stash too large to send then says so, instead of failing the scan on
	// its size and being told to turn the scan off.
	stash, err := a.bundleStash(ctx, repo, entry)
	if err != nil {
		return err
	}
	repoHash, err := git.RepoHash(ctx, repo, stash.info.Base)
	if err != nil {
		return err
	}

	// Descriptive details travel only inside the encrypted payload. A stash
	// imported before carries "Shared from <sender> — " in its message; the
	// manifest gets git's own message, and this sender is named instead.
	message := withoutSharedPrefix(stash.message)
	plaintext, err := payload.Encode(payload.Manifest{
		StashMessage: message,
		BaseCommit:   stash.info.Base,
		BundleRef:    stash.info.Ref,
		SenderName:   git.SenderName(ctx, repo),
		CreatedAt:    a.now().UTC(),
		RepoName:     git.RepoName(ctx, repo),
		RepoHash:     repoHash,
		Branch:       git.StashBranch(message),
		FileCount:    stash.files,
	}, stash.bundle)
	if err != nil {
		return err
	}
	// The encrypted size is known before encrypting, so a stash that is too
	// large stops here, before a password is asked for.
	if size := int64(len(plaintext)) + crypto.TagSize; size > a.maxPayload {
		return a.tooLarge(size, entry)
	}
	// The server doesn't check bundle_version or object_format, so this is
	// what makes sure a share can be pulled: by this version and later ones.
	if !payload.KnownFormat(api.PayloadFormatV1, api.PackagingStashRef, stash.version, objectFormat) {
		return userError{fmt.Sprintf("This version of stashvent can't share from this repository: its object format is %q (bundle v%d).",
			objectFormat, stash.version)}
	}
	aad, err := payload.AssociatedData(api.PayloadFormatV1, api.PackagingStashRef, stash.version, objectFormat)
	if err != nil {
		return err
	}

	// Scan before anything is encrypted or sent. A scan that fails stops the
	// push, unless the sender already chose to ignore it.
	findings, err := a.scan(ctx, repo, entry)
	switch {
	case err != nil && ctx.Err() != nil:
		return err
	case err != nil && !flags.ignoreSecrets:
		return userError{fmt.Sprintf("The secret scan failed, so nothing was shared: %s.\n"+
			"Pass --ignore-secrets to share without the scan.", scanFailure(err))}
	case err != nil:
		a.info("⚠ Secret scan failed: %s. Continuing (--ignore-secrets).", scanFailure(err))
	default:
		if err := a.secretsGate(ctx, entry, findings, flags.ignoreSecrets); err != nil {
			return err
		}
	}
	// Name the stash so sharing a different one than intended is easy to spot.
	a.info("✓ Packaged %s: %s (%s)", entry.Ref, entry.Message, plural(stash.files, "file"))

	ownPassword := flags.password.set || flags.passwordStdin
	switch {
	case flags.password.set:
		password, err = a.askNewPassword(ctx)
	case !flags.passwordStdin:
		password, err = a.newPassword()
	}
	if err != nil {
		return err
	}
	salt, err := crypto.NewSalt()
	if err != nil {
		return err
	}
	key, err := crypto.DeriveKey(password, salt)
	if err != nil {
		return err
	}
	ciphertext, nonce, err := crypto.Encrypt(key, plaintext, aad)
	if err != nil {
		return err
	}
	a.info("✓ Encrypted locally")

	client := apiclient.New(server, a.version)
	created, err := client.CreateShare(ctx, api.CreateShareRequest{
		Size:             int64(len(ciphertext)),
		ExpiresIn:        flags.expires,
		Salt:             salt,
		IV:               nonce,
		PayloadFormat:    api.PayloadFormatV1,
		PackagingMethod:  api.PackagingStashRef,
		BundleVersion:    stash.version,
		ObjectFormat:     objectFormat,
		SenderGitVersion: gitVersion,
	}, apiKey)
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) && apiErr.Code == api.CodePayloadTooLarge {
		return userError{fmt.Sprintf("This stash is %s once encrypted, which is more than this server accepts.",
			formatSize(int64(len(ciphertext))))}
	}
	if err != nil {
		return err
	}

	// The share now exists on the server. Unless the push gets to the point
	// where the sender has the link, it is taken away again: after a failure
	// or Ctrl+C nobody was told the link, and the history may not hold it.
	finished, recorded := false, false
	defer func() {
		if !finished {
			a.abandonShare(client, history, created, recorded)
		}
	}()

	if err := client.Upload(ctx, created.UploadURL, created.UploadHeaders, ciphertext); err != nil {
		return err
	}
	if err := client.CompleteShare(ctx, created.ShareID, created.DeleteToken); err != nil {
		return err
	}
	a.info("✓ Uploaded")

	link := server + "/" + created.ShareID
	// Recorded before the link is shown: a share the sender knows about must
	// be one they can delete. Never the password.
	err = history.Add(a.now(), userfiles.Share{
		ShareID:      created.ShareID,
		Server:       server,
		Link:         link,
		StashMessage: stash.message,
		CreatedAt:    a.now().UTC(),
		ExpiresAt:    created.ExpiresAt.UTC(),
		DeleteToken:  created.DeleteToken,
	})
	if err != nil {
		return err
	}
	recorded = true

	// A generated password exists nowhere else, so a failed write must be an
	// error. A typed one is never shown.
	if err := a.printShare(link, password, ownPassword, created.ExpiresAt, flags.expires); err != nil {
		return fmt.Errorf("printing share details: %w", err)
	}
	finished = true
	if ownPassword {
		a.info("The password is the one you chose. Send it through a different channel than the link.")
	} else {
		a.info("Send the password through a different channel than the link.")
	}
	return nil
}

// printShare writes the result of a push to stdout.
func (a *app) printShare(link, password string, ownPassword bool, expiresAt time.Time, expiresIn string) error {
	var out strings.Builder
	fmt.Fprintf(&out, "Share link: %s\n", link)
	if !ownPassword {
		fmt.Fprintf(&out, "Password:   %s\n", password)
	}
	fmt.Fprintf(&out, "Expires:    %s (in %s)\n", expiresAt.In(a.loc).Format("2006-01-02 15:04 MST"), expiresLabel[expiresIn])
	_, err := io.WriteString(a.stdout, out.String())
	return err
}

// abandonShare deletes a share whose push did not finish. It has its own
// short deadline, because the command's context is already cancelled after
// Ctrl+C. If the delete fails, the share is harmless: without /complete
// nobody can download it and the server removes it within about an hour, and
// with it, its link was never shown.
func (a *app) abandonShare(client *apiclient.Client, history *userfiles.History, share api.CreateShareResponse, recorded bool) {
	if recorded {
		_ = history.Remove(a.now(), share.ShareID) // the entry would point at a deleted share
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	err := client.DeleteShare(ctx, share.ShareID, share.DeleteToken, "")
	var apiErr *apiclient.Error
	if err == nil || (errors.As(err, &apiErr) && apiErr.Code == api.CodeShareNotFound) {
		a.info("The unfinished share was removed from the server.")
		return
	}
	a.info("⚠ The unfinished share could not be removed from the server. Its link was never shown,\n" +
		"  and the server deletes it on its own.")
}

// stdinPassword reads a password from the first line of stdin.
func (a *app) stdinPassword(ctx context.Context) (string, error) {
	line, err := a.readLine(ctx)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || line == "" {
		return "", userError{"No password on stdin."}
	}
	return line, nil
}

// passwordProblem says what is wrong with a password the sender chose, or ""
// if it can be used. Only the length is checked: anyone with the link can
// download the ciphertext and guess offline, so a short password would make
// the link alone enough.
func passwordProblem(password string) string {
	if utf8.RuneCountInString(password) < crypto.MinPasswordLength {
		return fmt.Sprintf("The password must be at least %d characters.", crypto.MinPasswordLength)
	}
	return ""
}

// askNewPassword asks for the sender's own password, twice: a typo would
// make a share nobody can decrypt.
func (a *app) askNewPassword(ctx context.Context) (string, error) {
	for attempt := 1; ; attempt++ {
		password, err := a.promptSecret(ctx, "Password: ", "--password-stdin")
		if err != nil {
			return "", err
		}
		problem := passwordProblem(password)
		if problem == "" {
			repeated, err := a.promptSecret(ctx, "Repeat password: ", "--password-stdin")
			if err != nil {
				return "", err
			}
			if repeated == password {
				return password, nil
			}
			problem = "The passwords don't match."
		}
		if attempt == maxPasswordAttempts {
			return "", userError{problem}
		}
		a.info("✗ %s Try again (%d of %d).", problem, attempt+1, maxPasswordAttempts)
	}
}

// chooseStash returns the stash to share: the one named, the only one, the
// one picked at the prompt, or, without a terminal, the newest.
func (a *app) chooseStash(ctx context.Context, repo, ref string) (git.Stash, error) {
	stashes, err := git.ListStashes(ctx, repo)
	if err != nil {
		return git.Stash{}, err
	}
	if len(stashes) == 0 {
		return git.Stash{}, userError{`No stash to share. Run "git stash" first.`}
	}
	if ref != "" {
		i := slices.IndexFunc(stashes, func(s git.Stash) bool { return s.Ref == ref })
		if i < 0 {
			return git.Stash{}, userError{ref + ` doesn't exist. Run "git stash list" to see your stashes.`}
		}
		return stashes[i], nil
	}
	if len(stashes) == 1 || !a.isTerminal() {
		return stashes[0], nil
	}
	return a.pickStash(ctx, stashes)
}

// pickStash lists the stashes and asks which one to share. The numbers are
// git's own, so the answer means the same as in "stashvent push N".
func (a *app) pickStash(ctx context.Context, stashes []git.Stash) (git.Stash, error) {
	a.info("Stashes in this repository:\n")
	shown := stashes[:min(len(stashes), maxStashesShown)]
	width := len(strconv.Itoa(len(shown) - 1))
	for i, s := range shown {
		a.info("  %*d  %s", width, i, cleanText(s.Message, maxStashMessageLen))
	}
	if more := len(stashes) - len(shown); more > 0 {
		a.info("  …and %d older", more)
	}
	a.info("")

	last := len(stashes) - 1
	for attempt := 1; ; attempt++ {
		_, _ = fmt.Fprint(a.stderr, "Stash to share [0]: ") // no newline, so not a.info
		line, err := a.readLine(ctx)
		if ctx.Err() != nil {
			a.info("")
			return git.Stash{}, ctx.Err()
		}
		if err != nil { // the end of input: nothing was chosen
			a.info("")
			return git.Stash{}, errCancelled
		}
		answer := strings.TrimSpace(line)
		if answer == "" {
			return stashes[0], nil
		}
		if ref, err := git.ParseStashRef(answer); err == nil {
			if i := slices.IndexFunc(stashes, func(s git.Stash) bool { return s.Ref == ref }); i >= 0 {
				return stashes[i], nil
			}
		}
		if attempt == maxPickAttempts {
			return git.Stash{}, userError{"No stash was chosen."}
		}
		a.info("✗ Enter a number from 0 to %d.", last)
	}
}

// scanStash looks for likely secrets in what the stash adds to its base
// commit: working tree, untracked files, and index.
func scanStash(ctx context.Context, repo string, stash git.Stash) ([]security.Finding, error) {
	patch, err := git.StashPatch(ctx, repo, stash.SHA)
	if err != nil {
		return nil, err
	}
	scanner, err := security.NewScanner()
	if err != nil {
		return nil, err
	}
	return scanner.Scan(ctx, patch.Worktree, patch.Index)
}

// scanFailure says why the scan didn't run, so the sender has more to go on
// than turning it off. Git errors carry git's own message, never diff content.
func scanFailure(err error) string {
	switch {
	case errors.Is(err, git.ErrPatchTooLarge):
		return fmt.Sprintf("the stash has more than %d MB of changes", git.MaxPatchBytes>>20)
	case errors.Is(err, security.ErrUnreadablePatch):
		return "git's diff of the stash could not be read"
	default:
		return strings.TrimSuffix(err.Error(), ".")
	}
}

// cleanPath is cleanText for a file path. A path that is too long loses its
// start, because the file name at the end is what tells two findings apart.
func cleanPath(path string) string {
	r := []rune(cleanText(path, len(path))) // len is in bytes, so nothing is cut here
	if len(r) > maxPathLen {
		return "…" + string(r[len(r)-maxPathLen:])
	}
	return string(r)
}

// stashNumber returns the N of a "stash@{N}". Commands shown to the user
// take the bare number, because PowerShell mangles stash@{N}.
func stashNumber(ref string) string {
	return strings.TrimSuffix(strings.TrimPrefix(ref, "stash@{"), "}")
}

// secretsGate reports the scan result and decides whether the push goes on.
// Findings are a warning, never a silent change to the stash: the sender
// confirms, or passes --ignore-secrets. Without a terminal the push is
// cancelled; no flag but --ignore-secrets gets past it (stashvent-cli
// skill). Only the file, line, and rule are shown, never the secret.
func (a *app) secretsGate(ctx context.Context, stash git.Stash, findings []security.Finding, ignoreSecrets bool) error {
	if len(findings) == 0 {
		a.info("✓ Secret scan passed")
		return nil
	}

	a.info("⚠ Potential secrets detected in %s: %s\n", stash.Ref, stash.Message)
	shown := findings[:min(len(findings), maxFindingsShown)]
	places := make([]string, len(shown))
	width := 0
	for i, f := range shown {
		places[i] = cleanPath(f.File)
		if f.Line > 0 {
			places[i] += fmt.Sprintf(":%d", f.Line)
		}
		width = max(width, utf8.RuneCountInString(places[i]))
	}
	for i, f := range shown {
		rule := f.RuleID
		if f.Staged {
			rule += "  (staged)"
		}
		a.info("  %-*s  %s", width, places[i], rule)
	}
	if more := len(findings) - len(shown); more > 0 {
		a.info("  …and %d more", more)
	}
	a.info("\nSecret values are not shown. To review: git stash show -p --include-untracked %s", stashNumber(stash.Ref))
	// That command leaves out what is only in the index. Quoted, so that
	// PowerShell keeps the braces and cmd.exe the caret.
	if slices.ContainsFunc(findings, func(f security.Finding) bool { return f.Staged }) {
		a.info(`The ones marked (staged) are only in the index: git show "%s^2"`, stash.Ref)
	}

	switch {
	case ignoreSecrets:
		a.info("Continuing (--ignore-secrets).")
		return nil
	case !a.isTerminal():
		a.info("Remove them, or pass --ignore-secrets to share anyway.")
		return errCancelled
	}
	ok, err := a.confirm(ctx, "Share anyway?")
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	return nil
}

// tooLarge is the error for a stash over the size limit.
func (a *app) tooLarge(size int64, stash git.Stash) error {
	return userError{fmt.Sprintf("This stash is %s once packaged, and the limit is %s.\n"+
		"Large or binary files are the usual cause. To see what it holds: git stash show --stat --include-untracked %s",
		sizeOver(size, a.maxPayload), formatSize(a.maxPayload), stashNumber(stash.Ref))}
}

// sizeOver formats a size that is over limit. Rounded, a size just over the
// limit would read the same as the limit ("5 MB, and the limit is 5 MB").
func sizeOver(size, limit int64) string {
	if s := formatSize(size); s != formatSize(limit) {
		return s
	}
	return "just over " + formatSize(limit)
}

// formatSize writes a byte count the way people say it: "812 bytes",
// "342 KB", "1.8 MB", "5 MB".
func formatSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+512)>>10)
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/(1<<20)), ".0") + " MB"
	}
}

// packagedStash is a stash as a bundle, plus what the manifest needs.
type packagedStash struct {
	bundle  []byte
	version int // bundle format version (2, or 3 for sha256 repos)
	info    git.BundleInfo
	message string
	files   int
}

// bundleStash writes the stash to a temp bundle and reads it back. The temp
// file is removed on every exit path.
func (a *app) bundleStash(ctx context.Context, repo string, stash git.Stash) (packagedStash, error) {
	files, err := git.FileCount(ctx, repo, stash.SHA)
	if err != nil {
		return packagedStash{}, err
	}

	f, err := os.CreateTemp("", "stashvent-*.bundle")
	if err != nil {
		return packagedStash{}, fmt.Errorf("creating temp file: %w", err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }() // best effort; the OS temp dir is per user
	if err := f.Close(); err != nil {      // git writes the file; Windows can't remove open files
		return packagedStash{}, err
	}

	info, err := git.CreateStashBundle(ctx, repo, stash, path)
	if err != nil {
		return packagedStash{}, err
	}
	// A bundle over the limit is not read into memory at all. The exact
	// check, with the manifest, comes once the payload is built.
	if stat, err := os.Stat(path); err == nil && stat.Size() > a.maxPayload {
		return packagedStash{}, a.tooLarge(stat.Size(), stash)
	}
	version, err := git.BundleVersion(path)
	if err != nil {
		return packagedStash{}, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is our own os.CreateTemp file
	if err != nil {
		return packagedStash{}, fmt.Errorf("reading bundle: %w", err)
	}
	return packagedStash{bundle: data, version: version, info: info, message: stash.Message, files: files}, nil
}

// plural formats n with noun, adding "s" unless n is 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
