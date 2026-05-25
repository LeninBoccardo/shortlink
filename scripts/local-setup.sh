#!/usr/bin/env bash
# One-command local bootstrap for ShortLink on macOS and Linux.
#
# Mirrors scripts/local-setup.ps1: same prereqs, same auto-install policy,
# same end state. See that script's header for the full description.
#
# Flags:
#   --with-k8s        also install kind + helm (for the optional §11 walkthrough)
#   --no-open         skip the browser-open step (useful in CI / SSH)
#   --skip-deps       trust the host has everything; skip prereq + install checks
#   --container-mode  wrap api/worker/observer in `docker run --memory --cpus`
#                     on the compose network; loadtest stays on the host
#
# Run from the repo root:
#   ./scripts/local-setup.sh
#   ./scripts/local-setup.sh --with-k8s
#   ./scripts/local-setup.sh --container-mode

set -euo pipefail

WITH_K8S=0
NO_OPEN=0
SKIP_DEPS=0
CONTAINER_MODE=0
for arg in "$@"; do
    case "$arg" in
        --with-k8s)       WITH_K8S=1 ;;
        --no-open)        NO_OPEN=1 ;;
        --skip-deps)      SKIP_DEPS=1 ;;
        --container-mode) CONTAINER_MODE=1 ;;
        -h|--help)
            sed -n '2,16p' "$0"
            exit 0 ;;
        *)
            echo "unknown flag: $arg" >&2
            exit 64 ;;
    esac
done

# Anchor every relative path at the repo root regardless of where the script
# was invoked from.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname -- "$SCRIPT_DIR")"
cd "$REPO_ROOT"

PID_FILE="$REPO_ROOT/.shortlink-pids"
CONTAINERS_FILE="$REPO_ROOT/.shortlink-containers"
LOG_DIR="$REPO_ROOT/logs"

# Ports the script refuses to clobber if held by something else. Same set as
# the PowerShell version, kept in lockstep.
REQUIRED_PORTS=(
    "8080:api"
    "8081:worker"
    "8090:loadtest-showcase"
    "8091:loadtest-sink"
    "9090:observer"
)
COMPOSE_PORTS=(
    "55432:postgres-compose"
    "16432:pgbouncer-compose"
    "9091:prometheus-compose"
    "3000:grafana-compose"
    "9000:minio-api-compose"
    "9001:minio-console-compose"
    "6379:redis-compose"
)

# Colours degrade gracefully on terminals that don't understand them.
if [ -t 1 ]; then
    CYAN='\033[36m'; GREEN='\033[32m'; YELLOW='\033[33m'; RED='\033[31m'; DIM='\033[2m'; RESET='\033[0m'
else
    CYAN=''; GREEN=''; YELLOW=''; RED=''; DIM=''; RESET=''
fi

step() { printf "\n${CYAN}==> %s${RESET}\n" "$*"; }
sub()  { printf "${DIM}    %s${RESET}\n" "$*"; }
err()  { printf "${RED}%s${RESET}\n" "$*" >&2; }
warn() { printf "${YELLOW}%s${RESET}\n" "$*"; }

has_cmd() { command -v "$1" >/dev/null 2>&1; }

# port_owner PORT -> echoes PID owning :PORT, or empty if free.
# Tries lsof first (works on macOS and most Linuxes), falls back to ss.
port_owner() {
    local port=$1
    if has_cmd lsof; then
        lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null | head -1
    elif has_cmd ss; then
        ss -ltnp "( sport = :$port )" 2>/dev/null | awk 'NR>1 {print $7}' | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2
    fi
}

