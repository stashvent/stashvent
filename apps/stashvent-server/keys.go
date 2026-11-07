package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/storage"
	"github.com/stashvent/stashvent/internal/storage/postgres"
)

const maxLabelLength = 200 // characters, as ck_api_keys_label

// serverCommand is stashvent-server: with no arguments it serves; "keys" holds
// the operator commands, which ship in the open-source server so
// a self-hoster can run a public instance for others.
func serverCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "stashvent-server",
		Short: "The StashVent API server",
		Long: `The StashVent API server. With no command it serves the /v1 API, configured
from environment variables (see .env.example). "keys" manages senders' API keys.`,
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(*cobra.Command, []string) error { return run() },
	}
	root.AddCommand(keysCommand())
	return root
}

func keysCommand() *cobra.Command {
	keys := &cobra.Command{
		Use:   "keys",
		Short: "Issue, list, revoke, and ban senders' API keys",
		Long: `Manage the API keys that senders need when REQUIRE_API_KEY is on. Keys are
named by their ID, which "keys list" shows. The commands read DATABASE_URL
(and, for ban, the S3 settings) like the server does.`,
	}

	var label string
	issue := &cobra.Command{
		Use:   "issue --label <label>",
		Short: "Create a key and print it, once",
		Long: `Create a key and print it on stdout. It is shown only this once: the server
keeps just its hash. Label it with who it is for, e.g. "Alice laptop"; hand
out one key per person.`,
		Args: cobra.NoArgs,
		RunE: withOperator(false, func(ctx context.Context, op *operator, _ []string) error {
			return op.issue(ctx, label)
		}),
	}
	issue.Flags().StringVar(&label, "label", "", "who the key is for (required)")
	_ = issue.MarkFlagRequired("label")

	var shareID string
	list := &cobra.Command{
		Use:   "list [--share <share-id>]",
		Short: "List keys, or find the key that created a share",
		Args:  cobra.NoArgs,
		RunE: withOperator(false, func(ctx context.Context, op *operator, _ []string) error {
			return op.list(ctx, shareID)
		}),
	}
	list.Flags().StringVar(&shareID, "share", "", "show only the key that created this share")

	revoke := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Refuse a key from now on; its shares stay until they expire",
		Long: `Refuse a key from now on, for pushes and deletes. The shares it created stay
pullable until they expire: use this for a leaked or replaced key, whose
owner did nothing wrong. Their delete tokens still work.`,
		Args: keyIDArgs(1, 1),
		RunE: withOperator(false, func(ctx context.Context, op *operator, args []string) error {
			id, _ := parseKeyID(args[0]) // checked by Args
			return op.revoke(ctx, id)
		}),
	}

	ban := &cobra.Command{
		Use:   "ban <key-id>",
		Short: "Revoke a key and end every share it created",
		Long: `Revoke a key, then end every unexpired share it created the way DELETE does:
the object is deleted and the share answers 404 at once. Use this for abuse
or a takedown request. Safe to run again if it stops part-way.`,
		Args: keyIDArgs(1, 1),
		RunE: withOperator(true, func(ctx context.Context, op *operator, args []string) error {
			id, _ := parseKeyID(args[0]) // checked by Args
			return op.ban(ctx, id)
		}),
	}

	var days int
	usage := &cobra.Command{
		Use:   "usage [<key-id>]",
		Short: "Show shares and bytes per key",
		Long: `Show the shares each key created, and their declared bytes, over the last
--days UTC days (today included). With a key ID, show that key day by day.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if days < 1 {
				return errors.New("--days must be at least 1")
			}
			return keyIDArgs(0, 1)(cmd, args)
		},
		RunE: withOperator(false, func(ctx context.Context, op *operator, args []string) error {
			var id int64
			if len(args) == 1 {
				id, _ = parseKeyID(args[0]) // checked by Args
			}
			return op.usage(ctx, id, days)
		}),
	}
	usage.Flags().IntVar(&days, "days", 30, "how many UTC days to cover, today included")

	keys.AddCommand(issue, list, revoke, ban, usage)
	return keys
}

// withOperator opens the database (applying migrations, so keys can be issued
// before the server first runs) and runs fn. withObjects also connects to
// object storage, which only ban needs.
func withOperator(withObjects bool, fn func(context.Context, *operator, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		databaseURL := os.Getenv("DATABASE_URL")
		var objects objectDeleter
		if withObjects {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			databaseURL = cfg.databaseURL
			objects = storage.NewObjects(cfg.s3)
		} else if databaseURL == "" {
			return errors.New("DATABASE_URL is required")
		}

		// Only problems are logged; the commands report their results themselves.
		logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelWarn}))
		pool, err := openDatabase(ctx, databaseURL, logger)
		if err != nil {
			return err
		}
		defer pool.Close()

		op := &operator{
			store:   postgres.New(pool),
			objects: objects,
			stdout:  cmd.OutOrStdout(),
			stderr:  cmd.ErrOrStderr(),
			now:     time.Now,
		}
		return fn(ctx, op, args)
	}
}

// keyIDArgs accepts min to max arguments, each a key ID. It runs before
// withOperator, so a typo is reported without touching the database.
func keyIDArgs(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.RangeArgs(min, max)(cmd, args); err != nil {
			return err
		}
		for _, arg := range args {
			if _, err := parseKeyID(arg); err != nil {
				return err
			}
		}
		return nil
	}
}

func parseKeyID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a key ID; \"keys list\" shows them", s)
	}
	return id, nil
}

// operatorStore is what the operator commands need (internal/storage/postgres).
type operatorStore interface {
	CreateAPIKey(ctx context.Context, keyHash []byte, label string) (storage.APIKey, error)
	GetAPIKey(ctx context.Context, id int64) (storage.APIKey, error)
	ListAPIKeys(ctx context.Context) ([]storage.APIKey, error)
	RevokeAPIKey(ctx context.Context, id int64) (storage.APIKey, error)
	BanAPIKey(ctx context.Context, id int64) (storage.APIKey, error)
	ListLiveSharesByKey(ctx context.Context, apiKeyID int64) ([]storage.ExpiredShare, error)
	ShareAPIKeyID(ctx context.Context, shareID string) (int64, error)
	ListKeyUsage(ctx context.Context, since time.Time, apiKeyID int64) ([]storage.KeyUsage, error)
	RevokeShare(ctx context.Context, shareID string) error
}

// operator runs the keys commands. Results go to stdout, notes to stderr. The
// key itself is printed once, by issue, and never logged.
type operator struct {
	store   operatorStore
	objects objectDeleter // only for ban
	stdout  io.Writer
	stderr  io.Writer
	now     func() time.Time
}

func (op *operator) note(format string, args ...any) {
	_, _ = fmt.Fprintf(op.stderr, format+"\n", args...)
}

func (op *operator) issue(ctx context.Context, label string) error {
	label = strings.TrimSpace(label)
	if n := utf8.RuneCountInString(label); n == 0 || n > maxLabelLength {
		return fmt.Errorf("--label must be 1 to %d characters", maxLabelLength)
	}
	if printable(label) != label {
		return errors.New("--label must not hold tabs, line breaks, or other control characters")
	}
	key, err := apikey.New()
	if err != nil {
		return err
	}
	k, err := op.store.CreateAPIKey(ctx, apikey.Hash(key), label)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(op.stdout, key); err != nil {
		return fmt.Errorf("printing the key: %w", err)
	}
	op.note("Issued key %d (%s). It is shown only this once: the server keeps just its hash.", k.ID, printable(k.Label))
	return nil
}

func (op *operator) list(ctx context.Context, shareID string) error {
	var keys []storage.APIKey
	if shareID != "" {
		id, err := op.store.ShareAPIKeyID(ctx, shareID)
		if errors.Is(err, storage.ErrShareNotFound) {
			return fmt.Errorf("no share %s (it may have expired and been removed)", shareID)
		}
		if err != nil {
			return err
		}
		if id == 0 {
			op.note("Share %s was created without an API key.", shareID)
			return nil
		}
		k, err := op.store.GetAPIKey(ctx, id)
		if err != nil {
			return err
		}
		keys = []storage.APIKey{k}
	} else {
		var err error
		if keys, err = op.store.ListAPIKeys(ctx); err != nil {
			return err
		}
		if len(keys) == 0 {
			op.note("No keys yet. Issue one with: stashvent-server keys issue --label <label>")
			return nil
		}
	}

	tw := tabwriter.NewWriter(op.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tLABEL\tCREATED\tSTATUS")
	for _, k := range keys {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", k.ID, printable(k.Label), formatDay(k.CreatedAt), keyStatus(k))
	}
	return tw.Flush()
}

func keyStatus(k storage.APIKey) string {
	switch {
	case k.BannedAt != nil:
		return "banned " + formatDay(*k.BannedAt)
	case k.RevokedAt != nil:
		return "revoked " + formatDay(*k.RevokedAt)
	default:
		return "active"
	}
}

func formatDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (op *operator) revoke(ctx context.Context, id int64) error {
	before, err := op.store.GetAPIKey(ctx, id)
	if errors.Is(err, storage.ErrAPIKeyNotFound) {
		return fmt.Errorf("no key %d", id)
	}
	if err != nil {
		return err
	}
	if before.RevokedAt != nil {
		op.note("Key %d (%s) is already %s.", id, printable(before.Label), keyStatus(before))
		return nil
	}
	if _, err := op.store.RevokeAPIKey(ctx, id); err != nil {
		return err
	}
	op.note("Revoked key %d (%s). The shares it created stay pullable until they expire.", id, printable(before.Label))
	return nil
}

// ban revokes the key, then ends its live shares one by one. A share that
// can't be ended stays live and the command fails, so running it again
// picks up exactly what is left.
func (op *operator) ban(ctx context.Context, id int64) error {
	k, err := op.store.BanAPIKey(ctx, id)
	if errors.Is(err, storage.ErrAPIKeyNotFound) {
		return fmt.Errorf("no key %d", id)
	}
	if err != nil {
		return err
	}
	shares, err := op.store.ListLiveSharesByKey(ctx, id)
	if err != nil {
		return fmt.Errorf("key %d is banned, but its shares could not be listed; run the command again: %w", id, err)
	}
	var failed int
	for _, s := range shares {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("interrupted; key %d is banned, run the command again to end the rest of its shares", id)
		}
		if err := endShare(ctx, op.store, op.objects, s.ShareID, s.StorageKey); err != nil {
			failed++
			op.note("Could not end share %s: %v", s.ShareID, err)
		}
	}
	ended := len(shares) - failed
	op.note("Banned key %d (%s). Ended %d %s.", id, printable(k.Label), ended, plural(ended, "share", "shares"))
	if failed > 0 {
		return fmt.Errorf("%d %s could not be ended; run the command again", failed, plural(failed, "share", "shares"))
	}
	return nil
}

func (op *operator) usage(ctx context.Context, id int64, days int) error {
	today := op.now().UTC()
	since := today.AddDate(0, 0, -(days - 1))
	rows, err := op.store.ListKeyUsage(ctx, since, id)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(op.stdout, 0, 0, 2, ' ', 0)

	if id != 0 {
		if _, err := op.store.GetAPIKey(ctx, id); errors.Is(err, storage.ErrAPIKeyNotFound) {
			return fmt.Errorf("no key %d", id)
		} else if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(tw, "DAY\tSHARES\tBYTES")
		for _, u := range rows {
			_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\n", formatDay(u.Day), u.Shares, u.Bytes)
		}
		return tw.Flush()
	}

	keys, err := op.store.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	type total struct {
		today, shares int
		bytes         int64
	}
	totals := map[int64]*total{}
	for _, u := range rows {
		t := totals[u.APIKeyID]
		if t == nil {
			t = &total{}
			totals[u.APIKeyID] = t
		}
		t.shares += u.Shares
		t.bytes += u.Bytes
		if formatDay(u.Day) == formatDay(today) {
			t.today += u.Shares
		}
	}
	_, _ = fmt.Fprintf(tw, "ID\tLABEL\tTODAY\tSHARES (%dd)\tBYTES (%dd)\n", days, days)
	for _, k := range keys {
		t := totals[k.ID]
		if t == nil {
			t = &total{}
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%d\n", k.ID, printable(k.Label), t.today, t.shares, t.bytes)
	}
	return tw.Flush()
}

// printable makes a label safe to print: the closed-source hosted service
// writes labels too, so one may hold tabs or line breaks that break the tables,
// or escape sequences that drive the operator's terminal. Each such
// character becomes "?".
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return '?'
		}
		return r
	}, s)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
