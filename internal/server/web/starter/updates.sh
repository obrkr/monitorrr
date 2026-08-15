#!/bin/sh
# What is waiting to be installed, and whether a reboot is owed.
# Read-only: counts and lists, never installs.
set -u

echo "=== pending updates ==="
if command -v apt-get >/dev/null 2>&1; then
	# -s simulates; nothing is downloaded or installed.
	count=$(apt-get -s upgrade 2>/dev/null | grep -c '^Inst ')
	echo "  apt: $count package(s) upgradable"
	apt-get -s upgrade 2>/dev/null | grep '^Inst ' | head -15 | sed 's/^Inst /    /'
	security=$(apt-get -s upgrade 2>/dev/null | grep '^Inst ' | grep -ci security)
	[ "$security" -gt 0 ] && echo "  of which security: $security"
elif command -v dnf >/dev/null 2>&1; then
	echo "  dnf: $(dnf -q check-update 2>/dev/null | grep -c '^[a-zA-Z0-9]') package(s) upgradable"
elif command -v softwareupdate >/dev/null 2>&1; then
	softwareupdate -l 2>&1 | sed 's/^/  /' | head -20
else
	echo "  no supported package manager found"
fi

echo
echo "=== reboot required ==="
if [ -f /var/run/reboot-required ]; then
	echo "  YES"
	cat /var/run/reboot-required.pkgs 2>/dev/null | sed 's/^/    /'
elif command -v needs-restarting >/dev/null 2>&1 && ! needs-restarting -r >/dev/null 2>&1; then
	echo "  YES"
else
	echo "  no"
fi

echo
echo "=== last boot ==="
if command -v uptime >/dev/null 2>&1 && uptime -s >/dev/null 2>&1; then
	echo "  $(uptime -s)"
elif command -v sysctl >/dev/null 2>&1 && sysctl -n kern.boottime >/dev/null 2>&1; then
	# macOS reports "{ sec = 1234567890, usec = 0 } Mon Jan ..."; the trailing
	# human-readable part is the useful half.
	sysctl -n kern.boottime | sed 's/.*} //' | sed 's/^/  /'
fi
uptime | sed 's/^[[:space:]]*/  /'