assert_ports_free() {
    local taken=()
    for entry in "$@"; do
        local port="${entry%%:*}"
        local owner="${entry##*:}"
        local pid
        pid=$(port_owner "$port" || true)
        if [ -n "${pid:-}" ]; then
            local name
            name=$(ps -p "$pid" -o comm= 2>/dev/null || echo "(unknown)")
            taken+=("  port $port ($owner) -> PID $pid ($name)")
        fi
    done
    if [ ${#taken[@]} -gt 0 ]; then
        err ""
        err "Refusing to start: required ports are in use."
        printf '%s\n' "${taken[@]}" >&2
        warn ""
        warn "Free them (kill <PID>) and re-run, or run scripts/local-teardown.sh."
        exit 2
    fi
}

# detect_os -> "darwin" | "linux"
detect_os() {
    case "$(uname -s)" in
        Darwin) echo darwin ;;
        Linux)  echo linux ;;
        *) echo "unsupported OS: $(uname -s) -- use the PowerShell script on Windows" >&2; exit 5 ;;
    esac
}

# install_pkg PACKAGE  -- routes to the host's package manager.
install_pkg() {
    local pkg=$1
    local os; os=$(detect_os)
    if [ "$os" = "darwin" ]; then
        if ! has_cmd brew; then
            err "Homebrew is required to auto-install on macOS. Get it from https://brew.sh, or install $pkg manually."
            exit 6
        fi
        sub "Installing $pkg via brew..."
        brew install "$pkg"
        return
    fi
    # Linux: probe for the common managers in order.
    if has_cmd apt-get; then
        sub "Installing $pkg via apt-get..."
        sudo apt-get update -qq
        sudo apt-get install -y "$pkg"
    elif has_cmd dnf; then
        sub "Installing $pkg via dnf..."
        sudo dnf install -y "$pkg"
    elif has_cmd pacman; then
        sub "Installing $pkg via pacman..."
        sudo pacman -S --noconfirm "$pkg"
    elif has_cmd zypper; then
        sub "Installing $pkg via zypper..."
        sudo zypper install -y "$pkg"
    else
        err "No supported package manager found (apt/dnf/pacman/zypper). Install $pkg manually."
        exit 6
    fi
}

assert_prereqs() {
    step "Checking prerequisites"
    for req in docker go git; do
        if ! has_cmd "$req"; then
            err "Missing required tool: $req"
            warn "  docker:  https://docs.docker.com/engine/install/"
            warn "  go 1.26: https://go.dev/dl/"
            warn "  git:     install via your package manager"
            exit 4
        fi
        sub "$req ok"
    done
    if ! docker info --format "{{.ServerVersion}}" >/dev/null 2>&1; then
        err "Docker CLI is on PATH but the daemon isn't responding. Start it (Docker Desktop / systemctl) and re-run."
        exit 4
    fi
    sub "docker daemon ok"
}

install_optional_deps() {
    step "Installing optional deps (auto)"
    if ! has_cmd make; then
        sub "make not found"
        install_pkg make
    else
        sub "make already installed"
    fi
    if [ "$WITH_K8S" -eq 1 ]; then
        for tool in kind helm kubectl; do
            if ! has_cmd "$tool"; then
                sub "$tool not found"
                install_pkg "$tool"
            else
                sub "$tool already installed"
            fi
        done
    fi
}

render_limits() {
    # cmd/limits validates that config/local-limits.yaml fits the host's
    # detected capacity, then writes deploy/docker-compose.override.yml
    # (and deploy/k8s/values-local.yaml for the optional k8s walkthrough).
    # Failure here means the host can't accommodate the requested limits;
    # the error message lists the largest contributors so the user knows
    # what to shrink in config/local-limits.yaml.
    step "Computing local resource limits"
    go run ./cmd/limits render
}

