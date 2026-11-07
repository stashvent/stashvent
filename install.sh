#!/bin/sh
# Installs the stashvent CLI on macOS and Linux:
#
#   curl -fsSL https://raw.githubusercontent.com/stashvent/stashvent/main/install.sh | sh
#
# A version other than the latest (a release candidate works too):
#
#   curl -fsSL https://raw.githubusercontent.com/stashvent/stashvent/main/install.sh | sh -s -- --version v0.1.0
#
# It downloads the release archive for this system and checksums.txt from
# GitHub, checks the archive's SHA-256, and puts the binary in ~/.local/bin
# (no sudo), adding that folder to PATH if needed. Running it again replaces
# the binary with the latest release.
#
# Everything runs from main on the last line, so a download cut off halfway
# runs nothing. Commands that could read stdin get </dev/null: with
# "curl ... | sh", stdin is this script.

set -eu

repo_url=https://github.com/stashvent/stashvent

usage() {
	cat <<EOF
Installs the latest stashvent release into ~/.local/bin.

Usage: install.sh [--version <tag>]

  --version <tag>  install this release instead, e.g. v0.1.0
                   (also read from STASHVENT_VERSION)
EOF
}

say() { printf '%s\n' "$*"; }

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is needed to install stashvent"
}

detect_os() {
	case $(uname -s) in
	Darwin) echo darwin ;;
	Linux) echo linux ;;
	MINGW* | MSYS* | CYGWIN*) fail "on Windows, run install.ps1 in PowerShell instead: see $repo_url#install" ;;
	*) fail "there is no stashvent build for $(uname -s); see $repo_url/releases" ;;
	esac
}

detect_arch() {
	case $(uname -m) in
	x86_64 | amd64)
		# A shell running under Rosetta 2 on Apple silicon reports x86_64;
		# take the native build there.
		if [ "$os" = darwin ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
			echo arm64
		else
			echo amd64
		fi
		;;
	arm64 | aarch64) echo arm64 ;;
	*) fail "there is no stashvent build for $(uname -m) CPUs; see $repo_url/releases" ;;
	esac
}

# latest_version follows the redirect of /releases/latest to the release
# GitHub marks as latest (.../releases/tag/v0.1.0): the highest final
# version, because the release workflow doesn't mark fixes on older lines or
# pre-releases. GitHub's API would allow only 60 anonymous requests an hour
# per address, which breaks shared networks and CI. Only the CLI's v... tags
# make GitHub releases (server-v... tags publish images only); if the server
# ever gets releases too, this must look for the newest v... tag instead.
latest_version() {
	latest="$repo_url/releases/latest"
	answer=$(curl -sSLI -o /dev/null -w '%{http_code} %{url_effective}' "$latest") ||
		fail "could not reach $latest"
	code=${answer%% *}
	url=${answer#* }
	case $code in
	200) ;;
	404) url= ;;
	*) fail "GitHub answered $code for $latest; try again later" ;;
	esac
	case $url in
	*/releases/tag/v*) echo "${url##*/}" ;;
	*) fail "there is no release at $repo_url/releases yet; to install a pre-release, name it with --version" ;;
	esac
}

# installed_version prints the version of the stashvent in install_dir
# ("stashvent v0.1.0 (commit 1a2b3c4)" gives v0.1.0), or nothing.
installed_version() {
	if [ -x "$target" ]; then
		"$target" version </dev/null 2>/dev/null | awk '{ print $2 }'
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		fail "sha256sum or shasum is needed to check the download"
	fi
}

# zsh_dir prints the folder zsh reads .zshrc from: ZDOTDIR is often set in
# ~/.zshenv without being exported, so zsh itself is asked. The marker skips
# anything .zshenv prints.
zsh_dir() {
	dir=$(zsh -c 'printf "\nstashvent-zdotdir=%s\n" "${ZDOTDIR:-$HOME}"' </dev/null 2>/dev/null |
		sed -n 's/^stashvent-zdotdir=//p' | tail -n 1) || true
	echo "${dir:-${ZDOTDIR:-$HOME}}"
}

# profile_file prints the startup file the user's shell reads, where the PATH
# line goes.
profile_file() {
	shell=${SHELL:-}
	case ${shell##*/} in
	zsh)
		# .zshrc runs after /etc/zprofile, where macOS's path_helper reorders
		# PATH; a line in .zshenv would end up behind /usr/local/bin.
		echo "$(zsh_dir)/.zshrc"
		;;
	bash)
		if [ "$os" = darwin ]; then
			# Terminal on macOS starts login shells, which read only the first
			# of these that exists; creating .bash_profile next to an existing
			# .profile would hide the .profile.
			for f in .bash_profile .bash_login .profile; do
				if [ -f "$HOME/$f" ]; then
					echo "$HOME/$f"
					return
				fi
			done
			echo "$HOME/.bash_profile"
		else
			echo "$HOME/.bashrc"
		fi
		;;
	fish) echo "$HOME/.config/fish/config.fish" ;;
	*) echo "$HOME/.profile" ;;
	esac
}

