# monitorrr

![Vibe Coded](https://img.shields.io/badge/vibe-coded-ff5fa8?style=for-the-badge)
![Built with Claude Code](https://img.shields.io/badge/built%20with-Claude%20Code-8a5cf6?style=for-the-badge)
![Two Static Binaries](https://img.shields.io/badge/deploy-two%20static%20binaries-3b82f6?style=for-the-badge)

A self-hosted **endpoint visibility and remote execution** tool for a home lab — a very small take on the Nexthink idea. Agents check in on a schedule, a dashboard shows who is reporting, and from a device's page you can run a script on it, push an installer and execute it, or pull a file back off it. One Go codebase builds static agents for Windows, macOS and Linux on amd64 and arm64; the server is a single binary with a SQLite file beside it.

> **This project is fully vibe coded.** The protocol, the agent lifecycle, the self-update mechanism and the permission model were all built through conversational, AI-assisted development rather than a planned build. It has been verified with real agents on real machines — a 40 MB file collected byte-identical, an agent updating itself under a live supervisor, a retirement that genuinely uninstalls — but it executes arbitrary code as root and SYSTEM on every machine it touches. Read it yourself before pointing it at anything you care about.

> **Built with Go and SQLite.** One external dependency: a pure-Go SQLite driver, so `CGO_ENABLED=0` produces a static binary for every target from one machine with no C toolchain. The UI is server-rendered templates and one vanilla JavaScript file, embedded in the binary with `go:embed`. No build step, no bundler, no framework and no CDN — a console that fails to load is a console you cannot use during an incident.

---

## Contents

- [What it does](#what-it-does)
- [Quick start](#quick-start)
- [How it works](#how-it-works)
- [Accounts and roles](#accounts-and-roles)
- [Running scripts](#running-scripts)
- [Pushing a file and running it](#pushing-a-file-and-running-it)
- [Collecting a file from a device](#collecting-a-file-from-a-device)
- [Retiring and re-enrolling](#retiring-and-re-enrolling)
- [Agent auto-update](#agent-auto-update)
- [Audit trail](#audit-trail)
- [Configuration](#configuration)
- [Security notes](#security-notes)
- [Project structure](#project-structure)
- [How it's built](#how-its-built)

---

## What it does

- **Cross-platform agent** — Windows, macOS and Linux on amd64 and arm64, from one codebase, with no runtime to install on the endpoint.
- **Heartbeat monitoring** — online/offline state, last seen, the device's public internet address and its own interfaces, on a check-in interval you set centrally.
- **Remote execution** — write `sh` and PowerShell scripts, dispatch them to one device or a whole tag, and read back exit code, duration, stdout and stderr.
- **Push a file and run it** — attach an installer to a script; the agent fetches it, verifies its digest, and hands the path to your script.
- **Collect a file** — give a full path and the agent sends it back. Size first, then a transfer with progress. Locked files are copied aside and read from the copy.
- **Tags and fleet dispatch** — label devices and run one script across everything matching, with incompatible machines reported rather than silently skipped.
- **Starter library** — twelve read-only diagnostics, `sh` and PowerShell pairs, imported with one click.
- **Self-updating agents** — the server serves a new build and agents replace themselves, verifying the replacement runs before installing it.
- **Retirement** — a device uninstalls its own agent on request, as distinct from deleting its record.
- **Accounts and roles** — admin or read-only, with a first-run setup flow.
- **Audit trail** — every change made through the console, with who, what, when and from where.

---

## Quick start

```bash
make build-all          # server + agents for every platform
make run                # serves on :8080
```

Open <http://localhost:8080>. The first visit asks you to create an administrator account; nothing else is reachable until you do.

Then, on a Linux or macOS target:

```bash
curl -fsSL "http://your-server:8080/install.sh?token=<token>" | sudo sh
```

The installer detects the machine's OS and CPU architecture, fetches the matching build, installs to `/usr/local/bin`, and registers a systemd service or launchd daemon. The token is on the Deployment page, which also carries per-OS manual instructions and a Windows scheduled-task setup.

Architecture detection is the point rather than a nicety: a Linux VM on an Apple Silicon host is arm64, and running an amd64 build there fails with `cannot execute binary file`.

---

## How it works

Every exchange is agent-initiated outbound HTTPS, so agents work behind NAT with no inbound firewall rules and no VPN. The check-in response doubles as the control channel — the server piggybacks everything it wants the agent to do onto the reply the agent is already waiting for.

```
agent                                 server
  ├── POST /v1/enroll ───────────────► verify shared token, issue device identity
  │   ◄── agent_id + agent_token
  │
  ├── POST /v1/checkin (every N s) ──► update last_seen, record any transitions
  │   ◄── {interval, jobs[],           piggybacked control data
  │        collections[], retire,
  │        update}
  │
  └── (sweeper marks a device offline after 3 missed check-ins)
```

**Heartbeats are not stored.** Writing a row per check-in would be ~1,400 rows per device per day to answer a question that one mutable `last_seen` column already answers. Only *transitions* — enrolled, online, offline, address change, version change — land in `device_events`, which keeps the table small and makes it readable as an actual timeline. That is also why SQLite is the right backing store here rather than a time-series database. All SQL is vanilla and confined to `internal/store`, so moving to Postgres later is a driver swap.

**Two different addresses, for two different questions.** The **public IP** is what the machine looks like from the internet, resolved by the agent itself against an external service and cached for 30 minutes. **Seen from** is the source address of its connection, which the server observes directly and which is private whenever the agent shares your network. The server cannot derive the first from the second, which is why the agent reports it.

---

## Accounts and roles

On first launch the server has no accounts and serves nothing but a setup page. Further accounts are added under Settings, in one of two roles:

| | Admin | Read-only |
|---|---|---|
| View devices, runs, scripts, collected files | yes | yes |
| Run scripts, collect files, retire devices | yes | no |
| Manage scripts, files, tags, settings | yes | no |
| Manage accounts, read the audit log | yes | not even visible |

The role rule is one line in the middleware: **read-only accounts may issue GET and HEAD, nothing else.** Every mutation is a POST, PATCH or DELETE, so there is no list of protected endpoints to keep in step with the routes — a new mutating endpoint is restricted the moment it is added, rather than the moment someone remembers to add it to a list.

Passwords are PBKDF2-HMAC-SHA256 at 600,000 iterations with a per-user salt, from the standard library. A login for an account that does not exist hashes anyway before failing, so response timing cannot be used to enumerate accounts. The last administrator cannot be deleted or demoted.

Settings also carries two destructive actions, each requiring a different word to be typed: **reset the fleet**, which clears devices, scripts, runs and stored files but keeps accounts; and **factory reset**, which additionally removes every account, the audit log and all settings, returning the instance to first run.

---

## Running scripts

Scripts are written under **Scripts** and dispatched from a device's page, or across a tag from the dashboard. A job is `queued` when dispatched, `running` when an agent collects it on a check-in, and `done` when the result is posted back. A sweeper marks it `lost` if the agent never reports.

Details that matter in practice:

- Jobs run on their own goroutine in the agent, so a five-minute script never delays heartbeats and makes the device look offline.
- Scripts execute in their own **process group**, and a timeout kills the group. Killing only the shell leaves `sleep 60` running, and because that grandchild inherits the output pipe, the agent would block until it finished anyway.
- The agent verifies the script's SHA-256 before writing it to disk.
- A non-zero exit is a normal result, not an execution failure. `error` is reserved for "could not run it at all" — timeout, missing interpreter, checksum mismatch.
- Each job snapshots the script body and hash, so editing or deleting a script never rewrites what the audit trail says already ran.
- Output is capped at 64 KB per stream, at the agent and again at the store.

Dispatching to a **named device** is strict: an incompatible one is an error. Dispatching to a **group** skips incompatible machines and reports them, because skipping a Windows box for a shell script is expected across a mixed fleet — but a fleet run must never be quietly narrower than it looked.

---

## Pushing a file and running it

Upload a file under Scripts, attach it to a script, and dispatch as usual. The agent downloads it, verifies the digest, makes it executable, and passes the path as `$MONITORRR_PAYLOAD` (`$env:MONITORRR_PAYLOAD` on Windows). The file is deleted from the device when the script finishes.

Bytes live on disk beside the database and are streamed in both directions. A pushed installer is routinely hundreds of megabytes, and buffering one would put the server at the mercy of whatever is uploaded and risk the OOM reaper on a small endpoint. Serving is authorised per job, not per payload: a device may only fetch the file for work actually dispatched to it.

---

## Collecting a file from a device

A full path typed on the device page pulls that file back. The agent reports the **size first**, before any bytes move — asking for a 40 GB VM image by mistake should be obvious from the dashboard, not discovered an hour later. The transfer then streams with a live progress bar.

A file that cannot be opened directly is **copied aside and read from the copy**, which is how a locked or in-use file becomes collectable; the copy is removed afterwards. That fallback cannot fix everything, so the failure messages distinguish the two cases: a permissions failure is fixed by how the agent is installed, a lock is not.

Collected files are kept for **7 days**, then deleted. The record survives the file — knowing something was pulled, by whom, and that it has since been removed is the point of an audit trail — and downloading an expired collection returns 410 with that explanation rather than a bare 404.

---

## Retiring and re-enrolling

Deleting a device and retiring it are different operations, so the console names them apart:

| | Delete | Retire |
|---|---|---|
| Server record | removed, with all history | kept, marked retired |
| Agent on the machine | untouched, keeps running | uninstalls itself |
| Next check-in | 401 → re-enrols as a new device | receives retire, tears down, exits |
| Use it for | resetting a device's identity | decommissioning a machine |

The agent's binary is removed too, wherever it was installed. The decision keys
on whether a service manager knows about the agent rather than on the path
alone, because the installer accepts `--prefix` and an agent in `/opt/monitorrr`
is every bit as real as one in `/usr/local/bin`. With no service registered
nothing is deleted — that is someone running a build by hand, and removing it
under them would be a surprise for no benefit.

Teardown runs in a **detached helper process**, not inline. Stopping your own service from inside it is a race you cannot win: systemd would kill the agent partway through its own cleanup, and Windows refuses to delete a running executable.

Retirement leaves a marker beside the identity file, and an agent that finds one exits instead of enrolling. That is what stops a supervisor restart quietly resurrecting a decommissioned machine as a ghost device — the failure this protects against is real, and was found by testing rather than reasoning. Coming back is therefore deliberate: reinstall (the installer clears the marker), run once with `-force-enroll`, or delete the marker by hand. The machine returns as a **new device**; the retired record is history, not something to reuse.

---

## Agent auto-update

Agents replace their own binary when the server is serving a different build for their platform. The comparison is between **digests, not version strings**: a version is opaque text that can repeat across rebuilds, and an agent that updated to a build reporting the same version would be told to update again forever. Digests cannot loop.

The order of operations is the safety story:

1. Download to a staging file beside the target, never over it
2. Verify the digest matches what the server said
3. **Run the replacement** with `-version` and check it identifies itself
4. Swap it in, then exit so the supervisor restarts on the new version

Step 3 is the important one. A truncated or wrong-architecture binary that passes a checksum but cannot execute would brick every machine it reached, and the fleet would have no way to receive the fix. Any failure leaves the existing installation untouched.

Updates are withheld while a device has work outstanding, since updating means exiting. Only a binary at the canonical install path is replaced, so a developer's build is never overwritten. `-no-auto-update` opts a machine out; the Deployment page has a fleet-wide switch.

---

## Audit trail

Every state-changing request is recorded: who, what, when, from where, and detail enough to know what happened. Sign-ins and failed sign-ins are recorded too — a trail showing only successful access misses the part worth reviewing.

It is middleware, not a call in each handler, for the same reason the read-only rule is: anything that changes state is a POST, PATCH or DELETE, so covering those covers everything by construction. Reads are not recorded, and neither are refused requests — recording those would make the log describe things that never happened.

Settings also carries a **theme** (dark, light, system, Nord, Dracula, Solarized Dark, Gruvbox) and a **display time zone**, defaulting to UTC rather than the viewer's browser so that two people reading the same timestamp see the same time.

---

## Configuration

Server flags, each also reading an environment variable:

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `-addr` | `MONITORRR_ADDR` | `:8080` | Listen address |
| `-db` | `MONITORRR_DB` | `monitorrr.db` | SQLite file |
| `-dist` | `MONITORRR_DIST` | `dist` | Where agent binaries are served from |
| `-public-url` | `MONITORRR_PUBLIC_URL` | *(inferred)* | Base URL shown in install commands |
| `-tls-cert` / `-tls-key` | `MONITORRR_TLS_*` | *(none)* | Enable HTTPS directly |
| `-debug` | — | off | Verbose logging, including every heartbeat |

Agent flags: `-server`, `-enroll-token`, `-state`, `-insecure`, `-once`, `-no-public-ip`, `-no-auto-update`, `-force-enroll`, `-debug`.

`make run` puts the database in `data/`, deliberately outside `dist/` so `make clean` cannot destroy it — losing it means every agent has to be reinstalled.

---

## Security notes

- **This tool executes arbitrary code as root and SYSTEM on every enrolled machine.** That is what it is for, and it is also the whole risk. Any admin account can do it, which is what the read-only role exists for.
- Devices authenticate with a per-device token issued at enrollment; only its SHA-256 hash is stored. The enrollment token is a separate shared secret and can be rotated without disturbing enrolled devices.
- `/install.sh` and `/download/*` authenticate with the enrollment token rather than an operator account: a machine being provisioned has the token but no business holding admin credentials.
- Session cookies are `HttpOnly` and `SameSite=Lax`, and `Secure` when served over TLS.
- **Without TLS, session cookies and agent tokens cross the network in clear text.** The server warns about this at startup. Terminate TLS before this is reachable from anywhere untrusted.
- Every script save and dispatch is logged with who, what (name and SHA-256), and where.

---

## Project structure

```
cmd/server/main.go          server entry point and flags
cmd/agent/main.go           agent entry point and flags
internal/proto/             the agent ↔ server wire contract
internal/store/             persistence; every SQL statement lives here
  store.go                  devices, heartbeats, transitions, schema, migrations
  scripts.go                scripts and jobs
  collections.go            files pulled off devices
  payloads.go               files pushed to devices
  users.go                  accounts, sessions, resets
  audit.go                  audit log, theme and time zone
  tags.go                   device tags and fleet dispatch
internal/server/            HTTP API and web UI
  server.go                 routes, check-in, sweepers
  auth.go                   setup, login, sessions, role enforcement
  audit.go                  change-recording middleware
  agentbuilds.go            agent binary digests and self-update decisions
  collections.go            file collection endpoints and retention
  payloads.go               file upload and per-job serving
  reset.go                  fleet reset and factory reset
  starter.go                the built-in script library
  web/templates/            server-rendered pages
  web/static/               one stylesheet, one script
  web/starter/              the built-in script library sources
internal/agent/             check-in loop, identity, execution
  agent.go                  the loop, enrollment, job queueing
  exec.go                   script execution, process groups, output caps
  collect.go                pulling files back
  update.go                 self-replacement
  uninstall.go              retirement and the tombstone
data/                       database, uploads and collected files (created on first run)
```

---

## How it's built

Go and SQLite, with `modernc.org/sqlite` as the only external dependency — a pure-Go driver, so `CGO_ENABLED=0` cross-compiles every target from one machine with no C toolchain. Both binaries are static; the endpoint needs nothing installed.

The interesting parts:

- **`internal/proto`** is the whole contract between the two halves. The server is strict about what it accepts and the agent is lenient about what it receives, which is what lets an old agent keep working against a new server while a new agent still fails loudly against an old one.
- **`internal/agent/exec.go`** is where most of the sharp edges live: process groups so a timeout kills the tree rather than just the shell, capped output that never short-writes to the child, and the distinction between "exited non-zero" and "could not be run".
- **`internal/agent/uninstall.go`** holds the tombstone. It is the smallest file with the most reasoning behind it, all of it about a supervisor restarting an agent at the wrong moment.
- **`internal/server/auth.go`** enforces both rules that matter — signed in, and allowed — in one middleware, on purpose.
- **`internal/store/store.go`** owns the schema and the migrations. Every column added after the first release is applied there, so an existing database is upgraded on open rather than needing to be rebuilt.
