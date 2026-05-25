# scripts/

One-command bring-up and tear-down for the full local ShortLink stack —
infra + host binaries + showcase page open in your browser.

| Script | Platform | Purpose |
|---|---|---|
| `local-setup.ps1`    | Windows (PowerShell) | bring everything up |
| `local-setup.sh`     | macOS / Linux (Bash) | bring everything up |
| `local-teardown.ps1` | Windows              | stop everything cleanly |
| `local-teardown.sh`  | macOS / Linux        | stop everything cleanly |

The `.ps1` and `.sh` versions are kept in functional lockstep — same
prerequisites, same auto-install policy, same end state, same flags
(translated to the host's idiomatic spelling).

## Quickstart

**Windows (PowerShell):**

```powershell
.\scripts\local-setup.ps1
# ...test...
.\scripts\local-teardown.ps1
```

**macOS / Linux:**

```bash
chmod +x scripts/local-setup.sh scripts/local-teardown.sh   # first time only
./scripts/local-setup.sh
# ...test...
./scripts/local-teardown.sh
```

## What setup does

In order:

1. **Prereq check.** Verifies `docker`, `go`, and `git` are on PATH and that
   the Docker daemon is responding. Bails with an install link if any are
   missing — these three you bring yourself.
2. **Auto-install optional deps.** `make` is installed via the host package
   manager if missing (winget/choco/scoop on Windows; brew on macOS;
   apt/dnf/pacman/zypper on Linux). With `--with-k8s` it also installs
   `kind`, `helm`, and `kubectl`.
3. **Port check.** Refuses to start if any of the required host ports are
   held by another process — no auto-kill. The compose ports get a softer
   check that skips if the compose stack is already up (idempotent re-runs).
4. **Render local limits.** Runs `go run ./cmd/limits render` which validates
   `config/local-limits.yaml` against detected host capacity and writes
   `deploy/docker-compose.override.yml` (and `deploy/k8s/values-local.yaml`).
   An over-budget config fails fast with the largest contributors listed.
   See SPEC §13 *Local resource limits*.
5. **Docker compose up** with `deploy/docker-compose.yml` + the rendered
   override file, then waits for Postgres and Redis to report healthy
   (up to 45 s).
6. **Apply migrations** via `go run ./cmd/migrate up`.
7. **Generate test API keys** via `go run ./cmd/keygen` — skipped if
   `config/keys.yaml` already exists.
8. **Build the host binaries** into `./bin/` via `go build`.
9. **Launch host binaries** (api, worker, observer, loadtest) as background
   processes. PIDs are recorded in `.shortlink-pids`. Stdout and stderr are
   teed into `./logs/{api,worker,observer,loadtest}.{log,err}`.
10. **Wait for `/healthz`** on each binary (up to 30 s each).
11. **Open the showcase page** in your default browser, unless `--no-open`.

The setup script is idempotent — re-running it stops the old binaries
(via the PID file) and restarts them, while reusing already-healthy
containers. Use it as your "fresh state" button during development.

## Flags

**Setup**:

| Flag                  | Effect                                                  |
|-----------------------|---------------------------------------------------------|
| `-WithK8s` / `--with-k8s` | also install kind + helm + kubectl                   |
| `-NoOpen`  / `--no-open`  | skip the browser-open step (useful in CI / SSH)      |
| `-SkipDeps`/ `--skip-deps`| trust the host has every dep; skip checks + installs |

**Teardown**:

| Flag                     | Effect                                                       |
|--------------------------|--------------------------------------------------------------|
| `-KeepLogs` / `--keep-logs` | leave `./logs/` in place after stopping the binaries        |
| `-KeepData` / `--keep-data` | skip `docker compose down -v` (preserve Postgres/MinIO data) |

## Files the scripts create

- `./bin/` — built binaries (api, worker, observer, loadtest, keygen, migrate)
- `./logs/` — `<name>.log` and `<name>.err` per host binary
- `./.shortlink-pids` — `name PID` per line; teardown reads this to know
  what to kill

`./bin/`, `./logs/`, and `.shortlink-pids` are all gitignored. `config/keys.yaml`
contains real key material and stays gitignored as it always has.

## Troubleshooting

- **"Refusing to start: required ports are in use"** — another process is
  holding 8080/8081/8090/8091/9090. The script prints the PID and process
  name. Stop it (`Stop-Process -Id <PID>` / `kill <PID>`) and re-run.
- **"<name> never returned 200 from /healthz within 30s"** — look at
  `./logs/<name>.err` for the startup error. Usually a config issue (wrong
  DATABASE_URL, REDIS_URL, missing migration) or a stale binary.
- **"Postgres or Redis never reported healthy after 45s"** — compose
  containers may be wedged from a previous run. `docker compose -f
  deploy/docker-compose.yml down -v` and re-run setup.
- **Windows: `make` installed but `make --version` still fails** — winget
  installs to `C:\Program Files (x86)\GnuWin32\bin`. The setup script
  adds it to PATH for its own session, but a fresh PowerShell needs you to
  either re-open it after the install or edit System Environment Variables.
- **Linux: `sudo apt-get install` prompts for password** — the script
  shells out to `sudo` for package installs, which is interactive on most
  systems. Run with `--skip-deps` if you'd rather install `make` ahead of
  time and avoid the prompt.

## Why scripts vs a Go program?

We considered a single `scripts/setup` Go program (cross-platform by
construction, no shell dialect issues) but landed on per-platform scripts
because:

- Setup is mostly orchestrating OTHER tools — package managers, docker,
  curl. A shell script is the idiomatic way to do that.
- The auto-install paths differ enough across OSes that the abstraction
  would be thicker than the underlying calls. Two ~300-line scripts read
  better than one 600-line Go program that does the same thing.
- No build step before running setup. The setup script is the first thing
  a user touches; making it depend on `go build` first defeats the point.

The host binaries themselves still ship via `go build`.
