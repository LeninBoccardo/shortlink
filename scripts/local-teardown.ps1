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

$PidFile = Join-Path $RepoRoot ".shortlink-pids"
$LogDir  = Join-Path $RepoRoot "logs"

Write-Host "==> Stopping host binaries" -ForegroundColor Cyan
if (Test-Path $PidFile) {
    Get-Content $PidFile | ForEach-Object {
        if (-not $_) { return }
        $parts = $_ -split "\s+"
        if ($parts.Count -lt 2) { return }
        $name  = $parts[0]
        $pid   = [int]$parts[1]
        $proc  = Get-Process -Id $pid -ErrorAction SilentlyContinue
        if (-not $proc) {
            Write-Host "    $name (PID $pid) already gone" -ForegroundColor DarkGray
            return
        }
        Write-Host "    $name (PID $pid) -> stopping" -ForegroundColor DarkGray
        try {
            $proc.CloseMainWindow() | Out-Null
            $proc.WaitForExit(5000) | Out-Null
        } catch {}
        if (-not $proc.HasExited) {
            Stop-Process -Id $pid -Force -ErrorAction SilentlyContinue
        }
    }
    Remove-Item $PidFile -ErrorAction SilentlyContinue
} else {
    Write-Host "    No .shortlink-pids file -- nothing to kill" -ForegroundColor DarkGray
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
