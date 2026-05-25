# Stops everything scripts/local-setup.ps1 brought up.
#
# Reads .shortlink-pids written by setup, kills each PID (gracefully first,
# then -Force after 5s), tears the docker compose stack down. Logs and the
# pid file are removed unless -KeepLogs is passed.
#
# Flags:
#   -KeepLogs   leave ./logs/ in place after stopping the binaries
#   -KeepData   skip `docker compose down -v` (preserve Postgres/MinIO data)
[CmdletBinding()]
param(
    [switch]$KeepLogs,
    [switch]$KeepData
)

$ErrorActionPreference = "Continue"   # never bail mid-teardown

$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

$PidFile        = Join-Path $RepoRoot ".shortlink-pids"
$ContainersFile = Join-Path $RepoRoot ".shortlink-containers"
$LogDir         = Join-Path $RepoRoot "logs"

# Host binaries we care about. Used by the backstop sweep below to identify
# orphan processes when the PID file is missing or stale. Limited to long-
# running daemons -- limits.exe is a short-lived CLI invoked during setup,
# so it's not in this list.
$HostBinaryNames = @('api', 'worker', 'observer', 'loadtest')
$HostBinaryPaths = $HostBinaryNames | ForEach-Object {
    (Join-Path $RepoRoot "bin\$_.exe").ToLowerInvariant()
}

function Get-OrphanHostBinaries {
    # Returns Process objects for any shortlink host binary that's still
    # alive AND whose exe lives under this repo's bin/. The path filter
    # is critical -- "api.exe" is a generic name and we must not kill
    # an unrelated process that happens to share it.
    Get-Process -ErrorAction SilentlyContinue | Where-Object {
        $HostBinaryNames -contains $_.ProcessName -and
        $_.Path -and
        ($HostBinaryPaths -contains $_.Path.ToLowerInvariant())
    }
}

Write-Host "==> Stopping host binaries" -ForegroundColor Cyan
$pidFilePids = @()
if (Test-Path $PidFile) {
    Get-Content $PidFile | ForEach-Object {
        if (-not $_) { return }
        $parts = $_ -split "\s+"
        if ($parts.Count -lt 2) { return }
        # $pid is a read-only automatic variable in PowerShell (the current
        # process PID); writing to it warns or silently no-ops depending on
        # version + strict mode. Use a different name.
        $procId = 0
        if (-not [int]::TryParse($parts[1], [ref]$procId)) {
            Write-Host "    skipping malformed line: '$_'" -ForegroundColor Yellow
            return
        }
        $name = $parts[0]
        $proc = Get-Process -Id $procId -ErrorAction SilentlyContinue
        if (-not $proc) {
            Write-Host "    $name (PID $procId) already gone" -ForegroundColor DarkGray
            return
        }
        Write-Host "    $name (PID $procId) -> stopping" -ForegroundColor DarkGray
        # Host binaries were launched with -NoNewWindow + redirected I/O, so
        # they have no main window. CloseMainWindow() returns false instantly
        # for them and WaitForExit(5000) then blocks for the full 5 s before
        # falling through to -Force -- ~20 s wasted across the four binaries.
        # Skip the dance and TerminateProcess directly; deferred cleanup in
        # the binaries won't run, which is acceptable for a local dev
        # teardown (compose down -v wipes the durable state).
        Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue
        $script:pidFilePids += $procId
    }
    Remove-Item $PidFile -ErrorAction SilentlyContinue
} else {
    Write-Host "    No .shortlink-pids file -- nothing to kill from the tracked list" -ForegroundColor DarkGray
}

