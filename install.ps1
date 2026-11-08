# Installs the stashvent CLI on Windows, in PowerShell:
#
#   irm https://raw.githubusercontent.com/stashvent/stashvent/main/install.ps1 | iex
#
# A version other than the latest (a release candidate works too); iex can't
# pass arguments, so it's an environment variable, which the script clears
# after a successful install so that the next run installs the latest again:
#
#   $env:STASHVENT_VERSION = "v0.1.0"; irm https://raw.githubusercontent.com/stashvent/stashvent/main/install.ps1 | iex
#
# It downloads the release zip for this PC and checksums.txt from GitHub,
# checks the zip's SHA-256, and puts stashvent.exe in
# %LOCALAPPDATA%\Programs\stashvent (no admin), adding that folder to the user
# Path if needed. Running it again replaces stashvent.exe with the latest
# release.
#
# iex runs this in the caller's session, so everything is inside one script
# block, the TLS setting is put back at the end, and errors are thrown, never
# "exit", which would close the caller's window. What it leaves behind on
# purpose: the install folder on this session's Path, and STASHVENT_VERSION
# cleared after a successful install.

& {
    $ErrorActionPreference = 'Stop'
    # The caller's strict mode would turn a missing property into an error.
    Set-StrictMode -Off
    # The progress bar makes Invoke-WebRequest much slower in Windows PowerShell.
    $ProgressPreference = 'SilentlyContinue'

    $repoUrl = 'https://github.com/stashvent/stashvent'

    function Get-Arch {
        try {
            $arch = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
        } catch {
            # Older .NET: a 32-bit PowerShell on 64-bit Windows sees x86 here,
            # and the real architecture in PROCESSOR_ARCHITEW6432.
            $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
        }
        switch ($arch) {
            'X64' { return 'amd64' }
            'AMD64' { return 'amd64' }
            'Arm64' { return 'arm64' }
            default { throw "there is no stashvent build for $arch Windows; see $repoUrl/releases" }
        }
    }

    # Get-StatusCode returns the HTTP status of a failed Invoke-WebRequest, or
    # 0 when there was no answer.
    function Get-StatusCode($err) {
        if ($err.Exception.Response) { return [int]$err.Exception.Response.StatusCode }
        return 0
    }

    # Invoke-Download runs Invoke-WebRequest with $params. GitHub's release
    # downloads sometimes answer 504 for a few seconds, so a request is tried
    # again (after 1, 2 and 4 seconds) when there is no answer or a 408, 429,
    # 500, 502, 503 or 504, like install.sh's curl --retry. The last error is
    # thrown. (-MaximumRetryCount needs PowerShell 7.1.)
    function Invoke-Download([hashtable]$params) {
        $delay = 1
        for ($try = 1; ; $try++) {
            try {
                return Invoke-WebRequest @params -UseBasicParsing
            } catch {
                $code = Get-StatusCode $_
                if ($try -gt 3 -or $code -notin 0, 408, 429, 500, 502, 503, 504) { throw }
                Start-Sleep -Seconds $delay
                $delay *= 2
            }
        }
    }

    # Save-ReleaseFile downloads $uri, a file of release $version, to
    # $outFile, or throws. Only a 404 means the release (or this file of it)
    # isn't there; any other status is GitHub's problem.
    function Save-ReleaseFile([string]$uri, [string]$outFile, [string]$version) {
        try {
            Invoke-Download @{ Uri = $uri; OutFile = $outFile } | Out-Null
        } catch {
            $code = Get-StatusCode $_
            if ($code -eq 404) { throw "could not download $uri; is $version on $repoUrl/releases?" }
            if ($code) { throw "GitHub answered $code for $uri; try again later" }
            throw "could not download ${uri}: $($_.Exception.Message)"
        }
    }

    # Get-LatestVersion follows the redirect of /releases/latest to the release
    # GitHub marks as latest (.../releases/tag/v0.1.0): the highest final
    # version, because the release workflow doesn't mark fixes on older lines
    # or pre-releases. GitHub's API would allow only 60 anonymous requests an
    # hour per address, which breaks shared networks and CI. Only the CLI's
    # v... tags make GitHub releases (server-v... tags publish images only); if
    # the server ever gets releases too, this must look for the newest v... tag instead.
    function Get-LatestVersion {
        $latest = "$repoUrl/releases/latest"
        $noRelease = "there is no release at $repoUrl/releases yet; to install a pre-release, set `$env:STASHVENT_VERSION"
        try {
            $response = Invoke-Download @{ Uri = $latest; Method = 'Head' }
        } catch {
            $code = Get-StatusCode $_
            if ($code -eq 404) { throw $noRelease }
            if ($code) { throw "GitHub answered $code for $latest; try again later" }
            throw "could not reach ${latest}: $($_.Exception.Message)"
        }
        # Windows PowerShell has the final URL in ResponseUri, PowerShell 7 in RequestMessage.
        $url = $response.BaseResponse.ResponseUri
        if (-not $url) { $url = $response.BaseResponse.RequestMessage.RequestUri }
        if ("$url" -notmatch '/releases/tag/(v[^/]+)$') { throw $noRelease }
        return $Matches[1]
    }

    # Get-InstalledVersion returns the version of the stashvent.exe in
    # installDir ("stashvent v0.1.0 (commit 1a2b3c4)" gives v0.1.0), or ''.
    function Get-InstalledVersion {
        if (-not (Test-Path -LiteralPath $target)) { return '' }
        try {
            $fields = -split "$(& $target version)"
            if ($fields.Count -ge 2) { return $fields[1] }
        } catch {}
        return ''
    }

    # Test-PathEntry says whether a Path value (';'-separated, possibly with
    # %VARIABLES%) lists installDir.
    function Test-PathEntry([string]$path) {
        foreach ($entry in $path -split ';') {
            if ([Environment]::ExpandEnvironmentVariables($entry).TrimEnd('\') -eq $installDir) { return $true }
        }
        return $false
    }

    # Add-UserPath adds installDir to the user Path, once, and to this session's.
    function Add-UserPath {
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
        try {
            # The raw value, written back as REG_EXPAND_SZ: going through
            # [Environment]::SetEnvironmentVariable would expand entries like
            # %USERPROFILE%\bin and store them as plain text.
            $raw = "$($key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames))"
            $added = -not (Test-PathEntry $raw)
            if ($added) {
                $entries = @($raw -split ';' | Where-Object { $_ }) + $installDir
                $key.SetValue('Path', ($entries -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
            }
        } finally {
            $key.Close()
        }
        if ($added) {
            # Tell running programs, such as Explorer, that the environment
            # changed, so new terminals see the Path: .NET broadcasts that after
            # any change to a user variable, also the removal of one that doesn't exist.
            [Environment]::SetEnvironmentVariable('STASHVENT_INSTALLER', $null, 'User')
            Write-Host "Added $installDir to your user Path. Terminals that are already open need a restart."
        }
        if (-not (Test-PathEntry $env:Path)) {
            $env:Path = "$env:Path;$installDir"
        }
    }

    # Write-ShadowNote says when another stashvent.exe comes first on the Path.
    function Write-ShadowNote {
        $found = Get-Command stashvent -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($found -and $found.Source -ne $target) {
            Write-Host "Note: $($found.Source) comes first on your Path, so `"stashvent`" runs that one. Remove it to use $target."
        }
    }

    function Install-Stashvent {
        $version = $env:STASHVENT_VERSION
        $pinned = if ($version) { ' (from STASHVENT_VERSION)' } else { '' }
        $arch = Get-Arch
        if (-not $version) { $version = Get-LatestVersion }
        if (-not $version.StartsWith('v')) { $version = "v$version" }

        $old = Get-InstalledVersion
        if ($old -eq $version) {
            Write-Host "stashvent $version$pinned is already installed in $installDir."
        } else {
            Install-Release $version $arch $pinned
            if ($old) {
                Write-Host "Installed stashvent $version in $installDir (was $old)."
            } else {
                Write-Host "Installed stashvent $version in $installDir."
            }
            if ((Get-InstalledVersion) -ne $version) {
                Write-Host "Note: $target doesn't run on this PC, or reports another version; try running it."
            }
        }

        Add-UserPath
        Write-ShadowNote
        if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
            Write-Host "Note: StashVent needs Git 2.32 or newer, and git isn't on your Path."
        }
    }

    function Install-Release([string]$version, [string]$arch, [string]$pinned) {
        $name = "stashvent_$($version.Substring(1))_windows_$arch"
        $asset = "$name.zip"
        $base = "$repoUrl/releases/download/$version"
        $tmp = Join-Path ([IO.Path]::GetTempPath()) "stashvent-install-$([Guid]::NewGuid())"
        $new = "$target.new"
        $previous = $null
        New-Item -ItemType Directory -Path $tmp | Out-Null
        try {
            Write-Host "Downloading stashvent $version$pinned for windows/$arch..."
            Save-ReleaseFile "$base/$asset" "$tmp\$asset" $version
            Save-ReleaseFile "$base/checksums.txt" "$tmp\checksums.txt" $version

            $want = foreach ($line in Get-Content -LiteralPath "$tmp\checksums.txt") {
                $fields = -split $line
                if ($fields.Count -eq 2 -and $fields[1] -eq $asset) { $fields[0] }
            }
            if (-not $want) { throw "checksums.txt has no line for $asset; nothing was installed" }
            $got = (Get-FileHash -LiteralPath "$tmp\$asset" -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($got -ne "$want".ToLowerInvariant()) {
                throw "checksum mismatch for $asset (expected $want, got $got); nothing was installed"
            }

            New-Item -ItemType Directory -Force -Path $installDir | Out-Null
            # Only stashvent.exe: the zip also holds hundreds of license files,
            # which Expand-Archive (and the antivirus) would take seconds over.
            Add-Type -AssemblyName System.IO.Compression.FileSystem
            $zip = [IO.Compression.ZipFile]::OpenRead("$tmp\$asset")
            try {
                $entry = $zip.GetEntry("$name/stashvent.exe")
                if (-not $entry) { throw "$asset has no $name/stashvent.exe; nothing was installed" }
                [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $new, $true)
            } finally {
                $zip.Dispose()
            }

            # Old binaries from earlier installs; one that still runs stays until next time.
            Get-ChildItem -LiteralPath $installDir -Filter 'stashvent.exe.*.old' |
                Remove-Item -Force -ErrorAction SilentlyContinue
            # A running stashvent.exe can't be overwritten, but it can be renamed;
            # each time to a new name, in case an earlier one is still running too.
            if (Test-Path -LiteralPath $target) {
                $previous = "$target.$([Guid]::NewGuid().ToString('N').Substring(0, 8)).old"
                Move-Item -LiteralPath $target -Destination $previous
            }
            Move-Item -LiteralPath $new -Destination $target
            if ($previous) { Remove-Item -LiteralPath $previous -Force -ErrorAction SilentlyContinue }
        } finally {
            # Also after a failure or Ctrl+C (which skips catch blocks) between
            # the two moves: never leave the folder without stashvent.exe.
            if ($previous -and -not (Test-Path -LiteralPath $target) -and (Test-Path -LiteralPath $previous)) {
                Move-Item -LiteralPath $previous -Destination $target
            }
            Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath $new -Force -ErrorAction SilentlyContinue
        }
    }

    # $IsWindows only exists in PowerShell 6 and later; Windows PowerShell is always on Windows.
    if ($PSVersionTable.PSEdition -eq 'Core' -and -not $IsWindows) {
        throw "install.ps1 is for Windows; on macOS and Linux, use install.sh: see $repoUrl#install"
    }
    $installDir = Join-Path $env:LOCALAPPDATA 'Programs\stashvent'
    $target = Join-Path $installDir 'stashvent.exe'

    # Windows PowerShell 5.1 on older Windows may not offer TLS 1.2, which
    # GitHub requires. The setting is process-wide, so it's put back.
    $protocol = [Net.ServicePointManager]::SecurityProtocol
    [Net.ServicePointManager]::SecurityProtocol = $protocol -bor [Net.SecurityProtocolType]::Tls12
    try {
        Install-Stashvent
        # Cleared only now, so that a failed install can simply be retried.
        if (Test-Path Env:STASHVENT_VERSION) { Remove-Item Env:STASHVENT_VERSION }
    } catch {
        # Thrown again, so that a script or CI step running this fails too.
        throw "stashvent was not installed: $($_.Exception.Message)"
    } finally {
        [Net.ServicePointManager]::SecurityProtocol = $protocol
    }
}
