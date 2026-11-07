package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/git"
	"github.com/stashvent/stashvent/internal/payload"
)

// maxPasswordAttempts is how many passwords an interactive pull accepts
// before giving up.
const maxPasswordAttempts = 3

func (a *app) pullCommand() *cobra.Command {
	var passwordStdin bool
	cmd := &cobra.Command{
		Use:   "pull <link>",
		Short: "Download, decrypt, and import a shared stash",
		Long: `Download a shared stash, decrypt it on this machine, and add it to your stash
list. Nothing in your working tree changes: apply it with "git stash apply"
when you want it.

Run it inside a clone of the repository the stash came from. The password is
asked for at a hidden prompt; three tries.`,
		Example: `  stashvent pull https://stashvent.example/8xK29qLm4Tz7

  # A bare share ID is looked up on your own server (--server)
  stashvent pull 8xK29qLm4Tz7

  # In a script: the password comes from stdin
  echo "$PASSWORD" | stashvent pull --password-stdin https://stashvent.example/8xK29qLm4Tz7`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A link names its own server. Only a bare ID needs the configured
			// one, so a bad --server or STASHVENT_SERVER can't break a pull
			// that never uses it.
			server, shareID, err := apiclient.ParseLink(args[0], "")
			if err != nil {
				return usageError{err}
			}
			if server == "" {
				if server, err = a.serverURL(); err != nil {
					return err
				}
			} else if err := apiclient.CheckServerURL(server); err != nil {
				return usageError{err}
			}
			ctx := cmd.Context()

			if _, err := git.CheckVersion(ctx); err != nil {
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

			// Prompt before contacting the server, so the short-lived
			// download URL is fetched right before it's used.
			password, err := a.readPassword(ctx, passwordStdin)
			if err != nil {
				return err
			}

			client := apiclient.New(server, a.version)
			share, err := client.GetShare(ctx, shareID)
			if err != nil {
				return err
			}
			if !payload.KnownFormat(share.PayloadFormat, share.PackagingMethod, share.BundleVersion, share.ObjectFormat) {
				return userError{"This share uses a newer format. Upgrade stashvent and try again."}
			}
			// The download is held in memory, and its size comes from
			// whichever server the link names. Above the limit only a newer
			// version could have pushed it; at or below the GCM tag no
			// server should have accepted it, and decrypting it can only fail.
			switch {
			case share.Size > a.maxPayload:
				return userError{fmt.Sprintf("This share is %s, more than this version of stashvent downloads (%s). Upgrade stashvent and try again.",
					sizeOver(share.Size, a.maxPayload), formatSize(a.maxPayload))}
			case share.Size <= crypto.TagSize:
				return userError{fmt.Sprintf("The server reported an invalid size for this share (%d bytes). Ask the sender to push it again.", share.Size)}
			}

			ciphertext, err := client.Download(ctx, share.DownloadURL, share.Size)
			if err != nil {
				return err
			}
			a.info("✓ Downloaded")

			// The server's format fields must match what the sender bound to
			// the ciphertext, or decryption fails.
			aad, err := payload.AssociatedData(share.PayloadFormat, share.PackagingMethod, share.BundleVersion, share.ObjectFormat)
			if err != nil {
				return userError{"Decryption failed: wrong password or corrupted share"}
			}

			// On a wrong password, prompt again and reuse the downloaded
			// ciphertext (kept in memory only). --password-stdin gets one try.
			var plaintext []byte
			for attempt := 1; ; attempt++ {
				key, err := crypto.DeriveKey(password, share.Salt)
				if err != nil {
					return err
				}
				plaintext, err = crypto.Decrypt(key, share.IV, ciphertext, aad)
				if err == nil {
					break
				}
				if passwordStdin || attempt == maxPasswordAttempts {
					return userError{"Decryption failed: wrong password or corrupted share"}
				}
				a.info("✗ Wrong password. Try again (%d of %d).", attempt+1, maxPasswordAttempts)
				if password, err = a.readPassword(ctx, false); err != nil {
					return err
				}
			}
			a.info("✓ Decrypted locally")

			// Decryption proved the password and the integrity, so a bad
			// layout here means an incompatible or broken sender.
			manifest, bundle, err := payload.Decode(plaintext)
			if err != nil {
				return userError{"This share is corrupted or was created by an incompatible stashvent version."}
			}

			if err := a.verifyRepository(ctx, repo, manifest); err != nil {
				return err
			}

			res, err := importBundle(ctx, repo, bundle, manifest.BundleRef, stashMessage(manifest))
			if err != nil {
				return err
			}
			apply := "git stash apply"
			if res.Index > 0 {
				apply += fmt.Sprintf(" %d", res.Index) // bare N: PowerShell mangles stash@{N}
			}
			if res.AlreadyStashed {
				a.info("✓ Already in your stash list as stash@{%d}", res.Index)
			} else {
				a.info("✓ Imported into stash@{%d}", res.Index)
			}
			_, err = fmt.Fprintf(a.stdout, "Run %q to apply it.\n", apply)
			return err
		},
	}
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	return cmd
}

// repoVerdict is the outcome of checking the manifest against the local
// repository.
type repoVerdict int

const (
	repoOK                 repoVerdict = iota // base commit present: same repository
	repoBaseMissing                           // same repository (or unknown): fetch
	repoBaseMissingShallow                    // shallow clone: fetch --unshallow
	repoForeign                               // different repository: fail
)

