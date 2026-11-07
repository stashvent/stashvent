# StashVent

**Send unfinished Git work to another developer. No WIP commit, no temporary
branch, no patch file. The server never sees your code.**

`stashvent push` packages a Git stash, encrypts it on your machine with a
password, and uploads it. The recipient runs `stashvent pull` with the link and
the password, and the stash appears in their own `git stash list`, ready for
`git stash apply`.

## Install

macOS and Linux:

```
curl -fsSL https://raw.githubusercontent.com/stashvent/stashvent/main/install.sh | sh
```

Windows (PowerShell):

```
irm https://raw.githubusercontent.com/stashvent/stashvent/main/install.ps1 | iex
```

The script installs the latest release for your system in your home folder (no
sudo or admin), checks it against the release's checksums, and adds it to your
`PATH`. Run it again to upgrade.
[RELEASES.md](https://github.com/stashvent/stashvent/blob/main/RELEASES.md#install) says how
to pin a version, install with `go install`, or download an archive by hand.
StashVent needs Git 2.32 or newer.

```
stashvent version
```

## Quickstart

The CLI uses the hosted service, `https://stashvent.dev`. During its closed beta,
pushing needs an API key: run `stashvent login` once with the one you were given
(see [API keys](#api-keys)). Pulling needs nothing.

```
# Sender, in the repository
git stash push -m "payment retry WIP"
stashvent push
#   Share link: https://stashvent.example/8xK29qLm4Tz7
#   Password:   Kj9nM2xP7qL5vW3bG8dFt4Rw
#   Expires:    in 6 hours

# Recipient, in their clone of the same repository
stashvent pull https://stashvent.example/8xK29qLm4Tz7
git stash apply
```

Send the link and the password through different channels. If the server
needs an API key for pushing, run `stashvent login` with the key its operator
gave you.

## What the server can and can't see

The stash is encrypted before it leaves your machine (AES-256-GCM, with a key
derived from the password by PBKDF2-SHA256). The password never reaches the
server.

| The server never sees | The server does see |
|---|---|
| the password | each share's size, creation time, and expiry |
| the code | how many times each share was looked up |
| the repository name, branch, base commit, and stash message | the salt and IV (not secret) |
| | with API keys on: which key created which share, and when |

Anyone with both the link **and** the password can import the stash until it
expires, is deleted, or reaches its pull limit.

## Limits

| | |
|---|---|
| Size | 5 MB of encrypted stash |
| Expiry | 6 hours by default; `--expires 1h`, `24h`, or `7d` |
| Pulls | 50 per share by default |
| Git | 2.32 or newer |

## API keys

On a server that requires them (the hosted service does during its beta),
pushing needs a key from the operator:

```
stashvent login
```

The key is sent only to the server it was saved for, only when pushing or
deleting, never when pulling. If you think your key was exposed, ask the
operator for a new one; they revoke the old one.

## More

This README also ships inside the release archives, so these links point to
the latest docs on GitHub, which may describe a newer version than yours:

- [DOCS.md](https://github.com/stashvent/stashvent/blob/main/DOCS.md): every
  command, choosing a server, the secret scan, scripts and CI, troubleshooting
- [SETUP.md](https://github.com/stashvent/stashvent/blob/main/SETUP.md):
  building, running, and testing StashVent yourself
- [RELEASES.md](https://github.com/stashvent/stashvent/blob/main/RELEASES.md):
  downloads, and how releases are made
- [SECURITY.md](https://github.com/stashvent/stashvent/blob/main/SECURITY.md):
  reporting a vulnerability privately
- [CONTRIBUTING.md](https://github.com/stashvent/stashvent/blob/main/CONTRIBUTING.md):
  reporting bugs and sending changes

## License

Copyright 2026 Hardik Tulsiani and the StashVent contributors. Licensed under
the [Apache License, Version 2.0](LICENSE); see also [NOTICE](NOTICE). The
release archives and the server image also carry the licenses of the libraries
StashVent is built from, in `third_party_licenses/`.
