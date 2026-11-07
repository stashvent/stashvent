---
name: stashvent-git
description: Rules for StashVent's Git integration (internal/git) — running git safely from Go, packaging stashes as Git bundles, temp-file and temp-ref hygiene, and version checks. Load whenever writing or reviewing code that shells out to git, creates/imports bundles, touches stash refs, or handles decrypted bundle files.
---

# StashVent Git Integration Rules

The user only runs `stashvent push` / `pull`. Every git command runs under
the hood via Go `os/exec`. A decrypted bundle is **plaintext source code**, so
treat temp files like secrets.

## Running git

- Always `exec.CommandContext(ctx, "git", arg1, arg2, ...)` with separate
  arguments. **Never** build a shell string or call `sh -c` / `cmd /c`.
  `stash@{0}` then needs no quoting, and user input can't inject commands.
- Set `cmd.Dir` to the repo root (from `git rev-parse --show-toplevel`),
  never rely on the process's working directory.
- Validate user-supplied refs before passing them on: must match
  `^stash@\{\d+\}$` (a bare `N` is also accepted and normalized to
  `stash@{N}`, since PowerShell mangles unquoted braces), and must never
  start with `-` (option injection). Use `git.ParseStashRef`, then look the
  ref up in `git.ListStashes` (`git stash list --format=%gd%x00%H%x00%gs`).
- Values from another user's manifest (base commit, bundle ref) are
  validated before git sees them: SHAs must be full 40/64 hex characters.
- Set env for predictable, non-interactive output:
  `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C` (parseable messages),
  appended to `os.Environ()`.
- Capture stderr and include it in wrapped errors:
  `fmt.Errorf("git bundle create: %w: %s", err, stderr)`.
  Never include bundle contents or diff output in errors or logs.

## Packaging (Approach B — temp ref + `git stash store`, decided for MVP)

Stash commit structure: `^1` = base (HEAD at stash time), `^2` = index
commit, `^3` = untracked-files commit (only if stashed with `-u`).

Sender:
```
SHA  = git rev-parse --verify stash@{N}^{commit}
BASE = git rev-parse --verify SHA^1
git update-ref refs/share/<random> SHA
git bundle create <tmpfile> refs/share/<random> ^BASE
git update-ref -d refs/share/<random>
```

Recipient (after decrypt):
```
git bundle verify <tmpfile>      # missing prerequisite → suggest `git fetch`
git fetch <tmpfile> refs/share/<name>
git stash store -m "Shared from <sender> — <msg>" <resolved FETCH_HEAD sha>
```

- Resolve `FETCH_HEAD` to a SHA immediately after fetch; don't pass the
  symbolic name around.
- An imported stash can be pushed on. Its message already starts with
  "Shared from <sender> — ", so the recipient drops those parts
  (`withoutSharedPrefix` in `apps/stashvent/cmd/pull.go`) and names only the
  newest sender, and push parses the branch from the message without them
  (found in end-to-end testing, 2026-10-18: the prefix grew by one per hop,
  and the branch was lost).
- If that SHA is already in the stash list (e.g. pulling your own share),
  skip `git stash store` and report where it is: storing the commit already
  at `stash@{0}` adds nothing, and elsewhere it would add a duplicate.

## Repository identity and verification

- Repo hash = SHA-256 of the **sorted** root commits
  (`git rev-list --max-parents=0 <rev>`), joined with `\n`. Sorting keeps
  histories with several roots deterministic. The sender hashes from the
  stash's base commit, the recipient from `HEAD`.
- Repo name is display only: `owner/repo` from the origin URL's last two
  path segments (host, user, and token dropped), else the folder name.
- Branch comes from the stash message (`WIP on <branch>:` / `On <branch>:`),
  so it's the branch at stash time, not the current one.
- Recipient, before `git bundle verify`: check the base commit with
  `git cat-file -e <sha>^{commit}`.
  - Base present → import. A commit ID covers its whole history, so this
    proves the repositories are related; don't compare hashes (extra roots
    or shallow clones would make the same repo look different).
  - Base missing → only now use the repo hash: shallow clone
    (`rev-parse --is-shallow-repository`) → `git fetch --unshallow`;
    hashes differ → "different repository"; otherwise → `git fetch`.
  - A different HEAD is only a note (importing never touches the working tree).
