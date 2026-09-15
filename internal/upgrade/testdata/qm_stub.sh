#!/bin/bash
# Stand-in for the Proxmox qm command in the upgrade snapshotter tests. The
# absolute interpreter path is deliberate: the tests set PATH to the stub
# directory alone, so env cannot find bash. It appends its argv to the file
# named by QM_STUB_RECORD and prints what the real command prints.
set -euo pipefail

printf '%s\n' "$*" >>"${QM_STUB_RECORD}"

VERB="${1}"

if [[ "${VERB}" == "${QM_STUB_FAIL_VERB:-}" ]]; then
    printf '%s\n' "${QM_STUB_FAIL_MESSAGE}" >&2
    exit 255
fi

case "${VERB}" in
    status)
        printf 'status: %s\n' "${QM_STUB_STATUS}"
        ;;
    listsnapshot)
        printf '%s\n' \
            '`-> keep-pre-upgrade-26x-1757000000 2026-09-04 15:33:20     no-description' \
            ' `-> pre-upgrade-26x-1757500000 2026-09-10 10:26:40     no-description' \
            '  `-> current                                             You are here!'
        ;;
    *)
        ;;
esac
