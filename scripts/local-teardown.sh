#!/usr/bin/env bash
# Stops everything scripts/local-setup.sh brought up. Same shape as the
# PowerShell version; see scripts/local-teardown.ps1 for the description.
#
# Flags:
#   --keep-logs   leave ./logs/ in place after stopping the binaries
#   --keep-data   skip `docker compose down -v` (preserve Postgres/MinIO)
set -uo pipefail

KEEP_LOGS=0
KEEP_DATA=0
for arg in "$@"; do
    case "$arg" in
        --keep-logs) KEEP_LOGS=1 ;;
        --keep-data) KEEP_DATA=1 ;;
        *) echo "unknown flag: $arg" >&2; exit 64 ;;
    esac
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname -- "$SCRIPT_DIR")"
cd "$REPO_ROOT"

PID_FILE="$REPO_ROOT/.shortlink-pids"
CONTAINERS_FILE="$REPO_ROOT/.shortlink-containers"
LOG_DIR="$REPO_ROOT/logs"

if [ -t 1 ]; then CYAN='\033[36m'; GREEN='\033[32m'; DIM='\033[2m'; RESET='\033[0m'; else CYAN=''; GREEN=''; DIM=''; RESET=''; fi
step() { printf "\n${CYAN}==> %s${RESET}\n" "$*"; }
sub()  { printf "${DIM}    %s${RESET}\n" "$*"; }

step "Stopping host binaries"
if [ -f "$PID_FILE" ]; then
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        name=$(echo "$line" | awk '{print $1}')
        pid=$(echo  "$line" | awk '{print $2}')
        if ! kill -0 "$pid" 2>/dev/null; then
            sub "$name (PID $pid) already gone"
            continue
        fi
        sub "$name (PID $pid) -> stopping"
        kill "$pid" 2>/dev/null || true
        # Wait up to 5s for a clean exit, then SIGKILL.
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            kill -0 "$pid" 2>/dev/null || break
            sleep 0.5
        done
        if kill -0 "$pid" 2>/dev/null; then
            kill -9 "$pid" 2>/dev/null || true
        fi
    done < "$PID_FILE"
    rm -f "$PID_FILE"
else
    sub "No .shortlink-pids file -- nothing to kill"
fi

# Container-mode binaries from setup.sh --container-mode. The file lists
# "name container-name" per line; stop+rm each so a follow-up setup run
# isn't blocked by a name collision.
if [ -f "$CONTAINERS_FILE" ]; then
    step "Stopping shortlink containers"
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        name=$(echo "$line" | awk '{print $1}')
        cname=$(echo "$line" | awk '{print $2}')
        sub "$name -> docker rm -f $cname"
        docker rm -f "$cname" >/dev/null 2>&1 || true
    done < "$CONTAINERS_FILE"
    rm -f "$CONTAINERS_FILE"
fi

# Backstop: if a previous setup crashed mid-way the file may be missing
# or stale. Sweep up any container matching the well-known names so the
# next setup run isn't blocked by ghost containers we never recorded.
step "Sweeping orphan shortlink containers (backstop)"
orphans=$(docker ps -aq \
    --filter "name=^shortlink-api$" \
    --filter "name=^shortlink-worker$" \
    --filter "name=^shortlink-observer$" 2>/dev/null)
if [ -n "$orphans" ]; then
    # shellcheck disable=SC2086  # word-splitting is intentional
    docker rm -f $orphans >/dev/null 2>&1 || true
    sub "swept $(echo "$orphans" | wc -l | tr -d ' ') orphan(s)"
else
    sub "none found"
fi

step "Bringing the docker compose stack down"
if [ "$KEEP_DATA" -eq 1 ]; then
    docker compose -f deploy/docker-compose.yml down
else
    docker compose -f deploy/docker-compose.yml down -v
fi

if [ "$KEEP_LOGS" -ne 1 ] && [ -d "$LOG_DIR" ]; then
    step "Removing ./logs/"
    rm -rf "$LOG_DIR"
fi

printf "\n${GREEN}Teardown complete.${RESET}\n"
