#!/usr/bin/env bash
#
# One-call installer for lhc (Linux Health Check).
#
#   # the newest release
#   curl -fsSL https://raw.githubusercontent.com/thyarles/lhc/main/install.sh | bash
#
#   # a specific release, with settings and a schedule
#   curl -fsSL .../install.sh | bash -s -- v1.2.0 \
#       --set email.daily_recipients=ops@example.com --set smtp.host=relay.example.com \
#       --time 06:30 --every 6h
#
#   # install the binary and config only, no timer/cron entry
#   ... | bash -s -- --no-schedule
#
# What it does: downloads lhc_linux_<arch>.tar.gz and checksums.txt from the
# GitHub release, verifies the SHA-256, installs /usr/local/bin/lhc, creates
# /etc/lhc/config.yaml if there is none, applies --set, and runs `lhc install`
# (systemd timer, or cron on older systems). Re-running it upgrades in place:
# the config, state and reports are kept.
#
# No git, no Go, and no GitHub API: releases/latest/download/... is a plain
# redirect, so a shared office NAT cannot exhaust an API rate limit.
#
# Env: REPO_SLUG (default thyarles/lhc), BIN_DIR (default /usr/local/bin),
#      DOWNLOADER=curl|wget to force one, BASE_URL to fetch the archive and
#      checksums.txt from an internal mirror instead of GitHub.
set -euo pipefail

REPO_SLUG="${REPO_SLUG:-thyarles/lhc}"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

say() { printf '\n==> %s\n' "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

TAG=""
SETS=()
SCHED=()
SCHEDULE=1
while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help) sed -n '2,28p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
        --set)     shift; [ $# -gt 0 ] || die "--set needs key=value"; SETS+=("$1") ;;
        --set=*)   SETS+=("${1#--set=}") ;;
        --time|--every|--backend)
                   opt="$1"; shift; [ $# -gt 0 ] || die "$opt needs a value"; SCHED+=("$opt" "$1") ;;
        --time=*|--every=*|--backend=*) SCHED+=("$1") ;;
        --no-random)   SCHED+=("--no-random") ;;
        --no-schedule) SCHEDULE=0 ;;
        -*) die "unknown option '$1' (see --help)" ;;
        *)  [ -z "$TAG" ] || die "two versions given: '$TAG' and '$1'"; TAG="$1" ;;
    esac
    shift
done

[ "$(id -u)" -eq 0 ] || die "must run as root (installs to $BIN_DIR and /etc/lhc)."

case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "unsupported architecture $(uname -m) (amd64 and arm64 are built)" ;;
esac

if [ -n "${BASE_URL:-}" ]; then
    BASE="$BASE_URL"
elif [ -n "$TAG" ]; then
    case "$TAG" in v*) ;; *) TAG="v$TAG" ;; esac
    BASE="https://github.com/$REPO_SLUG/releases/download/$TAG"
else
    BASE="https://github.com/$REPO_SLUG/releases/latest/download"
fi
ARCHIVE="lhc_linux_${ARCH}.tar.gz"

# Old curl builds (RHEL 6, some 7.x) offer only TLS 1.0, which GitHub has
# refused since 2018: they fail with "curl: (35) Peer reports incompatible or
# unsupported protocol version". curl being *present* does not mean it works,
# so a curl failure falls back to wget rather than aborting the install.
have()  { command -v "$1" >/dev/null 2>&1; }
_curl() { curl -fsSL "$1" -o "$2"; }
_wget() { wget -q -O "$2" "$1"; }
case "${DOWNLOADER:-auto}" in
    curl) have curl || die "DOWNLOADER=curl but curl is not installed."; fetch() { _curl "$1" "$2"; } ;;
    wget) have wget || die "DOWNLOADER=wget but wget is not installed."; fetch() { _wget "$1" "$2"; } ;;
    auto)
        if have curl && have wget; then
            fetch() {
                _curl "$1" "$2" && return 0
                printf '    curl could not fetch it (old TLS?), retrying with wget\n' >&2
                _wget "$1" "$2"
            }
        elif have curl; then fetch() { _curl "$1" "$2"; }
        elif have wget; then fetch() { _wget "$1" "$2"; }
        else die "neither curl nor wget is available."
        fi ;;
    *) die "DOWNLOADER must be curl, wget, or unset." ;;
esac

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

say "Downloading ${TAG:-the latest release} ($ARCHIVE)"
fetch "$BASE/$ARCHIVE" "$TMP/$ARCHIVE"   || die "download failed: $BASE/$ARCHIVE"
fetch "$BASE/checksums.txt" "$TMP/checksums.txt" || die "download failed: $BASE/checksums.txt"

say "Verifying SHA-256"
# Not `sha256sum -c --ignore-missing`: RHEL 7's coreutils predates that flag.
want="$(grep " $ARCHIVE\$" "$TMP/checksums.txt" | awk '{print $1}')"
[ -n "$want" ] || die "$ARCHIVE is not listed in checksums.txt"
if have sha256sum; then got="$(sha256sum "$TMP/$ARCHIVE" | awk '{print $1}')"
else got="$(openssl dgst -sha256 "$TMP/$ARCHIVE" | awk '{print $NF}')"; fi
[ "$want" = "$got" ] || die "checksum mismatch for $ARCHIVE (want $want, got $got)"
printf '    OK %s\n' "$got"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" lhc || die "archive did not unpack"
install -m 0755 "$TMP/lhc" "$BIN_DIR/lhc"
# Fail here rather than at 00:07 if the binary cannot run on this host.
"$BIN_DIR/lhc" version >/dev/null || die "$BIN_DIR/lhc will not run on this host"
say "Installed $("$BIN_DIR/lhc" version)"

CONF=/etc/lhc/config.yaml
if [ -f "$CONF" ]; then
    say "Config kept as-is: $CONF"
else
    say "Creating $CONF"
    "$BIN_DIR/lhc" config init --config "$CONF"
fi

if [ ${#SETS[@]} -gt 0 ]; then
    say "Applying ${#SETS[@]} setting(s)"
    "$BIN_DIR/lhc" config set --config "$CONF" "${SETS[@]}"
fi

if [ "$SCHEDULE" -eq 1 ]; then
    say "Scheduling"
    "$BIN_DIR/lhc" install --config "$CONF" ${SCHED[@]+"${SCHED[@]}"}
fi

if crontab -l 2>/dev/null | grep -q 'linux-healthcheck-managed'; then
    say "The Python linux-health-check cron entry is still installed"
    printf '    lhc does not remove it. Once lhc has run for a while, delete the line\n'
    printf '    marked linux-healthcheck-managed with: crontab -e\n'
fi

say "Done. Next:"
printf '    lhc config show --diff     # what this host overrides\n'
printf '    lhc report                 # preview a report (sends nothing)\n\n'