start_stack() {
    step "Bringing up docker compose infra"
    docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.override.yml up -d
    sub "Waiting for Postgres + Redis to report healthy (up to 45s)..."
    local deadline=$(( $(date +%s) + 45 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        local pg redis
        pg=$(docker inspect --format "{{.State.Health.Status}}" shortlink-postgres-1 2>/dev/null || echo "")
        redis=$(docker inspect --format "{{.State.Health.Status}}" shortlink-redis-1 2>/dev/null || echo "")
        if [ "$pg" = "healthy" ] && [ "$redis" = "healthy" ]; then
            sub "Postgres + Redis healthy"
            return
        fi
        sleep 2
    done
    err "Postgres or Redis never reported healthy after 45s."
    err "Inspect with: docker compose -f deploy/docker-compose.yml ps"
    exit 7
}

apply_migrations() {
    step "Applying migrations"
    go run ./cmd/migrate up
}

generate_keys() {
    step "Generating test API keys"
    if [ -f "config/keys.yaml" ]; then
        sub "config/keys.yaml already exists, leaving it alone (run cmd/keygen manually to rotate)"
        return
    fi
    go run ./cmd/keygen
}

build_binaries() {
    step "Building host binaries into ./bin"
    go build -o bin/ ./cmd/...
}

build_shortlink_images() {
    # Only needed in container mode. Same Dockerfile used by `make images` /
    # the k8s walkthrough -- distroless-nonroot, tagged shortlink-<svc>:dev.
    step "Building shortlink-<svc>:dev images for container mode"
    for svc in api worker observer; do
        sub "docker build --build-arg BINARY=$svc -t shortlink-${svc}:dev ."
        docker build --build-arg BINARY="$svc" -t "shortlink-${svc}:dev" .
    done
}

start_container() {
    local name=$1 port=$2
    local cpu mem container_name
    cpu=$("$REPO_ROOT/bin/limits" get "$name" cpu)
    mem=$("$REPO_ROOT/bin/limits" get "$name" memory_mb)
    container_name="shortlink-${name}"
    # Idempotent re-runs: remove any stale container with this name first.
    docker rm -f "$container_name" >/dev/null 2>&1 || true
    # SSRF_ALLOWLIST must include host.docker.internal so worker can deliver
    # webhooks to the loadtest sink running on the host (:8091). pgbouncer/
    # redis/minio resolve via compose DNS; observer is one of our containers
    # so address it by container name.
    # --add-host: Docker Desktop auto-adds host.docker.internal, but Linux
    # Docker doesn't. Without this, worker -> loadtest sink (host:8091) silently
    # fails on Linux even though SSRF_ALLOWLIST permits the name.
    docker run -d --name "$container_name" --network shortlink_default \
        --memory "${mem}M" --cpus "$cpu" \
        --add-host "host.docker.internal:host-gateway" \
        -p "${port}:${port}" \
        -e "DATABASE_URL=postgres://shortlink:shortlink@pgbouncer:6432/shortlink?sslmode=disable" \
        -e "REDIS_URL=redis://redis:6379" \
        -e "MINIO_ENDPOINT=minio:9000" \
        -e "OBSERVER_URL=http://shortlink-observer:9090" \
        -e "SSRF_ALLOWLIST=host.docker.internal,127.0.0.1,localhost" \
        "shortlink-${name}:dev" >/dev/null
    echo "$name $container_name" >> "$CONTAINERS_FILE"
    sub "$name -> $container_name (cpu=$cpu memory=${mem}M, port=$port)"
}

start_host_binary() {
    local name=$1 exe=$2; shift 2
    local args=("$@")
    local out="$LOG_DIR/$name.log"
    local errf="$LOG_DIR/$name.err"
    # Using nohup + & so the binary survives the script exit. Env is set by
    # the caller via plain shell exports.
    nohup "$exe" "${args[@]}" >"$out" 2>"$errf" &
    local pid=$!
    echo "$name $pid" >> "$PID_FILE"
    sub "$name -> PID $pid, log: $out"
}

wait_for_healthz() {
    local name=$1 url=$2 timeout=${3:-30}
    local deadline=$(( $(date +%s) + timeout ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if curl -fsS -o /dev/null --max-time 2 "$url"; then
            sub "$name healthy ($url)"
            return
        fi
        sleep 0.5
    done
    err "$name never returned 200 from $url within ${timeout}s."
    err "Check $LOG_DIR/$name.err for startup errors."
    exit 8
}

remove_previous_containers() {
    # Cleans containers recorded by a prior setup run (whether container mode
    # or not this time). Without this, host->container->host sequences leave
    # stale `shortlink-{api,worker,observer}` containers running because the
    # file gets deleted while the containers stay alive. Also salvages
    # partial-failure state from the last run.
    [ -f "$CONTAINERS_FILE" ] || return 0
    while read -r _name cname; do
        [ -n "$cname" ] && docker rm -f "$cname" >/dev/null 2>&1 || true
    done < "$CONTAINERS_FILE"
    rm -f "$CONTAINERS_FILE"
}

start_binaries() {
    step "Launching host binaries"
    mkdir -p "$LOG_DIR"
    : > "$PID_FILE"
    remove_previous_containers

    if [ "$CONTAINER_MODE" -eq 1 ]; then
        : > "$CONTAINERS_FILE"
        # api/worker/observer run inside the compose network with caps from
        # local-limits.yaml. Loadtest continues on the host (it serves the
        # showcase page and shells `docker stats` for the scaling panel).
        start_container observer 9090
        wait_for_healthz observer http://localhost:9090/healthz

        start_container worker 8081
        wait_for_healthz worker http://localhost:8081/healthz

        start_container api 8080
        wait_for_healthz api http://localhost:8080/healthz
    else
        # observer first (no env needed)
        start_host_binary observer "$REPO_ROOT/bin/observer"
        wait_for_healthz observer http://localhost:9090/healthz

        # worker + api need the SSRF allowlist so they can reach the loadtest
        # sink on the host loopback.
        export SSRF_ALLOWLIST="127.0.0.1,localhost,host.docker.internal"
        start_host_binary worker "$REPO_ROOT/bin/worker"
        wait_for_healthz worker http://localhost:8081/healthz

        start_host_binary api "$REPO_ROOT/bin/api"
        wait_for_healthz api http://localhost:8080/healthz
        unset SSRF_ALLOWLIST
    fi

    # loadtest with a long duration so the showcase stays up after the
    # attack ends; teardown.sh stops it cleanly via the PID file.
    start_host_binary loadtest "$REPO_ROOT/bin/loadtest" --duration=30m --grafana=http://localhost:3000
    wait_for_healthz loadtest http://localhost:8090/healthz
}

open_showcase() {
    if [ "$NO_OPEN" -eq 1 ]; then
        sub "Skipping browser open (--no-open)"
        return
    fi
    step "Opening the showcase page"
    local url="http://localhost:8090"
    if [ "$(detect_os)" = "darwin" ]; then
        open "$url"
    elif has_cmd xdg-open; then
        xdg-open "$url" >/dev/null 2>&1 || true
    else
        sub "No xdg-open / open available -- visit $url manually"
    fi
}

# ---------------------------------------------------------------------------
# main

if [ "$SKIP_DEPS" -ne 1 ]; then
    assert_prereqs
    install_optional_deps
fi

step "Checking required host ports are free"
assert_ports_free "${REQUIRED_PORTS[@]}"
# Soft check for compose ports: only fail if compose isn't already running
# the matching services.
compose_running=$(docker compose -f deploy/docker-compose.yml ps --services --status running 2>/dev/null || true)
if [ -z "$compose_running" ]; then
    assert_ports_free "${COMPOSE_PORTS[@]}"
else
    sub "Compose stack already up (services: $compose_running) -- skipping compose port check"
fi

render_limits
start_stack
apply_migrations
generate_keys
build_binaries
if [ "$CONTAINER_MODE" -eq 1 ]; then build_shortlink_images; fi
start_binaries
open_showcase

printf "\n${GREEN}Stack is up.${RESET}\n"
printf "${GREEN}  Showcase page : http://localhost:8090${RESET}\n"
printf "${GREEN}  Observer hub  : http://localhost:9090${RESET}\n"
printf "${GREEN}  Grafana       : http://localhost:3000${RESET}\n"
printf "${GREEN}  Prometheus    : http://localhost:9091${RESET}\n"
printf "${GREEN}  Logs          : %s${RESET}\n" "$LOG_DIR"
printf "${GREEN}  Teardown      : ./scripts/local-teardown.sh${RESET}\n"