// repoCheck decides what to do before importing. Having the base commit
// proves the histories are related (its ID covers all of its history), so
// the repo hash only matters when the base is missing, to tell "wrong
// repository" from "run git fetch". A shallow clone's root commits are its
// cut-off points, so its hash can't be trusted; an empty manifestHash
// (shares from before the repo hash existed) can't be compared at all.
func repoCheck(manifestHash, localHash string, baseKnown, shallow bool) repoVerdict {
	switch {
	case baseKnown:
		return repoOK
	case shallow:
		return repoBaseMissingShallow
	case manifestHash != "" && manifestHash != localHash:
		return repoForeign
	default:
		return repoBaseMissing
	}
}

// verifyRepository compares the decrypted manifest with the local repository
// before anything is imported. Only the recipient sees these details, after
// a successful decrypt.
func (a *app) verifyRepository(ctx context.Context, repo string, m payload.Manifest) error {
	if m.BaseCommit == "" {
		return nil // nothing to compare; `git bundle verify` still runs
	}
	base := shortSHA(m.BaseCommit)
	baseKnown := git.HasCommit(ctx, repo, m.BaseCommit)

	var localHash string
	var shallow bool
	if !baseKnown {
		shallow = git.IsShallow(ctx, repo)
		if h, err := git.RepoHash(ctx, repo, "HEAD"); err == nil {
			localHash = h // on error (no commits yet) "" can't match
		}
	}

	switch repoCheck(m.RepoHash, localHash, baseKnown, shallow) {
	case repoForeign:
		from := "a different repository"
		if name := cleanText(m.RepoName, maxRepoNameLen); name != "" {
			from += " (" + name + ")"
		}
		return userError{"This stash comes from " + from + ". Pull it inside a clone of that repository."}
	case repoBaseMissingShallow:
		return userError{fmt.Sprintf(`Base commit %s is missing from this shallow clone. Run "git fetch --unshallow" and try again.`, base)}
	case repoBaseMissing:
		return userError{fmt.Sprintf(`Base commit %s is missing. Run "git fetch" and try again.`, base)}
	}

	a.info("✓ Repository matches")
	// Importing never touches the working tree, so a different HEAD is only
	// worth a note; `git stash apply` deals with any conflicts.
	if head, err := git.Head(ctx, repo); err == nil && head != m.BaseCommit {
		on := base
		if branch := cleanText(m.Branch, maxRepoNameLen); branch != "" {
			on += " (" + branch + ")"
		}
		a.info("ℹ Based on %s; you're on %s.", on, shortSHA(head))
	}
	return nil
}

// shortSHA abbreviates a commit to 7 characters for display. Manifest values
// come from the sender, so they're cleaned first.
func shortSHA(s string) string {
	r := []rune(cleanText(s, 64))
	if len(r) > 7 {
		r = r[:7]
	}
	return string(r)
}

// stashMessage is the recipient's stash list entry (stashvent-git skill):
// "Shared from <sender> — <message>". Both parts come from the sender, so
// they are cleaned before they reach the reflog and `git stash list` output.
func stashMessage(m payload.Manifest) string {
	from := "Shared via stashvent"
	// " — " is the separator withoutSharedPrefix cuts at, so a name can't
	// hold it.
	if name := strings.ReplaceAll(cleanText(m.SenderName, maxSenderNameLen), sharedSeparator, " - "); name != "" {
		from = "Shared from " + name
	}
	// An older stashvent put the whole chain in the manifest.
	msg := cleanText(withoutSharedPrefix(m.StashMessage), maxStashMessageLen)
	if msg == "" {
		return from
	}
	return from + sharedSeparator + msg
}

const sharedSeparator = " — "

// withoutSharedPrefix drops the "Shared from <sender> — " parts that
// stashMessage put in front of an imported stash's message, so a stash pushed
// on again doesn't collect one per hop: the recipient sees the newest sender
// and git's own message. It is the inverse of stashMessage: push uses it for
// the manifest, pull for shares from older senders. A message that is only
// "Shared from <sender>" is left alone.
func withoutSharedPrefix(msg string) string {
	for {
		rest, ok := strings.CutPrefix(msg, "Shared from ")
		if !ok {
			rest, ok = strings.CutPrefix(msg, "Shared via stashvent")
		}
		if !ok {
			return msg
		}
		_, after, found := strings.Cut(rest, sharedSeparator)
		if !found {
			return msg
		}
		msg = after
	}
}

const (
	maxSenderNameLen   = 100 // runes
	maxStashMessageLen = 200 // runes
	maxRepoNameLen     = 100 // runes; also used for the branch
)

// cleanText drops control characters (C0 and C1, which include terminal
// escape sequences) and invisible format characters (Unicode category Cf,
// e.g. the right-to-left override U+202E, which could make the entry display
// different text than it contains), turns tabs and newlines into spaces,
// collapses runs of whitespace, and caps the length in runes.
func cleanText(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsSpace(r): // tabs, newlines, and other whitespace
			space = true
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
			continue
		}
		if n >= maxRunes {
			b.WriteString("…")
			break
		}
		if space && n > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// importBundle writes the decrypted bundle (plaintext source) to a temp file
// that is removed on every exit path, and imports it into the stash list.
func importBundle(ctx context.Context, repo string, bundle []byte, ref, message string) (git.ImportResult, error) {
	f, err := os.CreateTemp("", "stashvent-*.bundle")
	if err != nil {
		return git.ImportResult{}, fmt.Errorf("creating temp file: %w", err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }() // best effort; the OS temp dir is per user
	_, werr := f.Write(bundle)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return git.ImportResult{}, fmt.Errorf("writing temp bundle: %w", werr)
	}
	return git.ImportBundle(ctx, repo, path, ref, message)
}

// readPassword gets the share's password: the first line of stdin, or a
// hidden prompt on the terminal.
func (a *app) readPassword(ctx context.Context, fromStdin bool) (string, error) {
	if fromStdin {
		return a.stdinPassword(ctx)
	}
	return a.promptSecret(ctx, "Password: ", "--password-stdin")
}
