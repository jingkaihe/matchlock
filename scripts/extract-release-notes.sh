#!/usr/bin/env bash
set -euo pipefail

RELEASE_FILE="RELEASE.md"

if [ ! -f "$RELEASE_FILE" ]; then
    echo "Error: $RELEASE_FILE not found" >&2
    exit 1
fi

notes=$(awk '
    /^## / {
        if (found_first) {
            exit
        }
        found_first = 1
        next
    }
    found_first {
        print
    }
' "$RELEASE_FILE" | awk '
    {
        lines[NR] = $0
        if ($0 ~ /[^[:space:]]/) {
            last_non_empty = NR
        }
    }
    END {
        if (last_non_empty == 0) {
            exit
        }
        first_non_empty = 1
        while (first_non_empty <= NR && lines[first_non_empty] ~ /^[[:space:]]*$/) {
            first_non_empty++
        }
        for (i = first_non_empty; i <= last_non_empty; i++) {
            print lines[i]
        }
    }
')

if [ -z "$notes" ]; then
    echo "Error: no release notes snippet found in $RELEASE_FILE" >&2
    exit 1
fi

printf '%s\n' "$notes"
