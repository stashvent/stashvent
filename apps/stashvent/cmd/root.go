// Package cmd holds the stashvent CLI commands. Commands are thin: they parse
// flags and call internal packages. Results go to stdout; progress, prompts,
// and errors go to stderr.
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/stashvent/stashvent/apikey"
	"github.com/stashvent/stashvent/internal/api"
	"github.com/stashvent/stashvent/internal/apiclient"
	"github.com/stashvent/stashvent/internal/crypto"
	"github.com/stashvent/stashvent/internal/git"
	"github.com/stashvent/stashvent/internal/security"
	"github.com/stashvent/stashvent/internal/userfiles"
)

// defaultServer is the hosted instance. It must stay in
// apiclient.NormalizeServer's form: push and delete compare the chosen
// server with it to decide whether to say which server they use. A local dev
// server is chosen with STASHVENT_SERVER=http://localhost:8080.
const defaultServer = "https://stashvent.dev"

// serverEnv names the environment variable that selects the server.
const serverEnv = "STASHVENT_SERVER"

// apiKeyEnv names the environment variable that holds an API key, for CI and
// machines without config.json. It wins over the stored key.
const apiKeyEnv = "STASHVENT_API_KEY" //nolint:gosec // G101: the name of a variable, not a credential

// Exit codes (stashvent-cli skill).
const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitCancelled = 130
)

// usageError marks bad flags or arguments (exit code 2).
type usageError struct{ error }

// userError is an error whose message is already written for users.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

// errCancelled means the user declined a prompt (exit code 130, like Ctrl+C).
var errCancelled = errors.New("cancelled")

// Env is everything a run of the CLI takes from outside itself, so it can
// run without a terminal, a home directory, or the process's own state.
// Stdin, Stdout, and Stderr are required; the rest has the defaults named.
type Env struct {
	Version string
	Commit  string   // the build's commit; empty if unknown
	Args    []string // without the program name
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer

	Dir       string // working directory; default: the process's
	ConfigDir string // holds history.json and config.json; default: userfiles.Dir()

	Getenv     func(string) string // default: os.Getenv
	Now        func() time.Time    // default: time.Now
	IsTerminal func() bool         // whether Stdin is a terminal; default: it is not
	// ReadSecret reads one line from the terminal without showing it. Only
	// called when IsTerminal says yes.
	ReadSecret func(context.Context) (string, error)
}

// serverSource says where the server URL of a run came from.
type serverSource int

const (
	serverFromDefault serverSource = iota
	serverFromFlag
	serverFromEnv
)

func (s serverSource) String() string {
	switch s {
	case serverFromFlag:
		return "--server"
	case serverFromEnv:
		return serverEnv
	default:
		return "the default"
	}
}

type app struct {
	version string
	commit  string
	server  string // --server
	stdout  io.Writer
	stderr  io.Writer
	// in is stdin. Everything reads it through this one reader: a second
	// buffered reader would lose what the first had already read ahead.
	in        *bufio.Reader
	dir       string
	configDir string

	getenv      func(string) string
	now         func() time.Time
	loc         *time.Location // for times shown to the user
	isTerminal  func() bool
	readSecret  func(context.Context) (string, error)
	newPassword func() (string, error)
	maxPayload  int64 // api.DefaultMaxPayloadBytes; tests set less
	// scan is scanStash; tests replace it to reach failures a real stash
	// would need tens of megabytes for.
	scan func(context.Context, string, git.Stash) ([]security.Finding, error)

	// Set by serverURL, for the message when that server can't be reached.
	resolvedServer string
	serverFrom     serverSource
}

// Execute runs the CLI with args and returns the process exit code.
func Execute(version, commit string, args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return Run(ctx, Env{
		Version: version, Commit: commit, Args: args,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		IsTerminal: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		ReadSecret: readTerminalSecret,
	})
}

