#!/bin/sh
# Services that should be running and are not.
set -u

if command -v systemctl >/dev/null 2>&1; then
	echo "=== failed units ==="
	failed=$(systemctl --failed --no-legend --plain 2>/dev/null)
	if [ -n "$failed" ]; then
		echo "$failed" | sed 's/^/  /'
	else
		echo "  none"
	fi

	echo
	echo "=== enabled units that are not running ==="
	systemctl list-units --type=service --state=exited,dead --no-legend --plain 2>/dev/null |
		head -15 | sed 's/^/  /' || echo "  none"

	echo
	echo "=== recent errors in the journal ==="
	journalctl -p err -n 15 --no-pager 2>/dev/null | sed 's/^/  /' || echo "  journal unavailable"

elif command -v launchctl >/dev/null 2>&1; then
	echo "=== launchd services that exited non-zero ==="
	# Column 2 is the last exit status; anything non-zero is worth a look.
	launchctl list 2>/dev/null | awk 'NR>1 && $2 != "0" && $2 != "-" {print "  " $3 " (exit " $2 ")"}' |
		head -20
	echo
	echo "  (a dash in the status column means the job is not currently running)"
else
	echo "no supported service manager found"
fi
