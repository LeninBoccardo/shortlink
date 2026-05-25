# One-command local bootstrap for ShortLink on Windows.
#
# What it does, in order:
#   1. Verifies prerequisites you must supply yourself (docker, go, git)
#   2. Auto-installs missing optional deps (make, optionally kind + helm)
#   3. Refuses to start if any of the required host ports are taken
#   4. Renders deploy/docker-compose.override.yml from config/local-limits.yaml
#      via cmd/limits; aborts if the requested caps don't fit the host
#   5. Brings up the docker compose infra (Postgres, PgBouncer, Redis,
#      MinIO, Prometheus, Grafana, redis/postgres-exporter)
#   6. Applies migrations and generates test API keys
#   7. Builds the shortlink binaries into ./bin
#   8. Launches api / worker / observer / loadtest in the background, tees
#      their stdout+stderr into ./logs/, records PIDs in .shortlink-pids
#   9. Waits for each /healthz, then opens the showcase page in your browser
#
# Re-run idempotently: existing healthy containers are reused, the PID file
# is rewritten with the fresh process IDs. Use scripts/local-teardown.ps1
# to stop everything cleanly.
#
# Flags:
#   -WithK8s        also install kind + helm (for the optional §11 walkthrough)
#   -NoOpen         skip the browser-open step (useful in CI / SSH sessions)
#   -SkipDeps       trust the host has everything; skip prereq + install checks
#   -ContainerMode  wrap api/worker/observer in `docker run --memory --cpus`
#                   on the compose network instead of running them as host
#                   processes. Loadtest stays on the host (it serves the
#                   showcase page and shells `docker stats`). Use to validate
#                   behaviour under the resource caps from local-limits.yaml.
#
# Run from the repo root:
#   .\scripts\local-setup.ps1
#   .\scripts\local-setup.ps1 -WithK8s
#   .\scripts\local-setup.ps1 -ContainerMode
[CmdletBinding()]
param(
    [switch]$WithK8s,
    [switch]$NoOpen,
    [switch]$SkipDeps,
    [switch]$ContainerMode
)

$ErrorActionPreference = "Stop"
$ProgressPreference    = "SilentlyContinue"   # quieter Invoke-WebRequest

# Anchor every relative path at the repo root regardless of where the script
# was invoked from. PSScriptRoot is the directory holding this file.
$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

$PidFile        = Join-Path $RepoRoot ".shortlink-pids"
$ContainersFile = Join-Path $RepoRoot ".shortlink-containers"
$LogDir         = Join-Path $RepoRoot "logs"

# Ports the script will refuse to clobber. If any of these are taken the
# script aborts with a clear "PID X holds port Y" message -- killing other
# people's processes from a setup script is a footgun.
$RequiredPorts = @(
    @{ Port = 8080; Owner = "api" },
    @{ Port = 8081; Owner = "worker" },
    @{ Port = 8090; Owner = "loadtest showcase page" },
    @{ Port = 8091; Owner = "loadtest webhook sink" },
    @{ Port = 9090; Owner = "observer hub" }
)

# Compose ports too -- if these are taken the docker compose up will fail
# anyway, but we'd rather catch it early with a friendly message.
$ComposePorts = @(
    @{ Port = 55432; Owner = "Postgres (compose)" },
    @{ Port = 16432; Owner = "PgBouncer (compose)" },
    @{ Port = 9091;  Owner = "Prometheus (compose)" },
    @{ Port = 3000;  Owner = "Grafana (compose)" },
    @{ Port = 9000;  Owner = "MinIO API (compose)" },
    @{ Port = 9001;  Owner = "MinIO console (compose)" },
    @{ Port = 6379;  Owner = "Redis (compose)" }
)

function Write-Step($msg) {
    Write-Host ""
    Write-Host "==> $msg" -ForegroundColor Cyan
}

function Write-Sub($msg) {
    Write-Host "    $msg" -ForegroundColor DarkGray
}

function Test-Command($name) {
    $cmd = Get-Command $name -ErrorAction SilentlyContinue
    return $null -ne $cmd
}