// Run runs the CLI in env and returns the exit code. Cancelling ctx is what
// Ctrl+C does: cleanup still runs, and the code is 130.
func Run(ctx context.Context, env Env) int {
	return newApp(env).run(ctx, env.Args)
}

func newApp(env Env) *app {
	a := &app{
		version: env.Version, commit: env.Commit,
		stdout: env.Stdout, stderr: env.Stderr,
		in:  bufio.NewReader(env.Stdin),
		dir: env.Dir, configDir: env.ConfigDir,
		getenv: env.Getenv, now: env.Now, loc: time.Local,
		isTerminal: env.IsTerminal, readSecret: env.ReadSecret,
		newPassword: crypto.GeneratePassword,
		maxPayload:  api.DefaultMaxPayloadBytes,
		scan:        scanStash,
	}
	if a.getenv == nil {
		a.getenv = os.Getenv
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.isTerminal == nil {
		a.isTerminal = func() bool { return false }
	}
	return a
}

func (a *app) run(ctx context.Context, args []string) int {
	root := a.rootCommand()
	if args == nil {
		args = []string{} // cobra reads os.Args when it is given nil
	}
	root.SetArgs(args)

	cmd, err := root.ExecuteContextC(ctx)
	switch {
	case err == nil:
		return exitOK
	case ctx.Err() != nil, errors.Is(err, errCancelled):
		a.info("✗ Cancelled")
		return exitCancelled
	case errors.As(err, new(usageError)):
		a.info("✗ %v\nRun \"%s --help\" for usage.", err, cmd.CommandPath())
		return exitUsage
	default:
		a.info("✗ %v", a.friendly(err))
		return exitError
	}
}

// usageTemplate is cobra's, with two changes: the root's usage line names
// the command it needs (cobra prints "stashvent [flags]" and "stashvent
// [command]" for a root that can run), and the parts this CLI doesn't use
// (aliases, groups, help topics) are left out.
const usageTemplate = `Usage:{{if .HasParent}}
  {{.UseLine}}{{else}}
  {{.CommandPath}} <command> [flags]{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} <command> --help" for more information about a command.{{end}}
`

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "stashvent",
		Short: "Share unfinished Git work as an encrypted stash",
		Long: `StashVent shares unfinished Git work. A stash is packaged, encrypted on this
machine with a password, and uploaded. Whoever has the link and the password
can import it into their own stash list. The server never sees the password
or the contents.`,
		Example: `  # Share the newest stash; prints a link and a password
  stashvent push

  # Import a shared stash (asks for the password)
  stashvent pull https://stashvent.example/8xK29qLm4Tz7

  # See what this machine has shared, and take a share back
  stashvent list
  stashvent delete 8xK29qLm4Tz7`,
		Version:       a.versionText(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// Cobra only sets its default (2) on the path this root bypasses.
		SuggestionsMinimumDistance: 2,
		// With Args and RunE set, cobra hands unknown subcommands (e.g. a typo
		// like "psh") to RunE instead of returning its own unwrapped error, so
		// they get exit code 2 like every other usage error.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			msg := fmt.Sprintf("unknown command %q", args[0])
			if similar := cmd.SuggestionsFor(args[0]); len(similar) > 0 {
				msg += fmt.Sprintf(". Did you mean %q?", similar[0])
			}
			return usageError{errors.New(msg)}
		},
	}
	root.SetIn(a.in)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetVersionTemplate("stashvent {{.Version}}\n")
	root.SetUsageTemplate(usageTemplate)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().StringVar(&a.server, "server", "", "StashVent server URL (env "+serverEnv+")")
	// Shell completion works ("stashvent completion <shell>"), but it is
	// cobra's own text and untested here, so it stays out of the help.
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(a.pushCommand(), a.pullCommand(), a.listCommand(), a.deleteCommand(),
		a.loginCommand(), a.logoutCommand(), a.versionCommand())
	return root
}

