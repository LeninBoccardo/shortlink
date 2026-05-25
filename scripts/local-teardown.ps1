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

Write-Host "==> Stopping host binaries" -ForegroundColor Cyan
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
    }
    Remove-Item $PidFile -ErrorAction SilentlyContinue
} else {
    Write-Host "    No .shortlink-pids file -- nothing to kill" -ForegroundColor DarkGray
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
    Remove-Item -Recurse -Force $LogDir
}

Write-Host ""
Write-Host "Teardown complete." -ForegroundColor Green
