# Security

StashVent encrypts a stash on the sender's machine; the server never sees the code or
the password. A flaw that breaks that, or that lets someone create, read, or delete a
share they shouldn't, is what this policy is for.

## Reporting a vulnerability

**Don't open a public issue.** Report it privately through GitHub:
**Security → Report a vulnerability** on
[this repository](https://github.com/stashvent/stashvent/security/advisories/new).
Only the maintainers see the report.

Please include:

- what an attacker can do, and what they need first (a share link, the password, an
  API key, access to the server or its storage);
- the steps to reproduce it, or a proof of concept;
- the version (`stashvent version`, or the server image tag) and your OS.

You'll get a reply within a week. Once a fix is released, an advisory is published.
There is no bug bounty: reports aren't paid.

## What's in scope

- The `stashvent` CLI: encryption and key derivation, the payload format, Git
  bundle handling, the secret scan, and what it stores in `history.json` and
  `config.json`.
- The `stashvent-server` API server and its operator commands: authorization of
  creating, completing, and deleting shares, API keys, rate limits and quotas, and
  what it stores or logs.
- The published server image and the release archives.
- The hosted service at `https://stashvent.dev`, including its website and
  GitHub sign-in. Their code isn't in this repository, but report problems with
  them the same way.

## By design, not vulnerabilities

These are documented trade-offs (see "What the server can and can't see" in the
[README](README.md)):

- Anyone with both the link and the password can import the stash until it expires,
  is deleted, or reaches its pull limit.
- Anyone with the link, even without the password, can use up a share's pulls. The
  server never sees the password, so it can't tell a real pull from a fake one.
- The server knows each share's size, creation time, expiry, and how often it was
  looked up, and with API keys on, which key created it. On the hosted service,
  each key belongs to a GitHub account.
- A server running without `REQUIRE_API_KEY` accepts shares from anyone who can reach
  it; it warns about this at startup.

## Supported versions

Fixes go into the latest release. StashVent is pre-1.0, so older releases don't get
backported fixes; upgrade to the latest.
