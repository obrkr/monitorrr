#!/bin/sh
# Basic device inventory: OS, addresses, and local user accounts.
# POSIX sh — runs on Ubuntu/Debian and macOS without modification.
#
# Deliberately does not use `set -e`: one unavailable command should not
# abandon the rest of the report.
set -u

echo "=== host ==="
echo "hostname:  $(hostname)"
echo "collected: $(date)"

echo
echo "=== os ==="
if [ -r /etc/os-release ]; then
	# shellcheck disable=SC1091
	. /etc/os-release
	echo "name:      ${PRETTY_NAME:-${NAME:-unknown}}"
elif command -v sw_vers >/dev/null 2>&1; then
	echo "name:      $(sw_vers -productName) $(sw_vers -productVersion) (build $(sw_vers -buildVersion))"
else
	echo "name:      unknown"
fi
echo "kernel:    $(uname -sr)"
echo "arch:      $(uname -m)"
echo "uptime:    $(uptime | sed 's/^[[:space:]]*//')"

echo
echo "=== addresses ==="
if command -v ip >/dev/null 2>&1; then
	ip -4 -o addr show scope global | awk '{print "  " $2 ": " $4}'
	echo "  default via: $(ip route show default 2>/dev/null | awk '{print $3 " dev " $5; exit}')"
elif command -v ifconfig >/dev/null 2>&1; then
	ifconfig | awk '
		/^[a-z]/ { iface = substr($1, 1, length($1) - 1) }
		/[[:space:]]inet / && $2 != "127.0.0.1" { print "  " iface ": " $2 }'
	echo "  default via: $(route -n get default 2>/dev/null | awk '/gateway:/ {print $2}')"
else
	echo "  (no ip or ifconfig available)"
fi

echo
echo "=== local user accounts ==="
if command -v dscl >/dev/null 2>&1; then
	# macOS keeps accounts in Directory Services, not /etc/passwd.
	# Real accounts start at UID 500; everything below that is a system role.
	dscl . -list /Users UniqueID 2>/dev/null |
		awk '$2 >= 500 { printf "  %s (uid %s)\n", $1, $2 }' | sort
else
	# Linux: UID >= 1000 excludes system accounts, 65534 is nobody.
	awk -F: '$3 >= 1000 && $3 < 65534 { printf "  %s (uid %s, shell %s)\n", $1, $3, $7 }' /etc/passwd
fi

echo
echo "=== accounts with admin rights ==="
if command -v dscl >/dev/null 2>&1; then
	dscl . -read /Groups/admin GroupMembership 2>/dev/null |
		sed 's/^GroupMembership: //' | tr ' ' '\n' | grep -v '^$' | sed 's/^/  /'
else
	for group in sudo wheel admin; do
		members=$(getent group "$group" 2>/dev/null | cut -d: -f4)
		[ -n "${members:-}" ] && echo "  $group: $members"
	done
fi

echo
echo "=== currently logged in ==="
if who 2>/dev/null | grep -q .; then
	who | sed 's/^/  /'
else
	echo "  (nobody)"
fi
