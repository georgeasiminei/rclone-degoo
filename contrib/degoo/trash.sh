#!/bin/sh
# User-friendly POSIX wrapper for the Degoo trash commands.
set -eu

BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
AUTH="${RCLONE_DEGOO_AUTH:-}"
AUTH_SET=0
RCLONE="${RCLONE_DEGOO_BIN:-}"
SUBCOMMAND=""
LIMIT=0
JSON=0
DRY_RUN=0
YES=0

usage() {
    cat <<'EOF'
Usage:
  trash.sh list [AUTH_FILE] [--limit N] [--json]
  trash.sh empty [AUTH_FILE] [--dry-run] [--yes]

Options:
  --auth FILE    Account auth file.
  --limit N      Maximum number of trash items to list; 0 means all.
  --json         Return machine-readable JSON for list.
  --dry-run      Show what would be deleted without changing anything.
  --yes          Skip the two confirmation prompts for empty.
  --rclone PATH  Path to the rclone executable.
  --help, -h     Show this help.

The default auth file is
~/.local/share/rclone-degoo/config/auth.json. The default executable is
build/rclone from this repository, then rclone on PATH. Set RCLONE_DEGOO_BIN
or pass --rclone to override it.

Trash is account-wide and empty is irreversible. Generic rclone cleanup is
deliberately not supported yet, and no restore operation exists.
EOF
}

while [ "$#" -gt 0 ]; do
    arg=$1
    case "$arg" in
        --help|-h)
            usage
            exit 0
            ;;
        list|empty)
            if [ -n "$SUBCOMMAND" ]; then
                printf '%s\n' 'Only one subcommand is allowed.' >&2
                exit 2
            fi
            SUBCOMMAND=$arg
            ;;
        --auth)
            if [ "$#" -lt 2 ]; then
                printf '%s\n' '--auth requires a file path.' >&2
                exit 2
            fi
            AUTH=$2
            AUTH_SET=1
            shift
            ;;
        --auth=*)
            AUTH=${arg#--auth=}
            AUTH_SET=1
            ;;
        --rclone)
            if [ "$#" -lt 2 ]; then
                printf '%s\n' '--rclone requires a path.' >&2
                exit 2
            fi
            RCLONE=$2
            shift
            ;;
        --rclone=*)
            RCLONE=${arg#--rclone=}
            ;;
        --limit)
            if [ "$#" -lt 2 ]; then
                printf '%s\n' '--limit requires a value.' >&2
                exit 2
            fi
            LIMIT=$2
            shift
            ;;
        --limit=*)
            LIMIT=${arg#--limit=}
            ;;
        --json)
            JSON=1
            ;;
        --dry-run)
            DRY_RUN=1
            ;;
        --yes)
            YES=1
            ;;
        --)
            shift
            break
            ;;
        *)
            if [ "$AUTH_SET" -eq 0 ]; then
                AUTH=$arg
                AUTH_SET=1
            else
                printf 'Unexpected argument: %s\n' "$arg" >&2
                exit 2
            fi
            ;;
    esac
    shift
done

if [ -z "$SUBCOMMAND" ]; then
    usage >&2
    exit 2
fi

if [ "$AUTH_SET" -eq 0 ] && [ -z "$AUTH" ]; then
    AUTH=$HOME/.local/share/rclone-degoo/config/auth.json
fi
if [ -z "$AUTH" ]; then
    printf '%s\n' 'An auth file is required.' >&2
    exit 2
fi
if [ ! -f "$AUTH" ]; then
    printf 'Auth file not found: %s\n' "$AUTH" >&2
    exit 1
fi

if [ -z "$RCLONE" ]; then
    if [ -x "$BASE/../build/rclone" ]; then
        RCLONE=$BASE/../build/rclone
    elif command -v rclone >/dev/null 2>&1; then
        RCLONE=$(command -v rclone)
    else
        printf '%s\n' 'No rclone executable was found; build this fork or use --rclone.' >&2
        exit 127
    fi
fi
if ! command -v "$RCLONE" >/dev/null 2>&1; then
    printf 'The rclone executable was not found: %s\n' "$RCLONE" >&2
    exit 127
fi

case "$SUBCOMMAND" in
    list)
        if [ "$DRY_RUN" -ne 0 ] || [ "$YES" -ne 0 ]; then
            printf '%s\n' 'Only --limit and --json are valid with list.' >&2
            exit 2
        fi
        set --
        if [ "$LIMIT" != "0" ]; then
            set -- "$@" --limit "$LIMIT"
        fi
        if [ "$JSON" -ne 0 ]; then
            set -- "$@" --json
        fi
        exec "$RCLONE" degoo trash "$AUTH" "$@"
        ;;
    empty)
        if [ "$LIMIT" != "0" ] || [ "$JSON" -ne 0 ]; then
            printf '%s\n' 'Only --dry-run and --yes are valid with empty.' >&2
            exit 2
        fi
        set --
        if [ "$DRY_RUN" -ne 0 ]; then
            set -- "$@" --dry-run
        fi
        if [ "$YES" -ne 0 ]; then
            set -- "$@" --yes
        fi
        exec "$RCLONE" degoo empty-trash "$AUTH" "$@"
        ;;
esac