function Test-PortOwner($port) {
    # Returns the PID owning the port, or $null if free. Try the modern
    # Get-NetTCPConnection first; fall back to netstat parsing if the
    # cmdlet is unavailable (some PowerShell SKUs).
    try {
        $conn = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction Stop
        if ($conn) { return $conn[0].OwningProcess }
    } catch {
        $line = netstat -ano | Select-String -Pattern (":$port\s.*LISTENING")
        if ($line) {
            $parts = ($line[0].ToString() -split "\s+") | Where-Object { $_ }
            return [int]$parts[-1]
        }
    }
    return $null
}

function Assert-PortsFree($ports) {
    $taken = @()
    foreach ($p in $ports) {
        $owner = Test-PortOwner $p.Port
        if ($null -ne $owner) {
            $name = (Get-Process -Id $owner -ErrorAction SilentlyContinue).ProcessName
            if (-not $name) { $name = "(unknown)" }
            $taken += "  port $($p.Port) ($($p.Owner)) -> PID $owner ($name)"
        }
    }
    if ($taken.Count -gt 0) {
        Write-Host ""
        Write-Host "Refusing to start: required ports are in use." -ForegroundColor Red
        $taken | ForEach-Object { Write-Host $_ -ForegroundColor Red }
        Write-Host ""
        Write-Host "Free them (Stop-Process -Id <PID>) and re-run, or run scripts/local-teardown.ps1." -ForegroundColor Yellow
        exit 2
    }
}

function Install-WithManager($pkg, $description) {
    if (Test-Command "winget") {
        Write-Sub "Installing $description via winget..."
        winget install --id $pkg --silent --accept-package-agreements --accept-source-agreements
        return
    }
    if (Test-Command "choco") {
        Write-Sub "Installing $description via choco..."
        # choco package name conventions are sometimes different; the caller
        # passes a hashtable for these cases. For the common case (single
        # name) we just retry with the same string.
        choco install $pkg -y
        return
    }
    if (Test-Command "scoop") {
        Write-Sub "Installing $description via scoop..."
        scoop install $pkg
        return
    }
    Write-Host "No package manager available (winget/choco/scoop). Install $description manually, then re-run." -ForegroundColor Red
    exit 3
}

function Assert-Prereqs {
    Write-Step "Checking prerequisites"

    foreach ($req in @("docker", "go", "git")) {
        if (-not (Test-Command $req)) {
            Write-Host "Missing required tool: $req" -ForegroundColor Red
            Write-Host "  docker:  https://docs.docker.com/desktop/install/windows-install/" -ForegroundColor Yellow
            Write-Host "  go 1.26: https://go.dev/dl/" -ForegroundColor Yellow
            Write-Host "  git:     https://git-scm.com/download/win" -ForegroundColor Yellow
            exit 4
        }
        Write-Sub "$req ok"
    }

    # Docker CLI exists; confirm the daemon is reachable. docker info exits
    # non-zero when the daemon isn't running (Docker Desktop not started).
    try {
        docker info --format "{{.ServerVersion}}" 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "docker info failed" }
        Write-Sub "docker daemon ok"
    } catch {
        Write-Host "Docker CLI is on PATH but the daemon isn't responding. Start Docker Desktop and re-run." -ForegroundColor Red
        exit 4
    }
}

function Install-OptionalDeps {
    Write-Step "Installing optional deps (auto)"

    if (-not (Test-Command "make")) {
        Write-Sub "make not found"
        Install-WithManager "GnuWin32.Make" "GNU make"
        # winget installs make under Program Files (x86)\GnuWin32\bin -- not
        # always on PATH for the current session. Add it for this run so the
        # later steps don't fail; user can re-open PowerShell for a permanent
        # PATH or set it manually.
        $maybe = "C:\Program Files (x86)\GnuWin32\bin"
        if (Test-Path (Join-Path $maybe "make.exe")) {
            $env:Path = "$maybe;$env:Path"
            Write-Sub "make installed and added to PATH for this session"
        } elseif (Test-Command "make") {
            Write-Sub "make installed and already on PATH"
        } else {
            Write-Host "make installed but not on PATH; reopen PowerShell or add it manually." -ForegroundColor Yellow
        }
    } else {
        Write-Sub "make already installed"
    }

    if ($WithK8s) {
        foreach ($tool in @(@{ Cmd="kind"; Pkg="Kubernetes.kind" }, @{ Cmd="helm"; Pkg="Helm.Helm" }, @{ Cmd="kubectl"; Pkg="Kubernetes.kubectl" })) {
            if (-not (Test-Command $tool.Cmd)) {
                Write-Sub "$($tool.Cmd) not found"
                Install-WithManager $tool.Pkg $tool.Cmd
            } else {
                Write-Sub "$($tool.Cmd) already installed"
            }
        }
    }
}

