# monitorrr

A lightweight endpoint visibility tool for a home lab — a very small take on the
Nexthink idea. Agents check in on a schedule, the dashboard shows who is
reporting, and you can push a shell or PowerShell script to any of them.

Two static binaries, one SQLite file, no runtime dependencies on either side.

## What it does

**Monitoring (milestone 1)**

- **Cross-platform agent** — Windows, macOS, Linux (amd64 + arm64) from one codebase
- **Online / offline state** with last-seen timestamps
- **Addresses** — the device's public internet address plus its own interfaces
- **Configurable check-in interval**, changed centrally and adopted fleet-wide
- **Dashboard** — live device table and an activity timeline
- **Deployment panel** — agent downloads, enrollment token, per-OS install steps

**Remote execution (milestone 2)**

- **Scripts panel** — write and store `sh` and `powershell` scripts, each with
  its own timeout
- **Explicit dispatch** — run a script from a device's page; nothing executes on its own
- **Runs panel** — state, exit code, duration, stdout/stderr, and the exact
  script body that was sent
- **Compatibility enforced** — a PowerShell script cannot be queued against a
  Linux box, and a mixed selection fails rather than half-running

## Quick start

```bash
make build-all          # server + agents for every platform
make run                # serves on :8080
```

Open <http://localhost:8080>. The enrollment token is printed at startup and
shown on the Deployment page. On a Linux or macOS target:

```bash
curl -fsSL "http://your-server:8080/install.sh?token=<token>" | sudo sh
```

The installer detects the machine's OS and CPU architecture, fetches the
matching build, installs to `/usr/local/bin`, and registers a systemd service or
launchd daemon. Pass `--no-service` or `--prefix DIR` to change that (through a
pipe: `| sudo sh -s -- --no-service`).

Architecture detection is the point: a Linux VM on an Apple Silicon host is
arm64, and running an amd64 build there fails with `cannot execute binary file`.
The Deployment page also lists every build for manual download, and carries
copy-paste service definitions for systemd, launchd, and a Windows scheduled
task.

## How it works

Every exchange is agent-initiated outbound HTTPS, so agents work behind NAT with
no inbound firewall rules or VPN. The check-in response doubles as the control
channel — the server piggybacks interval changes (and later, queued jobs) onto
the reply the agent is already waiting for.

```
agent                                server
  ├── POST /v1/enroll ──────────────► verify shared token, issue device identity
  │   ◄── agent_id + agent_token
  │
  ├── POST /v1/checkin (every N s) ─► update last_seen, record any transitions
  │   ◄── {interval, jobs[], retire}  piggybacked control data
  │
  └── (sweeper marks a device offline after 3 missed check-ins)
```

### Addresses

Two different addresses, for two different questions:

- **Public IP** — what the machine looks like from the internet. The agent
  resolves this itself against an external service (`ifconfig.me` and two
  fallbacks), caching for 30 minutes: a home lab's public address changes
  rarely, and this is an outbound call to a third party from every endpoint.
  Pass `-no-public-ip` to the agent to disable it.
- **Seen from** — the source address of the agent's connection, which the server
  observes directly. On a lab LAN this is a private address, and it is the same
  for every device behind one NAT.

The server cannot derive the first from the second, which is why the agent
reports it rather than the server inferring it.

The server also resolves **its own** address at startup, by opening a UDP socket
towards a public address and reading back which interface the kernel chose (no
packet is sent). That address is substituted into install commands whenever the
dashboard is opened on `localhost` — otherwise the commands you copy would point
each target machine back at itself. `-public-url` overrides it for a reverse
proxy or a DNS name.

### Retire vs delete

Two different operations that are easy to confuse, so the dashboard names them
apart:

| | Delete | Retire |
|---|---|---|
| Server record | removed, with all history | kept, marked retired |
| Agent on the machine | untouched, keeps running | uninstalls itself |
| Next check-in | 401 → re-enrolls as a new device | receives retire, tears down, exits |
| Use it for | resetting a device's identity | decommissioning a machine |

Retirement is a request, not an instant state. The device shows **Retiring…**
until its agent next checks in, so a machine that is powered off is not silently
forgotten — it retires whenever it next comes back. Queued jobs are cancelled on
request, and no new ones can be aimed at it.

The teardown runs in a **detached helper process**, not inline. Stopping your own
service from inside it is a race you cannot win: systemd would kill the agent
partway through its own cleanup, and Windows refuses to delete a running
executable. Handing the work to a process that outlives the agent avoids both.