# Backstop: PID-file kills above only cover the LAST setup run. Anything
# from a previous crashed setup, a back-to-back setup that overwrote this
# file, or a Stop-Process that failed silently above will still be alive
# and holding 8080/8081/9090/8090/8091. Sweep by name+path so we don't
# touch unrelated processes that happen to share a binary name.
Write-Host ""
Write-Host "==> Sweeping orphan shortlink host binaries (backstop)" -ForegroundColor Cyan
$orphanProcs = Get-OrphanHostBinaries | Where-Object { $pidFilePids -notcontains $_.Id }
if ($orphanProcs) {
    foreach ($p in $orphanProcs) {
        Write-Host "    $($p.ProcessName) (PID $($p.Id)) at $($p.Path) -> stopping" -ForegroundColor DarkGray
        Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    }
} else {
    Write-Host "    none found" -ForegroundColor DarkGray
}

# Wait for every killed PID (tracked + orphans) to actually exit before
# moving on. Stop-Process returns immediately but the kernel takes a beat
# to release handles; without this, ./logs/ removal below races and trips
# on api.log being held by a process that's "stopping but not yet gone".
$allKilledPids = @($pidFilePids) + ($orphanProcs | ForEach-Object { $_.Id })
foreach ($id in $allKilledPids | Sort-Object -Unique) {
    Wait-Process -Id $id -Timeout 5 -ErrorAction SilentlyContinue
}

# Container-mode binaries from setup.ps1 -ContainerMode. The file lists
# "name container-name" per line; stop+rm each so a follow-up setup run
# isn't blocked by a name collision.
if (Test-Path $ContainersFile) {
    Write-Host ""
    Write-Host "==> Stopping shortlink containers" -ForegroundColor Cyan
    Get-Content $ContainersFile | ForEach-Object {
        if (-not $_) { return }
        $parts = $_ -split "\s+"
        if ($parts.Count -lt 2) { return }
        $name = $parts[0]
        $cname = $parts[1]
        Write-Host "    $name -> docker rm -f $cname" -ForegroundColor DarkGray
        docker rm -f $cname 2>&1 | Out-Null
    }
    Remove-Item $ContainersFile -ErrorAction SilentlyContinue
}

# Backstop: if a previous setup crashed mid-way the file may be missing
# or stale. Sweep up any container matching the well-known names so the
# next setup run isn't blocked by ghost containers we never recorded.
Write-Host ""
Write-Host "==> Sweeping orphan shortlink containers (backstop)" -ForegroundColor Cyan
$orphans = docker ps -aq --filter "name=^shortlink-api$" --filter "name=^shortlink-worker$" --filter "name=^shortlink-observer$" 2>$null
if ($orphans) {
    docker rm -f $orphans.Split() 2>&1 | Out-Null
    Write-Host "    swept $($orphans.Split().Count) orphan(s)" -ForegroundColor DarkGray
} else {
    Write-Host "    none found" -ForegroundColor DarkGray
}

Write-Host ""
Write-Host "==> Bringing the docker compose stack down" -ForegroundColor Cyan
if ($KeepData) {
    docker compose -f deploy/docker-compose.yml down
} else {
    # -v drops named volumes too (Postgres data, MinIO buckets). Default for
    # a teardown is "give me a clean slate"; -KeepData preserves them.
    docker compose -f deploy/docker-compose.yml down -v
}

if (-not $KeepLogs -and (Test-Path $LogDir)) {
    Write-Host ""
    Write-Host "==> Removing ./logs/" -ForegroundColor Cyan
    # Best-effort: if our backstop above missed a zombie, it'd still be
    # holding api.log/worker.log/etc. open and Remove-Item would emit a
    # noisy stack trace per file. Swallow per-file failures and surface
    # a single actionable line instead.
    try {
        Remove-Item -Recurse -Force $LogDir -ErrorAction Stop
    } catch {
        Write-Host "    Could not fully remove $LogDir -- a process is still holding a log file." -ForegroundColor Yellow
        Write-Host "    Re-run teardown, or find the holder with: handle.exe (Sysinternals) / Resource Monitor." -ForegroundColor Yellow
    }
}

Write-Host ""
Write-Host "Teardown complete." -ForegroundColor Green
