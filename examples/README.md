# Example scripts

The starter library now ships inside the server binary — press **Add starter
scripts** on the Scripts page to import it. The sources live in
`internal/server/web/starter/`:

| Script | What it reports |
|---|---|
| `inventory` | OS, addresses, local accounts, who is logged in |
| `disk-space` | Free space per filesystem, largest directories, inode usage |
| `top-processes` | Ten heaviest processes by CPU and by memory |
| `network-check` | Gateway, DNS, internet reachability, listening ports |
| `pending-updates` | Available package updates and whether a reboot is owed |
| `service-health` | Failed services and recent errors |

Each has a `sh` version for Linux and macOS and a `powershell` version for
Windows. All are **read-only diagnostics** — they report, they never change the
machine. Importing never overwrites a script you have edited: matching names are
skipped.
