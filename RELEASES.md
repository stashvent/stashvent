# Releases

All releases are on the **[releases page](https://github.com/stashvent/stashvent/releases)**.
This file says how to install one, and how a release is made.

- [Install](#install)
- [Making a release](#making-a-release)
- [When a release goes wrong](#when-a-release-goes-wrong)
- [How the pipeline works](#how-the-pipeline-works)

## Install

### Install script

```
# macOS and Linux
curl -fsSL https://raw.githubusercontent.com/stashvent/stashvent/main/install.sh | sh

# Windows (PowerShell)
irm https://raw.githubusercontent.com/stashvent/stashvent/main/install.ps1 | iex
```

The script ([`install.sh`](install.sh), [`install.ps1`](install.ps1)):

1. Finds your system and CPU, and the latest release: the highest version that
   isn't a pre-release.
2. Downloads that release's archive and `checksums.txt` from GitHub, and stops
   without installing anything if the archive's SHA-256 doesn't match.
3. Puts the binary in `~/.local/bin` (Windows: `%LOCALAPPDATA%\Programs\stashvent`),
   without sudo or admin.
4. Adds that folder to your `PATH` if it isn't there yet: one line in your
   shell's startup file, or your user `Path` on Windows. The file is `.zshrc`
   for zsh (in `ZDOTDIR` if you set one), where the line also moves the folder
   in front of `/usr/local/bin`; `~/.bashrc` for bash on Linux, and on macOS the
   first of `~/.bash_profile`, `~/.bash_login`, and `~/.profile` that exists;
   fish's `config.fish`; otherwise `~/.profile`.

If another `stashvent` is on your `PATH` (say, one you put in `/usr/local/bin` by
hand), the script tells you, so you can remove it.

Run it again to upgrade: it replaces the binary and prints the old and new
version, or says that version is already installed.

Another version, such as a release candidate:

```
curl -fsSL https://raw.githubusercontent.com/stashvent/stashvent/main/install.sh | sh -s -- --version v0.1.0-rc.1

$env:STASHVENT_VERSION = "v0.1.0-rc.1"; irm https://raw.githubusercontent.com/stashvent/stashvent/main/install.ps1 | iex
```

`install.sh` also reads `STASHVENT_VERSION`. Once an install succeeds, the
PowerShell script clears it in that window, so running the plain command again
installs the latest release; a failed install keeps it, so you can retry. A
`STASHVENT_VERSION` you set for good (a user environment variable, or an
`export` in your profile) pins every run; both scripts then say "from
STASHVENT_VERSION".

To read the script before running it, download it first:

```
curl -fsSLO https://raw.githubusercontent.com/stashvent/stashvent/main/install.sh
sh install.sh
```

To uninstall, delete the binary and the `PATH` line (or user `Path` entry) the
script added.

### go install

```
go install github.com/stashvent/stashvent/apps/stashvent@latest
```

This builds the CLI from source into `$(go env GOPATH)/bin`. Name a release
instead of `@latest` to pin it (`@v0.1.0`). `stashvent version` prints the
version without a commit.

### Download an archive by hand

Each release has one archive per system, named
`stashvent_<version>_<os>_<arch>`:

| System | File |
|---|---|
| Windows (most PCs) | `stashvent_<version>_windows_amd64.zip` |
| Windows on ARM (e.g. Snapdragon laptops) | `stashvent_<version>_windows_arm64.zip` |
| macOS, Apple silicon (M1 and later) | `stashvent_<version>_darwin_arm64.tar.gz` |
| macOS, Intel | `stashvent_<version>_darwin_amd64.tar.gz` |
| Linux, x86-64 | `stashvent_<version>_linux_amd64.tar.gz` |
| Linux, ARM64 | `stashvent_<version>_linux_arm64.tar.gz` |

Each archive holds one folder with the same name, containing the `stashvent`
binary, `LICENSE` (Apache-2.0), `NOTICE`, `README.md`, and
`third_party_licenses/`: the licenses and NOTICE files of the libraries
StashVent's CLI is built from. Every archive gets the same set, Windows' (one
library more than Linux and macOS need). For the five MPL-2.0 libraries it
also holds their source, as that license asks.

The two **Source code** files are added by GitHub to every release and hold no
binaries.

**1. Check the download** against `checksums.txt` from the same release:

```
# Linux: in the folder with both files
sha256sum --ignore-missing -c checksums.txt

# macOS: compare with the line for your file in checksums.txt
shasum -a 256 stashvent_<version>_darwin_arm64.tar.gz

# Windows (PowerShell): the same comparison
Get-FileHash .\stashvent_<version>_windows_amd64.zip
```

**2. Unpack it and put the binary on your `PATH`:**

```
# macOS, Linux
tar -xzf stashvent_<version>_linux_amd64.tar.gz
sudo mv stashvent_<version>_linux_amd64/stashvent /usr/local/bin/   # or any folder on your PATH, e.g. ~/.local/bin
```

On Windows, unzip it, move `stashvent.exe` from the unzipped folder to a folder such as
`%LOCALAPPDATA%\Programs\stashvent`, and add that folder to your user `Path`
(Start → "Edit environment variables for your account"). Open a new terminal
afterwards.

`stashvent.exe` is a command-line program, not an installer: double-clicking it
only flashes a window. Run it from a terminal.

**3. Check it runs:**

```
stashvent version     # stashvent v0.1.0 (commit 1a2b3c4)
```

The binaries aren't signed yet. On macOS, the first run is blocked by
Gatekeeper; allow it with `xattr -d com.apple.quarantine /usr/local/bin/stashvent`.
On Windows, SmartScreen may warn once. Windows PCs with Smart App Control
turned on may block unsigned programs however they're installed.

**Pre-releases** (`v0.1.0-rc.1`) are marked as such on the releases page. They're
release candidates for testing; use the latest release without a suffix
otherwise.

While the repository is private, only people with access to it can open the
releases page, and neither the install scripts nor `go install` work.

## Making a release

Versions follow [semver](https://semver.org) with a `v` in front: `v0.1.0`,
`v0.1.1`, `v0.2.0`. A release candidate adds `-rc.N`: `v0.1.0-rc.1`. Each
version is tested as a release candidate first.

**1. Start from a green `main`.** Merge what goes into the release and wait for
CI on `main` to pass. The release workflow runs the tests again, but not lint,
govulncheck, or the end-to-end tests.

**2. Dry-run it** if `.goreleaser.yaml` or the build changed:

```
make release-snapshot
```

This runs the same GoReleaser build in Docker into `dist/` (gitignored),
without publishing: the six archives and `checksums.txt`.

**3. Tag `main` and push the tag:**

```
git checkout main && git pull
git tag -a v0.1.0-rc.1 -m "v0.1.0-rc.1"
git push origin v0.1.0-rc.1
```

Always tag a commit on `main`, never a feature branch: a squash merge leaves
the branch's commits out of `main`. The workflow doesn't check which branch a
tag is on. To be explicit, tag the commit itself:
`git tag -a v0.1.0-rc.1 <sha> -m "v0.1.0-rc.1"`.

**4. Watch Actions → Release.** It tests on Windows, macOS, and Linux, then
builds and publishes. When it's done, the release page has the six archives,
`checksums.txt`, and a changelog of the commits since the previous tag.

This releases the CLI only. The server has its own tags; see
[Releasing the server](#releasing-the-server).

**5. Test the release candidate** with the published files, not a local build:

- Download one archive, check it against `checksums.txt`, and run
  `stashvent version`. It must print the tag and the tagged commit.
- Run the end-to-end tests against the release binary. They also need
  `stashvent-server` in the same folder (the one inside the archive, next to
  `stashvent`); the release has only the CLI, so copy the local one in:

  ```
  make build
  cp bin/stashvent-server* <unpacked folder>/
  make run-server-e2e                                        # terminal 1
  STASHVENT_E2E_BIN=<unpacked folder> make test-e2e          # terminal 2
  ```

  In PowerShell, set `$env:STASHVENT_E2E_BIN = "<unpacked folder>"` first.
- Push and pull for real between two clones against a deployed server, with
  `STASHVENT_SERVER` set, and go through the manual Windows checks (Ctrl+C, the
  hidden prompts).

**6. Release it.** When the candidate is good, tag the same commit without the
suffix and push the tag:

```
git tag -a v0.1.0 <sha of the rc> -m "v0.1.0"
git push origin v0.1.0
```

### Fixes for an older version

While there is one line of releases, every tag is on `main`. Once `main` has
moved on (say, to v0.2 work) and v0.1 needs a fix, branch from the release tag,
cherry-pick the fix, and tag there:

```
git checkout -b release/v0.1 v0.1.0
git cherry-pick <fix>
git tag -a v0.1.1 -m "v0.1.1"
git push origin release/v0.1 v0.1.1
```

The same workflow publishes it, without marking it as the latest release: that
stays with the highest version, which is what the install scripts install.

## Releasing the server

The server image has its own tags, `server-v…`, apart from the CLI's `v…`, so
each can be released when it changes. Tag a commit on `main` whose CI passed:

```
git checkout main && git pull
git tag -a server-v0.1.0-rc.1 -m "server-v0.1.0-rc.1"
git push origin server-v0.1.0-rc.1
```

**Actions → Image** runs the Go tests (Linux, with the race detector), then
publishes:

| Image | Published for |
|---|---|
| `ghcr.io/stashvent/stashvent-server:server-v0.1.0-rc.1` | the tag itself; never moves |
| `ghcr.io/stashvent/stashvent-server:<commit sha>` | every build; never moves |
| `ghcr.io/stashvent/stashvent-server:latest` | final versions only (`server-v0.1.0`), never a pre-release |

The image holds the server, `/LICENSE`, `/NOTICE`, and
`/third_party_licenses` (the licenses and NOTICE files of the libraries the
server is built from), collected by go-licenses during the image build; if
that step fails, the build fails.

If the tests fail, nothing is published. To deploy, point the hosted server
at the version's image, e.g. `…:server-v0.1.0-rc.1`; it logs that version at
startup. Rolling back is pointing it at the previous version.

A server version and a CLI version with the same number aren't tied
together: the `/v1` API is what they share, and it only changes in ways
older clients understand (the `stashvent-api` skill, "Compatibility").

Between releases, **Run workflow** on a branch builds `:<commit sha>` (and
moves `:main` when run on `main`), after the same tests.

## When a release goes wrong

- **The workflow fails before publishing** (tests or build): nothing is
  released. Fix it on `main` and tag the next candidate (`-rc.2`).
- **A published release is broken:** delete the release on the releases page,
  then its tag:

  ```
  git push --delete origin v0.1.0-rc.1
  git tag -d v0.1.0-rc.1
  ```

  Fix it and tag a **new** version (`-rc.2`, or `v0.1.1` after a release).
  Never reuse a tag that was published: someone may already have downloaded
  it, and the checksums would no longer match.
- **A server image is broken:** point the hosted server back at the previous
  version's image, fix it, and tag a **new** server version (`server-v0.1.0-rc.2`).
  Never push a published server tag again: the image would be rebuilt under
  the same tag, so a deploy pinned to it would change without anyone saying so.
- **A server tag doesn't match `server-v0.1.0` or `server-v0.1.0-rc.1`:**
  the Image workflow fails before publishing. Delete the tag and push one in
  that form.

## How the pipeline works

| File | Role |
|---|---|
| `.goreleaser.yaml` | What is built: the CLI (`./apps/stashvent`) for Windows, macOS, and Linux on amd64 and arm64, with `CGO_ENABLED=0` and `-trimpath`; the archives (one folder each: binary, `LICENSE`, `NOTICE`, `README.md`, and the dependencies' licenses, collected by go-licenses before the build), `checksums.txt`, and the changelog |
| `.github/workflows/release.yml` | Runs on a pushed CLI tag (`v…`); tests on three systems, then GoReleaser with only `contents: write`. It names the current and previous CLI tags for GoReleaser, so `server-v…` tags never end up in a CLI version or changelog |
| `Makefile` (`release-snapshot`) | The same GoReleaser build in Docker, without publishing; also names the latest CLI tag |
| `.github/workflows/image.yml` | The server image on GHCR: on a pushed server tag (`server-v…`: `:server-v…`, `:<sha>`, `:latest` for final versions), or by hand between releases (`:<sha>`, `:main` on main); the tests run first either way. It builds `deploy/docker/server.Dockerfile`, which also collects the dependencies' licenses into the image |

- The version and commit are set at build time: `-X main.version=v<version>
  -X main.commit=<short commit>`. `make build` gives `stashvent dev (commit …)`,
  except on a clean checkout of a tag (below).
- `go install github.com/stashvent/stashvent/apps/stashvent@v0.1.0` passes no
  `-ldflags`, so the CLI falls back to the module version Go records:
  `stashvent v0.1.0`, without a commit. Only release versions count: `@main`,
  a commit, or a checkout with local changes prints `dev`. A clean checkout of
  a tag records the tag, so `make build` there prints the tag instead of `dev`:
  it's built from exactly that release's source.
- The server has no such fallback. Go module versions are the CLI's `v…` tags,
  so `go install …/apps/stashvent-server@v0.1.0` would report the CLI's
  version; the server image (`server-v…` tags) is the way to run a release,
  and a `go install` of the server logs `version=dev`.
- Each folder under `apps/` is named after its binary, because `go install`
  names the binary after the last folder of the path.
- `install.sh` and `install.ps1` build the download URL from the archive name
  (`stashvent_<version>_<os>_<arch>`, the folder inside it) and
  `checksums.txt`, and find the latest release through
  `/releases/latest`, which only CLI tags create. Change them together with
  `.goreleaser.yaml`'s archives or checksum name.
- GitHub's "latest" flag, which the install scripts follow, goes only to the
  highest final version: `release.yml` sets `STASHVENT_MAKE_LATEST` for
  GoReleaser's `make_latest`. Without it, a fix on an older line (v0.1.1 after
  v0.2.0) would become latest and downgrade everyone who runs the scripts.
- A tag with a pre-release suffix (`-rc.1`) becomes a GitHub pre-release.
- GoReleaser is pinned to the same version in the `Makefile`
  (`GORELEASER_IMAGE`) and `release.yml`; change both together.
- Not done yet: signing and notarization; package managers (winget, Scoop,
  Homebrew), which need the repository to be public, like `go install`.
