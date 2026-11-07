# Using StashVent

StashVent sends unfinished Git work to another developer without a WIP commit,
a temporary branch, or a patch file. `stashvent push` packages a stash,
encrypts it on your machine with a password, and uploads it. The recipient runs
`stashvent pull`, types the password, and gets the stash in their own
`git stash list`. The server never sees the password or the code.

- [Install](#install)
- [Quickstart](#quickstart)
- [Choosing a server](#choosing-a-server)
- [API keys](#api-keys)
- [Commands](#commands)
- [The secret scan](#the-secret-scan)
- [Limits](#limits)
- [What the server can and can't see](#what-the-server-can-and-cant-see)
- [Files on your machine](#files-on-your-machine)
- [Scripts and CI](#scripts-and-ci)
- [Exit codes](#exit-codes)
- [Troubleshooting](#troubleshooting)

## Install

```
# macOS and Linux
curl -fsSL https://raw.githubusercontent.com/stashvent/stashvent/main/install.sh | sh

# Windows (PowerShell)
irm https://raw.githubusercontent.com/stashvent/stashvent/main/install.ps1 | iex
```

The script installs the latest release in `~/.local/bin`
(`%LOCALAPPDATA%\Programs\stashvent` on Windows), checks it against the
release's checksums, and adds the folder to your `PATH`. Run it again to
upgrade. [RELEASES.md](RELEASES.md#install) covers pinning a version,
`go install`, and downloading an archive by hand.

StashVent needs **Git 2.32 or newer** on your `PATH`. Check the install with:

```
stashvent version
```

## Quickstart

**Before you start**: the CLI uses the hosted service, `https://stashvent.dev`.
During its closed beta, pushing needs an API key. Save the one you were given,
once:

```
stashvent login
```

Only the sender needs a key: pulling never does, and a pull link names its own
server. To use another server, see [Choosing a server](#choosing-a-server).

**Sender**, in the repository with the work:

```
git stash push -m "payment retry WIP"
stashvent push
```

```
✓ Secret scan passed
✓ Packaged stash@{0}: On main: payment retry WIP (2 files)
✓ Encrypted locally
✓ Uploaded
Send the password through a different channel than the link.
Share link: https://stashvent.example/8xK29qLm4Tz7
Password:   Kj9nM2xP7qL5vW3bG8dFt4Rw
Expires:    2026-10-13 16:50 UTC (in 6 hours)
```

Send the link and the password through **different channels**, for example
the link in the pull request and the password in a direct message. The
password is shown only this once. Your stash stays in your own stash list.

**Recipient**, in their clone of the same repository:

```
stashvent pull https://stashvent.example/8xK29qLm4Tz7
```

```
Password:
✓ Downloaded
✓ Decrypted locally
✓ Repository matches
✓ Imported into stash@{0}
Run "git stash apply" to apply it.
```

Nothing in the working tree changes until you run `git stash apply`.

## Choosing a server

Every command that talks to a server picks it in this order:

1. `--server https://stash.example.com`
2. the `STASHVENT_SERVER` environment variable
3. the default, `https://stashvent.dev`, the hosted service

For a server on your own machine, use `http://localhost:8080`. To run a
server for your team, see [SELF_HOSTING.md](SELF_HOSTING.md).

```
# for one command
stashvent push --server https://stash.example.com

# for every command, e.g. in your shell profile
export STASHVENT_SERVER=https://stash.example.com        # macOS, Linux
$env:STASHVENT_SERVER = "https://stash.example.com"      # PowerShell
```

- A server must use `https://`. Plain `http://` is accepted only for
  `localhost` and loopback addresses.
- When the server isn't the default, `push` says so, e.g.
  `Server: https://stash.example.com (from STASHVENT_SERVER)`, because an
  environment variable decides where your work goes.
- `pull` with a full link uses the server in the link. A bare share ID
  (`stashvent pull 8xK29qLm4Tz7`) is looked up on your chosen server.

## API keys

A server can require an API key for pushing (the hosted service does, during
its beta). Its operator gives you the key. Pulling never needs one.

```
stashvent login                          # asks for the key at a hidden prompt
stashvent login --with-token < key.txt   # reads it from stdin
stashvent logout                         # forgets the key for this server
```

- The key is saved for the server chosen as above, and sent **only to that
  server, only on push and delete**. It is never sent on pull, and never to a
  server named in a link.
- A key is never read from the command line, and never printed.
- `STASHVENT_API_KEY` can hold the key instead (for CI). It wins over a saved
  key and goes to the server chosen for the command.
- If you think your key was exposed, ask the operator for a new one; they revoke
  the old one. `logout` only removes it from this machine.

## Commands

| Command | What it does |
|---|---|
| `stashvent push [stash]` | Encrypt and share a stash |
| `stashvent pull <link>` | Download, decrypt, and import a shared stash |
| `stashvent list` | List the shares sent from this machine |
| `stashvent delete <id>` | Delete a share before it expires |
| `stashvent login` / `logout` | Save or remove the API key for a server |
| `stashvent version` | Print the version and commit (also `--version`) |

`stashvent <command> --help` shows every flag with examples.

### push

```
stashvent push                 # the newest stash, stash@{0}
stashvent push 2               # stash@{2}; "stash@{2}" works too
stashvent push --expires 24h   # 1h, 6h (default), 24h, or 7d
stashvent push --password      # choose your own password, at a hidden prompt
```

- With no argument, several stashes, and a terminal, you're asked which one.
  The numbers are Git's own, so the answer means the same as `stashvent push N`.
- A password of your own needs at least 12 characters. It's asked for twice
  and never printed. It can't be given on the command line: use the prompt or
  `--password-stdin`.
- Stashes made with `git stash push --include-untracked` keep their untracked
  files.

### pull

```
stashvent pull https://stashvent.example/8xK29qLm4Tz7
```

- Run it inside a clone of the repository the stash came from. You get three
  tries at the password.
- The stash's base commit must exist in your clone. If it's missing, run
  `git fetch` and pull again.
- Each pull counts against the share's pull limit (see [Limits](#limits)).
  Wrong password tries within one pull don't.

### list

Shows the shares pushed from this machine that haven't expired, newest first.
It reads the local history only: shares pushed from another machine aren't
there, and nothing is checked with the server.

### delete

```
stashvent delete 8xK29qLm4Tz7
stashvent delete https://stashvent.example/8xK29qLm4Tz7
```

The link stops working at once. A share pushed from this machine is deleted
with the delete token kept in its history, always on the server it was pushed
to. A share pushed from another machine can be deleted with the API key that
pushed it, after `stashvent login` on this one. Stashes already pulled by
others are ordinary Git stashes and aren't touched.

## The secret scan

Before anything is encrypted or sent, `push` scans the stash for likely
secrets: cloud keys, tokens, private keys, passwords in URLs, StashVent API
keys. If it finds any, it shows where, never the values:

```
⚠ Potential secrets detected in stash@{0}: On main: config for staging

  .env:3  aws-access-token

Secret values are not shown. To review: git stash show -p --include-untracked 0
Remove them, or pass --ignore-secrets to share anyway.
```

At a terminal you're asked `Share anyway? [y/N]`. Without a terminal the push
is cancelled unless you pass `--ignore-secrets`.

## Limits

| Limit | Value |
|---|---|
| Size | 5 MB of encrypted stash |
| Expiry | `1h`, `6h` (default), `24h`, or `7d` |
| Pulls per share | 50 by default; a server can change it. After that, the sender pushes again |
| Own password | at least 12 characters |
| Git | 2.32 or newer |

A server can also limit how often you push, and with API keys, how many shares
a key creates per day. The message then says how long to wait.

## What the server can and can't see

The stash is encrypted on your machine (AES-256-GCM, with a key derived from the
password by PBKDF2-SHA256) before it's uploaded.

- **The server never sees** the password, the code, the repository name, the
  branch, the base commit, or the stash message. Those are all inside the
  encrypted package.
- **The server does see** each share's size, when it was created and expires,
  how often it was looked up, and the encryption parameters (salt and IV,
  which aren't secret). With API keys, it knows which key created which share.
- Anyone with the link **and** the password can import the stash until it
  expires, is deleted, or reaches its pull limit. Treat both like a secret,
  and send them through different channels.

## Files on your machine

StashVent keeps two files in your user config folder:

| System | Folder |
|---|---|
| Windows | `%AppData%\stashvent\` |
| macOS | `~/Library/Application Support/stashvent/` |
| Linux | `~/.config/stashvent/` (or `$XDG_CONFIG_HOME/stashvent/`) |

- `history.json`: shares pushed from this machine, with the delete token for
  each, so `list` and `delete` work. **Never passwords.**
- `config.json`: API keys from `stashvent login`, one per server.

Only you can read them: `0600` on macOS and Linux, your per-user `%AppData%` on
Windows. Expired shares are removed from the history automatically.

## Scripts and CI

StashVent never waits on a prompt without a terminal:

- Passwords come from stdin: `push --password-stdin`, `pull --password-stdin`.
- The API key comes from `STASHVENT_API_KEY`, or `login --with-token`.
- The stash picker is skipped (the newest stash is shared), yes/no questions
  count as No, and the secret scan cancels the push unless `--ignore-secrets`
  is given.

Results (link, password, expiry, `list` rows) go to stdout; progress, warnings,
and errors go to stderr. So `stashvent push > share.txt` captures only the
result.

```
echo "$PASSWORD" | stashvent push --password-stdin --expires 1h
echo "$PASSWORD" | stashvent pull --password-stdin "$LINK"
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error: network, Git, decryption, or server |
| 2 | Usage error: a bad flag or argument |
| 130 | Cancelled: Ctrl+C, or you answered No |

Ctrl+C cleans up first: temporary files are removed, and a share that was
being created is deleted again.

## Troubleshooting

| Message | What to do |
|---|---|
| `Base commit a82fd91 is missing.` | Run `git fetch`, then pull again |
| `Base commit a82fd91 is missing from this shallow clone.` | Run `git fetch --unshallow`, then pull again |
| `This stash comes from a different repository` | Pull inside a clone of the sender's repository |
| `Decryption failed: wrong password or corrupted share` | Check the password with the sender; you get three tries |
| `This share has been pulled the maximum number of times.` | Ask the sender to push it again |
| `StashVent needs Git 2.32 or newer` | Upgrade Git: https://git-scm.com/downloads |
| `This share uses a newer format.` | Upgrade stashvent (run the install command again), then pull again |
| `This share is …, more than this version of stashvent downloads` | Upgrade stashvent, then pull again |
| `The server reported an invalid size for this share` | The link's server sent a broken answer: ask the sender to push again |
| `The server at … returned an error (HTTP …): "…"` | The quoted text is that server's own explanation; if it asks for a newer stashvent, upgrade |
| `refusing insecure http:// server` | Use the server's `https://` address |
| `This server needs an API key to push.` | Run `stashvent login` with the key its operator gave you |
| `The server didn't accept your API key` | It was revoked or mistyped: log in again, or ask the operator for a new key |
