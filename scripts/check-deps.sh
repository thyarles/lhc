#!/usr/bin/env bash
# Dependency allowlist gate. lhc ships as one static binary that runs as root
# on production hosts, so every module it links is a decision, not a default.
# Allowed: cobra (+ pflag and mousetrap, its own dependencies), yaml, and the
# Go team's golang.org/x modules. Change this list deliberately, together
# with the depguard rule in .golangci.yml.
set -euo pipefail
cd "$(dirname "$0")/.."

allowed='^(github\.com/spf13/cobra|github\.com/spf13/pflag|github\.com/inconshreveable/mousetrap|go\.yaml\.in/yaml/v3|gopkg\.in/yaml\.v3|golang\.org/x/)'

# What go.mod requires, and what the binary actually links. (`go list -m all`
# would also list modules cobra needs only for its own docs and tests.)
required=$(go mod edit -json | sed -n 's/.*"Path": "\(.*\)".*/\1/p' | grep -v '^github.com/thyarles/lhc$' || true)
linked=$(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' ./... | sort -u)
bad=$(printf '%s\n%s\n' "$required" "$linked" | sort -u | grep -v '^$' | grep -Ev "$allowed" || true)
if [ -n "$bad" ]; then
    echo "Dependencies outside the allowlist:" >&2
    echo "$bad" | sed 's/^/  /' >&2
    exit 1
fi
echo "deps: OK — linked modules: $(echo "$linked" | tr '\n' ' ')"
