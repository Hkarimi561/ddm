# ddm installer for Windows.
#
#   powershell -c "irm https://<owner>.github.io/<repo>/install.ps1 | iex"
#
# Environment variables:
#   DDM_VERSION         release tag to install, e.g. v0.1.0 (default: latest)
#   DDM_INSTALL_DIR     where to put ddm.exe (default: %LOCALAPPDATA%\Programs\ddm)
#   DDM_REPO            GitHub repository to download from (owner/name)
#   DDM_BASE_URL        download archives from here instead of GitHub Releases
#                       (a URL or a local folder)
#   DDM_ARCH            override architecture detection (amd64 or arm64)
#   DDM_NO_MODIFY_PATH  set to 1 to leave the user PATH alone
#   DDM_NO_SERVICE      set to 1 to skip starting the web UI at login
#
# Works in Windows PowerShell 5.1 and PowerShell 7. Everything runs inside a
# script block so nothing leaks into the session that ran `iex`, and errors
# are thrown rather than calling `exit`, which would close that session.

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with the progress bar on 5.1
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = if ($env:DDM_REPO) { $env:DDM_REPO } else { '__DDM_REPO__' }
    if ($repo -like '__DDM_*') { $repo = 'Hkarimi561/ddm' }
    $version = if ($env:DDM_VERSION) { $env:DDM_VERSION } else { 'latest' }

    $arch = $env:DDM_ARCH
    if (-not $arch) {
        # PROCESSOR_ARCHITEW6432 is set when 32-bit PowerShell runs on 64-bit Windows.
        $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
        switch ($cpu) {
            'AMD64' { $arch = 'amd64' }
            'ARM64' { $arch = 'arm64' }
            default { throw "ddm install: unsupported CPU architecture '$cpu' (ddm ships amd64 and arm64 builds)" }
        }
    }

    $base = $env:DDM_BASE_URL
    if (-not $base) {
        if ($version -eq 'latest') {
            $base = "https://github.com/$repo/releases/latest/download"
        } else {
            if (-not $version.StartsWith('v')) { $version = "v$version" }
            $base = "https://github.com/$repo/releases/download/$version"
        }
    }
    $base = $base.TrimEnd('/', '\')

    function Get-Asset([string] $name, [string] $dest) {
        if ($base -match '^https?://') {
            Invoke-WebRequest -Uri "$base/$name" -OutFile $dest -UseBasicParsing
        } else {
            Copy-Item -LiteralPath (Join-Path ($base -replace '^file:///?', '') $name) -Destination $dest
        }
    }

    $asset = "ddm_windows_$arch.zip"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("ddm-install-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading $asset ($version) from $base"
        $zip = Join-Path $tmp $asset
        $sums = Join-Path $tmp 'checksums.txt'
        Get-Asset $asset $zip
        Get-Asset 'checksums.txt' $sums

        $want = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            $parts = $line -split '\s+', 2
            if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $asset) { $want = $parts[0].ToLowerInvariant() }
        }
        if (-not $want) { throw "ddm install: checksums.txt has no entry for $asset" }
        $got = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($got -ne $want) { throw "ddm install: checksum mismatch for $asset (expected $want, got $got)" }

        $unpacked = Join-Path $tmp 'unpacked'
        Expand-Archive -LiteralPath $zip -DestinationPath $unpacked -Force
        $exe = Join-Path $unpacked 'ddm.exe'
        if (-not (Test-Path -LiteralPath $exe)) { throw "ddm install: $asset does not contain ddm.exe" }

        $dir = if ($env:DDM_INSTALL_DIR) { $env:DDM_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\ddm' }
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
        $target = Join-Path $dir 'ddm.exe'
        try {
            Copy-Item -LiteralPath $exe -Destination $target -Force
        } catch {
            throw "ddm install: could not write $target. If ddm is running, stop it and try again. ($_)"
        }

        $installed = try { (& $target --version 2>$null) -join ' ' } catch { 'ddm' }
        if (-not $installed) { $installed = 'ddm' }
        Write-Host "Installed $installed to $target"

        $onPath = ($env:Path -split ';' | ForEach-Object { $_.TrimEnd('\') }) -contains $dir.TrimEnd('\')
        if (-not $onPath) {
            if ($env:DDM_NO_MODIFY_PATH -eq '1') {
                Write-Host ""
                Write-Host "$dir is not on your PATH; add it to run ddm from anywhere."
            } else {
                $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
                $entries = if ($userPath) { $userPath -split ';' | ForEach-Object { $_.TrimEnd('\') } } else { @() }
                if ($entries -notcontains $dir.TrimEnd('\')) {
                    $newPath = if ($userPath) { "$($userPath.TrimEnd(';'));$dir" } else { $dir }
                    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
                    Write-Host "Added $dir to your user PATH (open a new terminal to pick it up)."
                }
                $env:Path = "$env:Path;$dir"
            }
        }

        if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
            Write-Host ""
            Write-Host "Note: ddm needs Docker to load images: https://docs.docker.com/desktop/setup/install/windows-install/"
        }
        if ($env:DDM_NO_SERVICE -ne '1') {
            # Starts the settings web UI now and at every login, and prints its
            # address and password.
            Write-Host ""
            & $target service install
            if ($LASTEXITCODE -ne 0) { Write-Host "The web UI could not be set up; start it with: ddm serve" }
        }
        Write-Host ""
        Write-Host "Get started:  ddm pull nginx:latest"
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