// info writes a line of progress, warning, or error text to stderr. If
// writing to the terminal fails there's nowhere left to report it, so the
// error is deliberately ignored. Results go to stdout and are checked.
func (a *app) info(format string, args ...any) {
	_, _ = fmt.Fprintf(a.stderr, format+"\n", args...)
}

// serverURL applies the precedence --server > STASHVENT_SERVER > default and
// returns the server in the one form it is used and stored in.
func (a *app) serverURL() (string, error) {
	raw, from := defaultServer, serverFromDefault
	if a.server != "" {
		raw, from = a.server, serverFromFlag
	} else if env := a.getenv(serverEnv); env != "" {
		raw, from = env, serverFromEnv
	}
	server, err := apiclient.NormalizeServer(raw)
	if err != nil {
		if from != serverFromDefault {
			err = fmt.Errorf("%w (from %s)", err, from)
		}
		return "", usageError{err}
	}
	a.resolvedServer, a.serverFrom = server, from
	return server, nil
}

// workDir returns the directory the command runs in.
func (a *app) workDir() (string, error) {
	if a.dir != "" {
		return a.dir, nil
	}
	return os.Getwd()
}

// history returns this user's share history. Nothing is read yet.
func (a *app) history() (*userfiles.History, error) {
	dir := a.configDir
	if dir == "" {
		var err error
		if dir, err = userfiles.Dir(); err != nil {
			return nil, userError{"Your user config directory could not be found, so the share history can't be kept."}
		}
	}
	return userfiles.NewHistory(dir), nil
}

// config returns this user's config.json, which holds API keys. Nothing is
// read yet.
func (a *app) config() (*userfiles.Config, error) {
	dir := a.configDir
	if dir == "" {
		var err error
		if dir, err = userfiles.Dir(); err != nil {
			return nil, userError{"Your user config directory could not be found, so API keys can't be kept."}
		}
	}
	return userfiles.NewConfig(dir), nil
}

// configError marks an error reading or writing config.json, so it is worded
// with that file's name (see userFileFailure).
type configError struct{ error }

func (e configError) Unwrap() error { return e.error }

// apiKey returns the API key to send to server, "" if there is none:
// STASHVENT_API_KEY if it is set, otherwise the key stored for that server.
// fromEnv says which. The caller decides that server may get the key: only
// the server chosen for a push or delete, never a link's host.
func (a *app) apiKey(server string) (key string, fromEnv bool, err error) {
	if env := strings.TrimSpace(a.getenv(apiKeyEnv)); env != "" {
		// Never repeated: it is a secret, and may be one pasted by mistake.
		if !apikey.Valid(env) {
			return "", true, userError{apiKeyEnv + " doesn't hold a StashVent API key (they start with " + apikey.Prefix + "). Check its value."}
		}
		return env, true, nil
	}
	config, err := a.config()
	if err != nil {
		return "", false, err
	}
	key, _, err = config.APIKey(server)
	if err != nil {
		return "", false, configError{err}
	}
	return key, false, nil
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the stashvent version and commit",
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintf(a.stdout, "stashvent %s\n", a.versionText())
			return err
		},
	}
}

// versionText is what version and --version print after "stashvent": the
// version, and the commit when the build knows it. The User-Agent carries
// the version only.
func (a *app) versionText() string {
	if a.commit == "" {
		return a.version
	}
	return a.version + " (commit " + a.commit + ")"
}

func exactArgs(n int) cobra.PositionalArgs { return usageArgs(cobra.ExactArgs(n)) }

func maxArgs(n int) cobra.PositionalArgs { return usageArgs(cobra.MaximumNArgs(n)) }

// usageArgs marks cobra's argument-count errors as usage errors (exit 2).
func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

