#!/bin/sh
# The processes actually consuming this machine.
set -u

echo "=== load ==="
uptime | sed 's/^[[:space:]]*/  /'

# ps prints a header row that would sort into the results, so it is dropped
# before sorting rather than hoped to land somewhere harmless.
top_by() {
	ps -eo "$1" 2>/dev/null | tail -n +2 | sort -rn | head -10 | sed 's/^/  /'
}

echo
echo "=== top 10 by CPU (%cpu %mem pid user command) ==="
top_by pcpu,pmem,pid,user,comm

echo
echo "=== top 10 by memory (%mem %cpu pid user command) ==="
top_by pmem,pcpu,pid,user,comm

echo
echo "=== memory summary ==="
if command -v free >/dev/null 2>&1; then
	free -h | sed 's/^/  /'
elif command -v vm_stat >/dev/null 2>&1; then
	# macOS reports pages; convert to something readable.
	vm_stat | awk '/page size of/ {size=$8}
		/Pages free/ {free=$3}
		/Pages active/ {active=$3}
		/Pages wired/ {wired=$4}
		END {
			gsub(/\./,"",free); gsub(/\./,"",active); gsub(/\./,"",wired);
			if (size == "") size = 4096;
			printf "  free: %.1f GB, active: %.1f GB, wired: %.1f GB\n",
				free*size/1073741824, active*size/1073741824, wired*size/1073741824
		}'
fi
