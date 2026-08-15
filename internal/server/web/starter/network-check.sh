#!/bin/sh
# Is this machine's network actually working, and where does it break.
set -u

echo "=== interfaces ==="
if command -v ip >/dev/null 2>&1; then
	ip -4 -o addr show scope global | awk '{print "  " $2 ": " $4}'
	gw=$(ip route show default 2>/dev/null | awk '{print $3; exit}')
else
	ifconfig 2>/dev/null | awk '
		/^[a-z]/ { iface = substr($1, 1, length($1) - 1) }
		/[[:space:]]inet / && $2 != "127.0.0.1" { print "  " iface ": " $2 }'
	gw=$(route -n get default 2>/dev/null | awk '/gateway:/ {print $2}')
fi
echo "  default gateway: ${gw:-none found}"

echo
echo "=== gateway reachable ==="
if [ -n "${gw:-}" ] && ping -c 2 -W 2 "$gw" >/dev/null 2>&1; then
	echo "  yes ($gw)"
else
	echo "  NO — ${gw:-no gateway configured}"
fi

echo
echo "=== DNS resolution ==="
if command -v nslookup >/dev/null 2>&1 && nslookup example.com >/dev/null 2>&1; then
	echo "  working"
elif command -v host >/dev/null 2>&1 && host example.com >/dev/null 2>&1; then
	echo "  working"
else
	echo "  FAILED — name resolution is not working"
fi
echo "  nameservers:"
grep -E '^nameserver' /etc/resolv.conf 2>/dev/null | sed 's/^/    /' || echo "    (none listed)"

echo
echo "=== internet reachable ==="
if ping -c 2 -W 2 1.1.1.1 >/dev/null 2>&1; then
	echo "  yes (1.1.1.1 responds)"
else
	echo "  NO — cannot reach 1.1.1.1"
fi

echo
echo "=== listening ports ==="
if command -v ss >/dev/null 2>&1; then
	ss -tlnp 2>/dev/null | head -15 | sed 's/^/  /'
else
	netstat -an 2>/dev/null | grep -i listen | head -15 | sed 's/^/  /'
fi
