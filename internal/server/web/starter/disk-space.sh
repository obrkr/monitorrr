#!/bin/sh
# Free space per filesystem, plus the biggest directories worth looking at.
# Read-only: reports, never deletes.
set -u

# Pseudo filesystems are always "100% full" by design — devfs, tmpfs and friends
# are not disks. Including them turns the capacity warning into noise.
drop_pseudo() {
	awk 'NR==1 || $1 !~ /^(devfs|map|tmpfs|overlay|udev|none|auto_home)$/'
}

# Two different df calls on purpose: -h is readable but its column layout
# differs between macOS and Linux, while -P guarantees capacity in $5 and the
# mount point in $6. Display uses the first, the threshold test the second.
echo "=== filesystems ==="
df -h 2>/dev/null | drop_pseudo

echo
echo "=== filesystems above 80% used ==="
if df -P 2>/dev/null | drop_pseudo |
	awk 'NR>1 && int($5) >= 80 {found=1; print "  " $6 " — " $5 " used"} END {exit !found}'; then
	:
else
	echo "  none"
fi

echo
echo "=== largest directories ==="
for base in /var /home /Users /opt /srv; do
	[ -d "$base" ] || continue
	# One level down only: a full-depth scan on a big disk is slow and rarely
	# more useful than knowing which top-level directory is the problem.
	sizes=$(du -sh "$base"/* 2>/dev/null | sort -rh | head -5)
	# Skip bases with nothing in them rather than printing a bare heading.
	[ -n "$sizes" ] || continue
	echo "  under $base:"
	echo "$sizes" | sed 's/^/    /'
done

echo
echo "=== inode usage (a full inode table looks like a full disk) ==="
# No -P here: it drops the inode columns entirely on macOS.
df -i 2>/dev/null | drop_pseudo | head -10 || echo "  unavailable"