// readLine reads one line from stdin, without its line ending. The read
// blocks and ignores ctx, so it runs in a goroutine; on Ctrl+C the process
// exits next and the reader goes with it. At the end of input it returns
// io.EOF.
func (a *app) readLine(ctx context.Context) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := a.in.ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case r := <-done:
		if r.err != nil && r.line == "" {
			return "", r.err
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// confirm asks a yes/no question that defaults to No. Without a terminal it
// never blocks: the answer is No. Ctrl+C returns at once instead of waiting
// for Enter.
func (a *app) confirm(ctx context.Context, question string) (bool, error) {
	if !a.isTerminal() {
		a.info("%s [y/N] no (not a terminal)", question)
		return false, nil
	}
	_, _ = fmt.Fprintf(a.stderr, "%s [y/N] ", question) // no newline, so not a.info

	line, err := a.readLine(ctx)
	if ctx.Err() != nil {
		a.info("")
		return false, ctx.Err()
	}
	if err != nil {
		return false, nil // the end of input is No
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// promptSecret asks for one line on the terminal without showing it. Without
// a terminal it points to stdinFlag, the command's way to read from stdin.
func (a *app) promptSecret(ctx context.Context, prompt, stdinFlag string) (string, error) {
	if !a.isTerminal() || a.readSecret == nil {
		return "", usageError{fmt.Errorf("stdin is not a terminal; use %s", stdinFlag)}
	}
	_, _ = fmt.Fprint(a.stderr, prompt) // no newline, so not a.info
	secret, err := a.readSecret(ctx)
	a.info("") // the Enter that ended the line wasn't shown either
	return secret, err
}

// readTerminalSecret is Env.ReadSecret for the real terminal.
func readTerminalSecret(ctx context.Context) (string, error) {
	fd := int(os.Stdin.Fd())
	// ReadPassword ignores ctx and turns echo off. Save the terminal state so
	// Ctrl+C can restore echo and return at once instead of waiting for Enter.
	state, err := term.GetState(fd)
	if err != nil {
		return "", fmt.Errorf("reading terminal state: %w", err)
	}

	type result struct {
		secret []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		secret, err := term.ReadPassword(fd)
		done <- result{secret, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return "", fmt.Errorf("reading password: %w", r.err)
		}
		return string(r.secret), nil
	case <-ctx.Done():
		_ = term.Restore(fd, state) // the process exits next; the reader goroutine goes with it
		return "", ctx.Err()
	}
}

// friendly turns an error into a message for users: API codes and known
// failures become plain sentences, other errors pass through.
func (a *app) friendly(err error) string {
	var ue userError
	if errors.As(err, &ue) {
		return ue.msg
	}
	var ve *git.VersionError
	if errors.As(err, &ve) {
		return fmt.Sprintf("StashVent needs Git %s or newer (you have %s).\nUpgrade: https://git-scm.com/downloads",
			git.MinVersion, ve.Have)
	}
	if msg, ok := a.userFileFailure(err); ok {
		return msg
	}
	var netErr *apiclient.NetworkError
	if errors.As(err, &netErr) {
		switch netErr.Op {
		case apiclient.OpUpload:
			return "The upload didn't finish. Check your connection and try again."
		case apiclient.OpDownload:
			return "The download didn't finish. Check your connection and try again."
		}
		msg := fmt.Sprintf("Could not reach %s. Check that it is running and that you are connected, then try again.", netErr.Server)
		if netErr.Server == a.resolvedServer && a.serverFrom != serverFromDefault {
			msg += fmt.Sprintf("\nThat server was set with %s.", a.serverFrom)
		}
		return msg
	}
	var storageErr *apiclient.StorageError
	if errors.As(err, &storageErr) {
		if storageErr.Op == apiclient.OpUpload {
			return fmt.Sprintf("Storage didn't accept the upload (HTTP %d). Try again in a moment.", storageErr.Status)
		}
		return fmt.Sprintf("The download failed (HTTP %d). The share may have just expired or been deleted.", storageErr.Status)
	}
	var apiErr *apiclient.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case api.CodeShareNotFound:
			return "Share not found or expired."
		case api.CodePayloadTooLarge:
			return "This stash is too large for this server."
		case api.CodeSizeMismatch:
			return "The upload didn't arrive complete. Try again."
		case api.CodeInvalidDeleteToken:
			return "The server didn't accept this machine's delete token for the share."
		case api.CodeRateLimited:
			return "Too many requests. Wait a moment and try again."
		case api.CodeMissingAPIKey:
			return "This server needs an API key to push. Run \"stashvent login\" with the key its operator gave you."
		case api.CodeInvalidAPIKey:
			return "The server didn't accept your API key: it was revoked or mistyped.\n" +
				"Ask the server's operator for a new one, then run \"stashvent login\"."
		case api.CodeQuotaExceeded:
			msg := "Your API key has used its daily share limit on this server."
			if apiErr.RetryAfter > 0 {
				msg += " Try again in " + formatWait(apiErr.RetryAfter) + "."
			} else {
				msg += " Try again tomorrow (UTC)."
			}
			return msg
		case api.CodeStorageFull:
			return "The server is out of storage space. Try again later."
		case api.CodeShareNotOwned:
			return "Your API key didn't create this share, so it can't delete it."
		case api.CodePullLimitReached:
			return "This share has been pulled the maximum number of times. Ask the sender to push it again."
		default:
			// A code without wording of its own: invalid_request, or one this
			// version doesn't know (a newer server's, such as one asking for
			// an upgrade). The server's own words are the only explanation
			// there is. They come from whichever server a link names, so
			// they're cleaned like any outside text, and the server is named.
			// internal's message is for the operator, not the user.
			if m := cleanText(apiErr.Message, maxServerMessageLen); m != "" && apiErr.Code != api.CodeInternal {
				return withRequestID(fmt.Sprintf("The server at %s returned an error (HTTP %d): \"%s\".",
					cleanText(apiErr.Server, maxLinkHost), apiErr.Status, m), apiErr)
			}
			return withRequestID(fmt.Sprintf("The server returned an unexpected error (%d).", apiErr.Status), apiErr)
		}
	}
	return err.Error()
}

const (
	maxServerMessageLen = 200 // runes of a server's error message shown
	maxRequestIDLen     = 64  // runes; the server's own IDs have 26
)

// withRequestID appends the response's request ID, for support. It's a
// header of whichever server answered, so it's cleaned like the message.
func withRequestID(msg string, apiErr *apiclient.Error) string {
	if id := cleanText(apiErr.RequestID, maxRequestIDLen); id != "" {
		msg += " Request ID: " + id
	}
	return msg
}

// userFileFailure words the errors of the share history and of config.json.
func (a *app) userFileFailure(err error) (string, bool) {
	name, path, holds := "share history", "history.json", "the delete tokens of your shares"
	if h, herr := a.history(); herr == nil {
		path = h.Path()
	}
	if errors.As(err, new(configError)) {
		name, path, holds = "config file", "config.json", "your API keys"
		if c, cerr := a.config(); cerr == nil {
			path = c.Path()
		}
	}
	switch {
	case errors.Is(err, userfiles.ErrCorrupt):
		return fmt.Sprintf("The %s can't be read: %s\n"+
			"Fix or remove that file. It is never overwritten, because it holds %s.", name, path, holds), true
	case errors.Is(err, userfiles.ErrNewerVersion):
		return fmt.Sprintf("The %s was written by a newer stashvent: %s\nUpgrade stashvent to use it.", name, path), true
	case errors.Is(err, userfiles.ErrLocked):
		return fmt.Sprintf("The %s is in use by another stashvent. Try again in a moment.", name), true
	}
	return "", false
}

// formatWait says how long to wait, to the minute: "5h 12m", "40m", "1m".
func formatWait(d time.Duration) string {
	minutes := max(1, int(math.Ceil(d.Minutes())))
	switch h, m := minutes/60, minutes%60; {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh %dm", h, m)
	}
}