function Render-Limits {
    # cmd/limits validates that config/local-limits.yaml fits the host's
    # detected capacity, then writes deploy/docker-compose.override.yml
    # (and deploy/k8s/values-local.yaml for the optional k8s walkthrough).
    # Failure here means the host can't accommodate the requested limits;
    # the error message lists the largest contributors so the user knows
    # what to shrink in config/local-limits.yaml.
    Write-Step "Computing local resource limits"
    & go run ./cmd/limits render
    if ($LASTEXITCODE -ne 0) { throw "cmd/limits render failed; edit config/local-limits.yaml or raise host.max_total_*" }
}

function Start-Stack {
    Write-Step "Bringing up docker compose infra"
    docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.override.yml up -d
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed" }

    Write-Sub "Waiting for Postgres + Redis to report healthy (up to 45s)..."
    $deadline = (Get-Date).AddSeconds(45)
    while ((Get-Date) -lt $deadline) {
        $pgReady    = (docker inspect --format "{{.State.Health.Status}}" shortlink-postgres-1 2>$null) -eq "healthy"
        $redisReady = (docker inspect --format "{{.State.Health.Status}}" shortlink-redis-1    2>$null) -eq "healthy"
        if ($pgReady -and $redisReady) {
            Write-Sub "Postgres + Redis healthy"
            return
        }
        Start-Sleep -Seconds 2
    }
    throw "Postgres or Redis never reported healthy after 45s. Inspect with: docker compose -f deploy/docker-compose.yml ps"
}

function Apply-Migrations {
    Write-Step "Applying migrations"
    & go run ./cmd/migrate up
    if ($LASTEXITCODE -ne 0) { throw "migrations failed" }
}

function Generate-Keys {
    Write-Step "Generating test API keys"
    $keysFile = Join-Path $RepoRoot "config\keys.yaml"
    if (Test-Path $keysFile) {
        Write-Sub "config\keys.yaml already exists, leaving it alone (run cmd/keygen manually to rotate)"
        return
    }
    & go run ./cmd/keygen
    if ($LASTEXITCODE -ne 0) { throw "keygen failed" }
}

function Build-Binaries {
    Write-Step "Building host binaries into .\bin"
    & go build -o bin\ ./cmd/...
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}

function Build-ShortlinkImages {
    # Only needed in container mode. Same Dockerfile used by `make images` /
    # the k8s walkthrough -- distroless-nonroot, static binary, image tag
    # shortlink-<svc>:dev. We always rebuild so a code change between runs
    # actually lands; layer caching makes a no-op rebuild quick.
    Write-Step "Building shortlink-<svc>:dev images for container mode"
    foreach ($svc in @("api", "worker", "observer")) {
        Write-Sub "docker build --build-arg BINARY=$svc -t shortlink-${svc}:dev ."
        docker build --build-arg BINARY=$svc -t "shortlink-${svc}:dev" .
        if ($LASTEXITCODE -ne 0) { throw "docker build for $svc failed" }
    }
}