# ensure_path adds install_dir to PATH in the shell's startup file, once.
ensure_path() {
	case ":$PATH:" in
	*":$install_dir:"*) return ;;
	esac
	profile=$(profile_file)
	# The line is written as is; $HOME and $PATH expand when the shell starts.
	# zsh's moves the folder to the front even if it's already on PATH (a
	# nested login shell on macOS gets it back behind /usr/local/bin); the
	# others add it once, so nested shells don't repeat it. fish_add_path
	# would be shorter, but needs fish 3.2.
	# shellcheck disable=SC2016
	case $profile in
	*.zshrc) line='path=("$HOME/.local/bin" ${path:#$HOME/.local/bin})' ;;
	*.fish) line='contains "$HOME/.local/bin" $PATH; or set -gx PATH "$HOME/.local/bin" $PATH' ;;
	*) line='case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac' ;;
	esac
	if ! grep -qsF "$line" "$profile"; then
		if { mkdir -p "$(dirname "$profile")" &&
			printf '\n# Added by the stashvent installer\n%s\n' "$line" >>"$profile"; } 2>/dev/null; then
			say "Added $install_dir to PATH in $profile."
		else
			say "Note: could not write $profile; add $install_dir to your PATH yourself."
		fi
	fi
	say "Open a new terminal to use stashvent, or run: export PATH=\"$install_dir:\$PATH\""
}

# warn_shadowed says when there's another stashvent on PATH: it may come
# first now, or after a new terminal (an older install in /usr/local/bin).
warn_shadowed() {
	found=$(command -v stashvent || true)
	if [ -n "$found" ] && [ "$found" != "$target" ]; then
		say "Note: there is another stashvent at $found. Remove it, so that \"stashvent\" always runs $target."
	fi
}

main() {
	[ -n "${HOME:-}" ] || fail "HOME is not set, so there is no ~/.local/bin to install into"
	install_dir="$HOME/.local/bin"
	target="$install_dir/stashvent"

	version=${STASHVENT_VERSION:-}
	# Said in the messages, so a STASHVENT_VERSION left in a profile doesn't
	# silently keep "run it again to upgrade" on an old release.
	pinned=${version:+ (from STASHVENT_VERSION)}
	while [ $# -gt 0 ]; do
		case $1 in
		--version)
			[ $# -ge 2 ] || fail "--version needs a release tag, e.g. --version v0.1.0"
			version=$2
			pinned=
			shift 2
			;;
		--version=*)
			version=${1#--version=}
			pinned=
			shift
			;;
		-h | --help)
			usage
			return
			;;
		*) fail "unknown argument: $1 (see --help)" ;;
		esac
	done

	need curl
	need tar
	os=$(detect_os)
	arch=$(detect_arch)
	if [ -z "$version" ]; then
		version=$(latest_version)
	fi
	case $version in
	v*) ;;
	*) version=v$version ;;
	esac

	old=$(installed_version)
	if [ "$old" = "$version" ]; then
		say "stashvent $version$pinned is already installed in $install_dir."
	else
		name="stashvent_${version#v}_${os}_${arch}"
		asset="$name.tar.gz"
		base="$repo_url/releases/download/$version"
		tmp=$(mktemp -d)
		trap 'rm -rf "$tmp" "$target.new"' EXIT
		trap 'exit 130' INT
		trap 'exit 143' TERM

		say "Downloading stashvent $version$pinned for $os/$arch..."
		curl -fsSL -o "$tmp/$asset" "$base/$asset" ||
			fail "could not download $base/$asset; is $version on $repo_url/releases?"
		curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" ||
			fail "could not download $base/checksums.txt"

		want=$(awk -v name="$asset" '$2 == name { print $1 }' "$tmp/checksums.txt")
		[ -n "$want" ] || fail "checksums.txt has no line for $asset; nothing was installed"
		got=$(sha256 "$tmp/$asset")
		[ "$got" = "$want" ] ||
			fail "checksum mismatch for $asset (expected $want, got $got); nothing was installed"

		# Only the binary: the archive also holds hundreds of license files.
		tar -xzf "$tmp/$asset" -C "$tmp" "$name/stashvent" ||
			fail "could not unpack $name/stashvent from $asset; nothing was installed"
		mkdir -p "$install_dir"
		cp "$tmp/$name/stashvent" "$target.new"
		chmod 755 "$target.new"
		mv -f "$target.new" "$target"

		if [ -n "$old" ]; then
			say "Installed stashvent $version in $install_dir (was $old)."
		else
			say "Installed stashvent $version in $install_dir."
		fi
		if [ "$(installed_version)" != "$version" ]; then
			say "Note: $target doesn't run on this system, or reports another version; try running it."
		fi
	fi

	ensure_path
	warn_shadowed
	command -v git >/dev/null 2>&1 || say "Note: StashVent needs Git 2.32 or newer, and git isn't on your PATH."
}

main "$@"