- Record `packaging_method`, `bundle_version`, `object_format`, and
  `sender_git_version` in share metadata so Approach A can be added later
  without breaking existing shares.

## Temp refs

- Only under `refs/share/`. Never `refs/heads/`, `refs/tags/`, or `refs/stash`.
- Use a random suffix (`refs/share/<random>`), not a fixed `refs/share/tmp`,
  so concurrent or crashed runs don't collide.
- Delete with `defer` immediately after creation, so they're removed on
  every exit path.
- Nothing in this package ever runs `git push` or talks to a Git remote.

## Temp files (bundles)

- Create with `os.CreateTemp("", "stashvent-*.bundle")` (0600 on Unix).
  On Windows, POSIX perms don't apply; rely on the per-user temp dir.
- `defer os.Remove(path)` right after creation, so the file is removed on
  success **and** failure. Close before removing (Windows can't delete open files).
- Never write the decrypted bundle anywhere except the temp file. Never
  cache it, log its path at info level, or leave it for "debugging."

## Version checks

- Run `git --version` at the start of both `push` and `pull`; parse it
  tolerantly (`2.32.0.windows.2`, `2.39.3 (Apple Git-145)`).
- **Minimum: Git 2.32**, on both sender and recipient. The
  floor comes from `git stash show -p --include-untracked` (secret
  scanning). The full push/pull round trip is verified on 2.32.0.windows.2.
- Below 2.32, fail early with a clear message and upgrade link. Define the
  minimum once as a constant, never inline it.

## Secret scanning input

- Scan the diff (`git stash show -p --include-untracked <sha>`), not
  bundle bytes. Bundles are compressed packfiles, so regexes can't see
  secrets inside them.
- `git.StashPatch` builds the input as two patches: that diff
  (`Worktree`), and `git diff <sha>^1 <sha>^2` (`Index`). The bundle
  carries the index commit, so a change that was staged and then undone in
  the working tree is shared even though `git stash show` doesn't print it.
- Both commands get `git.patchFlags`, so user config and `.gitattributes`
  can't change the format, hide content, or run another program:
  - `--text`: a file Git calls binary (`-diff` attribute, UTF-16 text)
    would otherwise be one "Binary files differ" line with nothing to scan.
  - `-U0 --inter-hunk-context=0`: only changed lines, one hunk per change,
    so line numbers are exact (`diff.interHunkContext` would join hunks).
  - `--no-color --no-ext-diff --no-textconv --no-relative
    --submodule=short`.
- The `a/` and `b/` prefixes are pinned with config before the subcommand
  (`git.patchConfig`: `-c diff.noprefix=false -c diff.mnemonicPrefix=false
  -c diff.srcPrefix=a/ -c diff.dstPrefix=b/`). **Never pass `--src-prefix`
  or `--dst-prefix` to `git stash show`:** Git 2.55 on Ubuntu (GitHub's
  runners) then prints the paths with no prefix at all. `git diff` is not
  affected.
- When adding a diff call for scanning, build it with `git.patchArgs`; when
  Git gains a config key that changes patch output, pin it there. Test
  against more than the local Git: CI runs a much newer one.
- Patches are read with `patchOutput`, not `run`: `run` trims whitespace,
  and `patchOutput` stops at `git.MaxPatchBytes` (`ErrPatchTooLarge`)
  instead of reading a huge stash into memory.

## Testing

- Integration tests create a real throwaway repo with `t.TempDir()` and
  `git init`, make a stash, push and pull it between two repos, and assert
  on `git stash list`.
- Cover: untracked files (`-u`), staged-only changes, missing base commit
  on the recipient, and cleanup (no leftover `refs/share/*` or temp files
  after success *or* forced failure).
- Repository verification is also tested through a real pull
  (`TestRepoMismatchOnDockerStack`): an unrelated repository, a
  clone that hasn't fetched the base, a shallow clone, and a HEAD that moved
  on. Each refusal imports nothing and leaves no `refs/share/*`, and doing
  what the message says (`git fetch`, `git fetch --unshallow`) makes the same
  pull work. A shallow clone of a local path needs `git clone --no-local
  --depth 1`: a local clone ignores `--depth`.