function Start-Container($name, $port) {
    # Resource caps come from config/local-limits.yaml via cmd/limits get
    # (bin/limits.exe was built by Build-Binaries before we got here).
    $cpu = (& (Join-Path $RepoRoot "bin\limits.exe") get $name cpu).Trim()
    $mem = (& (Join-Path $RepoRoot "bin\limits.exe") get $name memory_mb).Trim()

    $containerName = "shortlink-$name"
    # Idempotent re-runs: remove any stale container with this name first.
    docker rm -f $containerName 2>&1 | Out-Null

    # Network-internal addresses for the compose-deployed services. pgbouncer,
    # redis, minio resolve via compose DNS. observer is one of OUR containers
    # so we address it by the explicit container name (no compose alias).
    # SSRF_ALLOWLIST must include host.docker.internal so worker can deliver
    # webhooks to the loadtest sink running on the host (:8091).
    $envArgs = @(
        "-e", "DATABASE_URL=postgres://shortlink:shortlink@pgbouncer:6432/shortlink?sslmode=disable",
        "-e", "REDIS_URL=redis://redis:6379",
        "-e", "MINIO_ENDPOINT=minio:9000",
        "-e", "OBSERVER_URL=http://shortlink-observer:9090",
        "-e", "SSRF_ALLOWLIST=host.docker.internal,127.0.0.1,localhost"
    )

    # --add-host: Docker Desktop auto-adds host.docker.internal, but Linux
    # Docker doesn't. Without this, worker -> loadtest sink (host:8091) silently
    # fails on Linux even though SSRF_ALLOWLIST permits the name.
    # --memory-swap=--memory disables swap (defaults to 2x memory otherwise),
    # so the cap actually caps. Otherwise the scaling panel teaches the wrong
    # lesson: "capped at 512M" but RSS+swap can reach 1024M.
    docker run -d --name $containerName --network shortlink_default `
        --memory "${mem}M" --memory-swap "${mem}M" --cpus $cpu `
        --add-host "host.docker.internal:host-gateway" `
        -p "${port}:${port}" `
        @envArgs `
        "shortlink-${name}:dev" 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "docker run for $name failed" }
    Add-Content -Path $ContainersFile -Value "$name $containerName"
    Write-Sub "$name -> $containerName (cpu=$cpu memory=${mem}M, port=$port)"
}

function Start-HostBinary($name, $exe, $argList, $envVars) {
    $logOut = Join-Path $LogDir "$name.log"
    $logErr = Join-Path $LogDir "$name.err"

    # Set env vars in the current scope; Start-Process inherits them. Save
    # and restore so different binaries can have different env without
    # leaking into each other.
    $saved = @{}
    foreach ($k in $envVars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        [Environment]::SetEnvironmentVariable($k, $envVars[$k])
    }
    try {
        # -NoNewWindow runs the child in the current console; PowerShell 7+
        # rejects combining it with -WindowStyle, so we don't pass the latter.
        $proc = Start-Process -FilePath $exe -ArgumentList $argList `
            -WorkingDirectory $RepoRoot `
            -RedirectStandardOutput $logOut `
            -RedirectStandardError $logErr `
            -PassThru -NoNewWindow
    } finally {
        foreach ($k in $envVars.Keys) {
            [Environment]::SetEnvironmentVariable($k, $saved[$k])
        }
    }
    Add-Content -Path $PidFile -Value "$name $($proc.Id)"
    Write-Sub "$name -> PID $($proc.Id), log: $logOut"
    # No return: the unassigned $proc object would otherwise leak its column
    # listing (NPM/PM/WS/CPU/Id/SI/ProcessName) into the script's stdout.
}

function Wait-ForHealthz($name, $url, $timeoutSec = 30) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            $resp = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 2
            if ($resp.StatusCode -eq 200) {
                Write-Sub "$name healthy ($url)"
                return
            }
        } catch {
            # not ready yet
        }
        Start-Sleep -Milliseconds 500
    }
    throw "$name never returned 200 from $url within $timeoutSec s. Check $LogDir\$name.err for startup errors."
}

function Remove-PreviousContainers {
    # Cleans containers recorded by a prior setup run (whether ContainerMode
    # or not this time). Without this, host->container->host sequences leave
    # stale `shortlink-{api,worker,observer}` containers running because the
    # file gets deleted while the containers stay alive. Also salvages
    # partial-failure state from the last run.
    if (-not (Test-Path $ContainersFile)) { return }
    Get-Content $ContainersFile | ForEach-Object {
        $parts = $_ -split '\s+', 2
        if ($parts.Count -ge 2 -and $parts[1]) {
            docker rm -f $parts[1] 2>&1 | Out-Null
        }
    }
    Remove-Item $ContainersFile
}

function Start-Binaries {
    Write-Step "Launching host binaries"
    New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
    if (Test-Path $PidFile) { Remove-Item $PidFile }
    New-Item -ItemType File -Path $PidFile | Out-Null
    Remove-PreviousContainers
    if ($ContainerMode) { New-Item -ItemType File -Path $ContainersFile | Out-Null }

    $apiExe      = Join-Path $RepoRoot "bin\api.exe"
    $workerExe   = Join-Path $RepoRoot "bin\worker.exe"
    $observerExe = Join-Path $RepoRoot "bin\observer.exe"
    $loadtestExe = Join-Path $RepoRoot "bin\loadtest.exe"

    if ($ContainerMode) {
        # api/worker/observer run inside the compose network with caps from
        # local-limits.yaml. Loadtest continues on the host (it serves the
        # showcase page and shells `docker stats` for the scaling panel).
        Start-Container "observer" 9090
        Wait-ForHealthz "observer" "http://localhost:9090/healthz"

        Start-Container "worker" 8081
        Wait-ForHealthz "worker"   "http://localhost:8081/healthz"

        Start-Container "api" 8080
        Wait-ForHealthz "api"      "http://localhost:8080/healthz"
    } else {
        $sharedEnv = @{
            SSRF_ALLOWLIST = "127.0.0.1,localhost,host.docker.internal"
        }

        Start-HostBinary "observer" $observerExe @() @{}
        Wait-ForHealthz "observer" "http://localhost:9090/healthz"

        Start-HostBinary "worker"   $workerExe   @() $sharedEnv
        Wait-ForHealthz "worker"   "http://localhost:8081/healthz"

        Start-HostBinary "api"      $apiExe      @() $sharedEnv
        Wait-ForHealthz "api"      "http://localhost:8080/healthz"
    }

    # loadtest with a long duration so the showcase page stays up after the
    # attack ends; user can Ctrl-C via teardown.
    Start-HostBinary "loadtest" $loadtestExe @("--duration=30m", "--grafana=http://localhost:3000") @{}
    Wait-ForHealthz "loadtest" "http://localhost:8090/healthz"
}

function Open-Showcase {
    if ($NoOpen) {
        Write-Sub "Skipping browser open (-NoOpen)"
        return
    }
    Write-Step "Opening the showcase page"
    Start-Process "http://localhost:8090"
}

# ---------------------------------------------------------------------------
# main

if (-not $SkipDeps) {
    Assert-Prereqs
    Install-OptionalDeps
}

Write-Step "Checking required host ports are free"
Assert-PortsFree $RequiredPorts
# Compose ports get a softer check: if compose is already running the
# containers it expects, the ports are "taken" by Docker itself, which is
# fine and means the user is re-running the script.
$composeRunning = (docker compose -f deploy/docker-compose.yml ps --services --status running 2>$null)
if (-not $composeRunning) {
    Assert-PortsFree $ComposePorts
} else {
    Write-Sub "Compose stack already up (services: $($composeRunning -join ', ')) -- skipping compose port check"
}

Render-Limits
Start-Stack
Apply-Migrations
Generate-Keys
Build-Binaries
if ($ContainerMode) { Build-ShortlinkImages }
Start-Binaries
Open-Showcase

Write-Host ""
Write-Host "Stack is up." -ForegroundColor Green
Write-Host "  Showcase page : http://localhost:8090" -ForegroundColor Green
Write-Host "  Observer hub  : http://localhost:9090" -ForegroundColor Green
Write-Host "  Grafana       : http://localhost:3000" -ForegroundColor Green
Write-Host "  Prometheus    : http://localhost:9091" -ForegroundColor Green
Write-Host "  Logs          : $LogDir" -ForegroundColor Green
Write-Host "  Teardown      : .\scripts\local-teardown.ps1" -ForegroundColor Green
