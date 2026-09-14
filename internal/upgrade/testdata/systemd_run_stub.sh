#!/bin/bash
# Stand-in for systemd-run in the upgrade snapshotter tests. The absolute
# interpreter path is deliberate: the tests set PATH to the stub directory
# alone, so env cannot find bash. It appends its argv to the file named by
# SYSTEMD_RUN_STUB_RECORD, drops its own options, and runs the command that
# follows them.
set -euo pipefail

printf '%s\n' "$*" >>"${SYSTEMD_RUN_STUB_RECORD}"

while [[ $# -gt 0 && "${1}" == --* ]]; do
    shift
done

exec "$@"
