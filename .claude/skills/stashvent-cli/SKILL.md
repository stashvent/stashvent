---
name: stashvent-cli
description: Conventions for the stashvent CLI (apps/stashvent) — command structure with cobra, output style and streams, prompts and confirmations, exit codes, config and history files, server URL handling, cancellation, and Windows support. Load whenever adding or changing CLI commands, flags, prompts, terminal output, or local config/history files.
---

# StashVent CLI Conventions

The CLI is the product. Every command should look and behave the same way.
The golden files in `apps/stashvent/cmd/testdata/` are what the CLI prints.

## Structure

- Framework: `github.com/spf13/cobra`.
- Layout:

```
apps/stashvent/
  main.go          # only calls cmd.Execute() (with the version) and sets the exit code
  cmd/
    root.go        # Env, Run, global flags (--server), help, version
    push.go
    pull.go
    list.go
    delete.go
    login.go       # login / logout (API key per server)
    testdata/      # golden files
internal/userfiles/  # history.json and config.json
```

- Commands are thin: parse flags, then call `internal/*` packages. `push`
  is orchestration, split into named steps on `app`; the git, crypto, and
  HTTP logic itself stays in `internal/*`.
- Everything a run takes from outside is in `cmd.Env` (streams, working
  directory, config directory, environment, clock, terminal check, hidden
  prompt). `cmd.Execute` fills it from the process; `cmd.Run(ctx, env)` is
  the entry point for tests, including ones in another package. Don't call
  `os.Getenv`, `os.Getwd`, `time.Now`, or `os.Stdin` from a command.
- Stdin is read through one `bufio.Reader` (`app.in`, via `readLine`). A
  second reader would lose what the first read ahead, so two prompts in one
  run would break.

## Commands

| Command | Notes |
|---|---|
| `push [stash]` | `--password` (hidden prompt, no value), `--password-stdin`, `--expires 1h\|6h\|24h\|7d` (default `6h`), `--ignore-secrets` |
| `pull <link>` | a link or a bare share ID; `--password-stdin` |
| `list` | local history only, no request |
| `delete <id>` | an ID or a link; the stored delete token, else the API key (see below) |
| `login` | saves an API key for the chosen server; hidden prompt or `--with-token` (stdin), never an argument |
| `logout` | removes the saved key for the chosen server |
| `version` | also `--version` |

- Bare `stashvent`, `stashvent help`, and `--help` print the same help on
  stdout and exit 0. An unknown command is a usage error (exit 2) with a
  "Did you mean" hint.
- Every command has a `Long` description and `Example`s. Examples use the
  reserved host `stashvent.example`, never a real one.
- cobra's `completion` command is kept but hidden: it works, but it is
  cobra's text and untested on the three shells.
- There is no `--yes`. The only yes/no prompt is the secret scan, and no
  flag but `--ignore-secrets` gets past that. If a command ever needs
  `--yes`, it must still never bypass the secret scan.

## Output

- **stdout** is for results: share link, password, expiry, `list` rows.
- **stderr** is for progress, warnings, prompts, and errors.
  This keeps `stashvent push > share.txt` and piping clean.
- Style:
  - `✓ Encrypted locally` for completed steps
  - `⚠ Potential secrets detected` for warnings
  - `✗ Decryption failed: wrong password or corrupted share` for errors
- No colour today. If it is added: only when the stream is a terminal and
  `NO_COLOR` is not set.
- Error messages say what happened and what to do next:
  `✗ Base commit a82fd91 is missing. Run "git fetch" and try again.`
  Never show raw Go errors, stack traces, or API error codes to users.
  `app.friendly` is where errors get their wording; a network failure is
  `apiclient.NetworkError` and never suggests switching to another server.
- Text that comes from outside (stash messages, manifest fields, values in
  `history.json`) goes through `cleanText` before it reaches the terminal.

## Secrets on screen

- The generated password is printed **once**, at the end of `push`, with
  the reminder to share it through a separate channel.
