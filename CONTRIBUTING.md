# Contributing to StashVent

Thanks for helping. Bug reports, fixes, tests, and docs are all welcome.

## Before you start

- **Security issues:** don't open an issue. Report them privately, as
  [SECURITY.md](SECURITY.md) describes.
- **Bugs:** open an issue with `stashvent version`, your OS, `git --version`,
  the command you ran, and what it printed. Never paste a share's password,
  a link together with its password, or an API key.
- **Larger changes** (a new command or flag, an API change, a new storage
  backend): open an issue first, so we can agree on the approach before you
  write the code.
- **Not in this repo:** the hosted service at `stashvent.dev`, its website,
  and its accounts. Questions about a hosted API key go to its operator, not
  to issues here.

## Making a change

[SETUP.md](SETUP.md) covers the tools, the first run, and every check, with
the exact commands. Before opening a pull request, run at least `make test`
and `make lint`, and depending on what you changed:

- the server or storage: `make test-integration` (needs `make up`)
- the CLI's push, pull, delete, or login flows: the end-to-end tests
  (`make run-server-e2e`, then `make test-e2e`)
- concurrency or Git handling: `make test-linux`, which runs the
  tests like CI's Linux job, with `-race` and a newer Git

CI runs the unit tests on Linux, Windows, and macOS, and lint, the database
checks, the integration tests, and the end-to-end tests (against the server
image) on Linux.

- **CLI output** is pinned by golden files: after a deliberate change,
  rewrite them as [SETUP.md](SETUP.md#cli-output) shows and check the diff.
- **Database:** schema and query changes follow
  [SETUP.md](SETUP.md#database), including committing the generated code.
- **Docs:** update [DOCS.md](DOCS.md), [SETUP.md](SETUP.md), or
  [RELEASES.md](RELEASES.md) when commands, flags, limits, make targets, or the
  release pipeline change.
- **Rules per area** live in `.claude/skills/`: crypto, Git, Postgres, the
  `/v1` API, the CLI, and general Go. They're written for Claude Code, and they
  are plain Markdown that holds for everyone.

## What must not break

- The server never sees the password or the code. Repository name, branch,
  base commit, and file names travel only inside the encrypted payload; a new
  server-side field must not reveal any of them.
- Installed CLIs stay in use for years. Within `/v1`, API changes are additive
  only, and the payload format and the files in the user's config folder are
  compatibility contracts: change them only in a new version of the format.
- The API key format and hash in `apikey/` are a contract too: keys already
  issued must keep working.
- The server's logs hold no secrets and nothing that identifies a person:
  never tokens, API keys, request headers (only a shortened User-Agent),
  presigned URLs, the request URL or its query string, request or response
  bodies, or client IP addresses. The stashvent-api skill has the full list,
  and `TestLogsHoldNoSecrets` checks it; extend the test when you add an
  endpoint.

## Pull requests

- One topic per pull request, with a description of what changes and why.
- Behaviour changes come with tests.
- Commit messages say what the change does, in the imperative ("Show the
  server's message for unknown error codes").

## License

StashVent is licensed under [Apache-2.0](LICENSE). By opening a pull request
you agree that your contribution is licensed under it too (section 5 of the
license); there's no contributor agreement or sign-off to fill in. You keep
the copyright in your changes, and the project's notice credits "Hardik
Tulsiani and the StashVent contributors".