The agent only removes its binary if it is running from the canonical install
path (`/usr/local/bin/monitorrr-agent`, or `C:\Program Files\monitorrr\`).
Someone testing a build from a working directory should not have it deleted out
from under them.

### Job lifecycle

A job is `queued` when dispatched, flips to `running` when an agent collects it
on a check-in (claiming is transactional, so a job is handed out exactly once),
and becomes `done` when the result is posted back. A sweeper marks it `lost` if
the agent never reports — a machine rebooted mid-script, say — so nothing sits
in `running` forever.

Details that matter in practice:

- Jobs run on their own goroutine in the agent, so a five-minute script never
  delays heartbeats and makes the device look offline.
- Scripts execute in their own **process group**, and a timeout kills the group.
  Killing only the shell leaves `sleep 60` running, and because that grandchild
  inherits the output pipe, the agent would block until it finished anyway.
- The agent verifies the script's SHA-256 before writing it to disk. A mismatch
  is refused outright rather than partially executed.
- A non-zero exit is a normal result, not an execution failure. `Error` is
  reserved for "could not run it at all" — timeout, missing interpreter,
  checksum mismatch.
- Each job snapshots the script body and hash. Editing or deleting a script
  never rewrites what the audit trail says already ran.
- Output is capped at 64 KB per stream, at the agent and again at the store.

**Heartbeats are not stored.** Writing a row per check-in would be ~1,400 rows
per device per day to answer a question that one mutable `last_seen` column
already answers. Only *transitions* — enrolled, online, offline, address change,
version change — land in `device_events`, which keeps the table small and makes
it readable as an actual timeline. That is also why SQLite is the right backing
store here rather than a time-series database. All SQL is vanilla and confined
to `internal/store`, so moving to Postgres later is a driver swap.

## Layout

```
cmd/server, cmd/agent      entry points
internal/proto             agent ↔ server wire contract
internal/store             persistence; all SQL lives here
internal/server            HTTP API, web UI (embedded templates + assets)
internal/agent             check-in loop, identity, script execution
```

Five pages: **Dashboard** (fleet state; a row opens the machine), **Device**
(facts, actions, run a script, timeline), **Scripts** (authoring only),
**Runs** (history and output), **Deployment** (installers and downloads).

Actions belong to a device, not to a list. Retire, delete, and dispatch all live
on the device page; the scripts page is purely for writing and storing scripts.

The UI is server-rendered Go templates plus vanilla JS — no Node, no build step.
Everything is embedded in the binary with `go:embed`.

## Configuration

Server flags (each also reads an env var):

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `-addr` | `MONITORRR_ADDR` | `:8080` | Listen address |
| `-db` | `MONITORRR_DB` | `monitorrr.db` | SQLite file |
| `-dist` | `MONITORRR_DIST` | `dist` | Where agent binaries are served from |
| `-admin-password` | `MONITORRR_ADMIN_PASSWORD` | *(none)* | Basic auth for UI and admin API |
| `-public-url` | `MONITORRR_PUBLIC_URL` | *(inferred)* | Base URL shown in install commands |
| `-tls-cert` / `-tls-key` | `MONITORRR_TLS_*` | *(none)* | Enable HTTPS directly |

Agent flags: `-server`, `-enroll-token`, `-state`, `-insecure`, `-once`,
`-no-public-ip` (skip resolving the public address via a third party), `-debug`.

The database defaults to `monitorrr.db` in the working directory; `make run`
puts it in `data/`, deliberately outside `dist/` so `make clean` cannot destroy
it. Losing it means every agent has to be reinstalled, since device identities
live there.

## Versioning

Builds are stamped from git: `git describe` for the version and a UTC build
timestamp to separate repeated builds of the same commit. Agents report the full
string, so the dashboard shows exactly which build each machine is running —
which is how you tell whether a fleet has picked up a fix.

```
monitorrr-agent v0.1.0-2-g6c2aac2 (2026-08-15T09:47:40Z)
```

Capabilities are advertised separately from the version. An agent sends the list
of instructions it understands (`jobs`, `retire`), because a build identifier is
opaque and an old agent silently ignores fields it does not know — the server
needs to distinguish "will never act on this" from "has not got round to it".

## Security notes

- Devices authenticate with a per-device token issued at enrollment; only its
  SHA-256 hash is stored. The enrollment token is a separate shared secret and
  can be rotated without disturbing enrolled devices.
- `/install.sh` and `/download/*` authenticate with the enrollment token
  (`?token=`) rather than the admin password: a machine being provisioned has
  the token but no business holding admin credentials, and anyone with the token
  can enroll anyway, so the trust level is unchanged.
- The identity file is written `0600` via a temp file and rename.
- **Defaults are lab defaults.** With no `-admin-password` the UI is open, and
  without TLS the agent tokens cross the network in clear text. The server warns
  about both at startup. Set a password and terminate TLS before this is
  reachable from anywhere untrusted.
- **Remote execution is a serious trust boundary.** Scripts run as whatever the
  agent runs as — root under systemd, SYSTEM under the Windows task. Anyone who
  can reach the admin UI can execute arbitrary code on every enrolled machine,
  so `-admin-password` stops being optional the moment this leaves an isolated
  network. Every save and dispatch is logged with who, what (name + SHA-256),
  and where.