- Typed passwords are never echoed or printed, including in error
  messages: `--password=x` and `--password x` are refused without repeating
  `x` (the flag accepts any value so that cobra can't quote it). With a
  password of the sender's own, `push` prints the link and expiry only.
- A custom password must be at least `crypto.MinPasswordLength` (12)
  characters, and is used byte for byte: only the line ending is dropped.
- Never print the delete token, presigned URLs, or decrypted file contents
  (except what `git` itself shows the user).
- Never print an API key, also not one that is refused (`login` with a
  value that isn't a key, a malformed `STASHVENT_API_KEY`): it may be some
  other secret pasted by mistake. `push` may say where the key came from
  (`API key: from STASHVENT_API_KEY`), never what it is.

## Prompts and confirmations

- Passwords and API keys: `golang.org/x/term`'s `ReadPassword` on the
  terminal (`Env.ReadSecret`, through `promptSecret`). If stdin isn't a
  terminal, fail with a usage error pointing to the command's stdin flag:
  `--password-stdin`, or `--with-token` for `login`. `push --password` asks
  twice.
- Yes/no questions default to **No**: `Share anyway? [y/N]`.
- When not attached to a terminal (scripts, CI), never block on a prompt:
  - yes/no prompts are treated as No
  - the secret-scan warning cancels the push unless `--ignore-secrets` is
    passed
  - the stash picker is skipped and the newest stash is shared
- Stash picker: `push` with no argument, two or more stashes, and a
  terminal lists them and asks. The numbers are git's own (0-based), so
  the answer means the same as in `stashvent push N`. Enter picks 0.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error (network, git, decryption, server error) |
| 2 | Usage error (bad flags or arguments) |
| 130 | Cancelled by the user (Ctrl+C, or answered No / Cancel) |

## Config and history files

- Directory: `os.UserConfigDir()` + `/stashvent/`
  (Windows `%AppData%`, macOS `~/Library/Application Support`, Linux
  `~/.config`). Never hardcode `~/.config`.
- `history.json`: shares sent from this machine. **Never passwords.** The
  layout is frozen at v0.1; fields may be added, never renamed or removed:

  ```json
  {
    "version": 1,
    "shares": [
      {
        "share_id": "8xK29qLm4Tz7",
        "server": "https://stash.example.com",
        "link": "https://stash.example.com/8xK29qLm4Tz7",
        "stash_message": "On main: payment retry WIP",
        "created_at": "2026-10-13T10:50:00Z",
        "expires_at": "2026-10-13T16:50:00Z",
        "delete_token": "<base64url>"
      }
    ]
  }
  ```

  - `server` is the only host the delete token is ever sent to. `delete`
    ignores `--server` and `STASHVENT_SERVER`, and checks the stored URL
    like one typed on the command line.
  - A share missing from the history can be deleted with the API key
    (`deleteWithKey`). The key goes only to the server chosen for the
    command (`--server`, `STASHVENT_SERVER`, default), compared in
    `NormalizeServer`'s form, never to the host in a link: a link to
    another host is refused before any request, naming the host and
    suggesting `--server <host>`.
    With a history entry, only the token is sent, never the key.
  - Fields this version doesn't know are kept when the file is rewritten,
    at the top level and in each share.
  - A `version` above 1 is an error asking to upgrade; the file is left
    alone.
- `config.json` holds API keys only, one per server, keyed by the server in
  `NormalizeServer`'s form. No stored default server. Its layout is frozen
  at v0.1 like the history's:

  ```json
  {
    "version": 1,
    "servers": {
      "https://stash.example.com": {
        "api_key": "sv_…",
        "source": "pasted",
        "saved_at": "2026-10-17T10:00:00Z"
      }
    }
  }
  ```

  - `source` says how the key was obtained; a browser login, if added,
    adds its own value instead of a new layout.
  - A key is sent only to the server it is stored for, only on push and
    delete; never on pull or list, never to a link's host. The client
    makes this structural: only `CreateShare` and `DeleteShare` take a key.
  - `STASHVENT_API_KEY`, for CI, wins over the stored key (`app.apiKey`). It
    isn't tied to a server: it goes to the server chosen for the command
    (`--server`, `STASHVENT_SERVER`, default), like `GH_TOKEN` for `gh`,
    and otherwise follows the same rules. The person who sets
    it also picks the server.
  - Errors reading it are wrapped in `configError`, so messages name
    `config.json` rather than the history.
- Permissions: directory `0700`, files `0600` on Unix. On Windows those
  bits don't exist; the files rely on the access list inherited from
  `%AppData%` (the user, SYSTEM, Administrators). No explicit ACL is set,
  also not on `config.json` (decided 2026-10-17): `%AppData%` is already
  per-user, it is the protection the history's delete tokens have, and
  Administrators could read any file anyway.
- Write atomically: a temp file in the same directory, then rename.
- Every read and write holds a lock file (`history.json.lock`,
  `config.json.lock`): two pushes at once would lose an entry, and on
  Windows a rename fails while another process has the file open. A lock
  older than 10 seconds is taken over.
- Tolerate a missing or empty file (first run); a read doesn't create the
  directory. A corrupt file is an error naming the path, never overwritten.
- Expired entries are dropped when the file is read, and removed from it.
- No build-tagged files in `internal/userfiles`: lint only runs for Linux.

## Push: order and cleanup

1. Flags, server, the history, and the API key (`STASHVENT_API_KEY` or
   `config.json`) are checked first. A history or config that can't be
   read, or a malformed `STASHVENT_API_KEY`, stops the push before anything
   leaves the machine.
2. Git checks, choosing the stash.
3. Packaging, quietly, then the size check: the bundle file, then the
   encrypted size (the plaintext plus `crypto.TagSize`), compared with
   `api.DefaultMaxPayloadBytes` (5 MB, `app.maxPayload`). A size just over
   the limit reads "just over 5 MB" (`sizeOver`). The CLI can't read a
   server's `MAX_PAYLOAD_BYTES`; the server's `413` stays the authority
   below it. Packaging comes before the scan so a stash too large to send
   gets the size error, not the scan's own size limit
   (`git.ErrPatchTooLarge`) with advice to pass `--ignore-secrets`.
4. The secret scan (`app.scan`), then "✓ Packaged …". Nothing is encrypted
   or sent before the scan: that's the promise the help and DOCS.md make.
5. Password, encryption, create, upload, complete.
6. The share is written to the history, then the result is printed.

From "create" until the result is printed, a failure or Ctrl+C deletes the
share again (`abandonShare`: a `DELETE` on its own 5 second context, and
the history entry is removed). A share the sender was never shown must not
stay behind.

## Server URL

- Precedence: `--server` flag > `STASHVENT_SERVER` env var > default.
  There is no stored default and no `config set-server` (decided
  2026-10-15); `config.json` holds API keys only.
- The default is the hosted instance, `https://stashvent.dev` (registered
  2026-10-26, default since 2026-10-27). It is written in `NormalizeServer`'s
  form, because push and delete compare the chosen server with it;
  `TestDefaultServerIsTheHostedInstance` holds both. Local development sets
  `STASHVENT_SERVER=http://localhost:8080`. Tests always choose their server
  (`newCLI`, the e2e machines) and never reach the default.
- Servers are used and stored in one form (`apiclient.NormalizeServer`):
  scheme, lower-case host, optional port. A path, query, or user name is
  refused. A share link is the server plus `/<id>`.
- Refuse `http://` URLs except for `localhost` / loopback (local dev).
- `push`, and `delete` with the API key, print `Server: <url> (from …)` on
  stderr whenever the server is not the default, because an environment
  variable can redirect where the work and the key go.
- `pull` accepts a full link or a bare share ID. The server is taken from
  the link's host when a full link is given, and the configured server is
  not even resolved then.

## HTTP client

- Uses the shared types from `internal/api` (see `stashvent-api`).
- Timeouts on every request, `User-Agent: stashvent/<version>`. API response
  bodies are read up to 64 KB (`maxResponseBytes`) and `Download` refuses a
  size no share can have: a pull talks to whichever server a link names.
- Maps API error `code` values to friendly messages. For `invalid_request`
  and unknown codes it shows the server's message instead (`cleanText`,
  at most 200 runes, quoted, naming the server, with the request ID): it's
  how a newer server explains itself to this version. `internal`, and a
  message that cleans down to nothing, get the generic message plus the
  request ID. The request ID is a header of whichever server answered, so
  it goes through `cleanText` too.
- Push checks its own format fields with `payload.KnownFormat` before
  creating the share: the server doesn't check `bundle_version` or
  `object_format`, so nothing else stops a share no stashvent can pull.
- Before downloading, pull checks all four format fields with
  `payload.KnownFormat` and refuses unknown values with "This share uses a
  newer format. Upgrade stashvent and try again." It also checks `size`:
  the download is held in memory, and the size comes from whichever server
  the link names. Above `app.maxPayload` it asks for an upgrade; at or below
  `crypto.TagSize` it says the server reported an invalid size.
- The upload sends the response's `upload_headers` and takes any 2xx as
  success; links may hold share IDs of 12 to 64 characters. These and the
  two rules above are the forward-compatibility rules in `stashvent-api`
  ("Compatibility"): don't narrow them.
- `delete`: `204` removes the history entry; `404` means the share is
  already gone (unknown, expired, deleted) and also removes it, exit 0.
  Any other failure keeps the entry, so the delete can be tried again.
  With an API key (the share isn't in the history) the ID was typed, so a
  `404` also says it may be mistyped, and has no `✓`; still exit 0.
- `quota_exceeded` and `rate_limited` carry `Retry-After`; `apiclient.Error`
  has it as `RetryAfter`, and the quota message says how long to wait
  (`formatWait`).

## Cancellation and cleanup

- The root context comes from `signal.NotifyContext(ctx, os.Interrupt)`,
  and every long operation takes it.
- On Ctrl+C, deferred cleanup must still run: temp bundles, temp refs
  (see `stashvent-git`), and the unfinished share (above). Exit with
  code 130.

## Version

- `stashvent version` and `--version` print the version and the commit,
  set at build time: `-ldflags "-X main.version=<version> -X
  main.commit=<commit>"`. A release prints `stashvent v0.1.0 (commit
  1a2b3c4)` (`.goreleaser.yaml`), `make build` `stashvent dev (commit
  1a2b3c4)` (the tag instead of `dev` on a clean checkout of one, below),
  and a build without the commit only `stashvent dev`.
- `go install …/apps/stashvent@v0.1.0` sets no `-ldflags`: when `version`
  is still `dev`, `buildVersion` (`main.go`) takes the module version from
  `debug.ReadBuildInfo`, so it prints `stashvent v0.1.0`, without a commit.
  Only a release version counts (a tag, `-rc.1` included). A pseudo-version
  (`@main`, a commit) stays `dev`, because it would put the commit into the
  User-Agent, and so does a `+dirty` checkout. A clean checkout of a tag
  records the tag, so `make build` there prints `stashvent v0.1.0 (commit
  1a2b3c4)`.
- The folder is named after the binary (`apps/stashvent`) because
  `go install` names the binary after the last folder of its path.
- The User-Agent carries the version only (`stashvent/v0.1.0`), never the
  commit.

## Windows

The CLI must work the same on Windows, macOS, and Linux.

- Paths only via `filepath`, never string concatenation with `/`.
- No assumptions about `/tmp`; use `os.CreateTemp("", ...)`.
- Run `git` by name from `PATH` (`git.exe` on Windows); never a shell.
- Terminal detection and password prompts must work in Windows Terminal
  and PowerShell.
- CI runs the CLI tests on `windows-latest` as well as Linux and macOS.

## Testing

- Command tests run the whole CLI through `newCLI(...).run(args...)`
  (`harness_test.go`): buffers for the streams, a temp config directory
  (never the developer's), a fixed clock and password, queued answers for
  hidden prompts, and `fakeServer`, an in-memory `/v1` API with blob
  storage. Assert on stdout, stderr, and the exit code. `fakeServer`
  records the `Authorization` of every call (`authorization(route)`), can
  require keys (`requireKeys`), and can answer any error code
  (`failWithCode`).
- Real git in temp repositories: `git init -b main`, a repository-local
  `user.name`, and `git stash push -m`, so output is the same on every
  machine and git version.
- Golden files in `apps/stashvent/cmd/testdata/` for `push` (stdout, stderr),
  `pull` (stdout, stderr), the secret-scan warning, `list`, and the help of
  the root and each command (`login` and `logout` included). Rewrite them with
  `go test ./apps/stashvent/cmd -update`, and read the diff: output changes are
  deliberate. The test server's address is replaced by
  `https://stashvent.example`; where columns are aligned (`list`), build
  the input by hand instead.
- The end-to-end test is `TestCLIHappyPathOnDockerStack` in
  `apps/stashvent-server/cli_integration_test.go` (`make test-integration`): the
  server is `package main`, so the test lives there and drives the CLI
  through `cmd.Run`. `validation_integration_test.go` does the same for the
  repository checks (`TestRepoMismatchOnDockerStack`), the secret scan on a
  real push (`TestSecretScanOnDockerStack`: nothing is sent, no value is
  printed, nothing is stripped), and the storage cap.
- The built binary is tested in `e2e/` (`make test-e2e`, against `make
  run-server-e2e`): each "machine" is a temp dir set as `APPDATA`,
  `XDG_CONFIG_HOME`, `HOME`, and `TMP`/`TEMP`/`TMPDIR`, with every
  `STASHVENT_` and `GIT_` variable removed (the system git config stays: it
  is the machine's git install), so the real `os.UserConfigDir` and
  `os.CreateTemp` are used and checked (no `stashvent-*` file left, user
  files `0600`). Ctrl+C is tested there on Unix (`TestCtrlC`); on Windows it
  stays a manual check.
- Fixtures that look like secrets are assembled at run time (`"AKIA" +
  …`), so the repository's own scans don't flag the test files. The URL
  rule (`stashvent-url-password`) counts too: write a dev login as
  `"postgres://app:" + "app@host"`.
