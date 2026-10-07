# Working on lhc

Conventions for anyone (human or tool) changing this repository.

## Non-negotiables

- **One static binary.** `CGO_ENABLED=0`, linux/amd64 and linux/arm64. It runs
  as root on production hosts from RHEL 7 (kernel 3.10, systemd 219) to
  current Debian and SUSE.
- **Dependencies are an allowlist:** the standard library, `spf13/cobra` (and
  its pflag/mousetrap), `go.yaml.in/yaml/v3`, `golang.org/x/*`. Enforced by
  `scripts/check-deps.sh` and depguard in `.golangci.yml`. Kubernetes is read
  by running `kubectl`, not by linking client-go.
- **Checks never touch the OS directly.** Every command, file read and
  `LookPath` goes through `runner.Runner` (`env.Runner`); time comes from
  `env.Now()`, state from `env.State`. forbidigo rejects `exec.Command` and
  `os.Hostname` outside `internal/runner` and `internal/host`. This is what
  makes every check testable with `checktest`.
- **Quiet by default.** A check reports change, not inventory. A missing
  subsystem is one INFO line (`NotApplicable`), never CAUTION. The regression
  tests are mostly "must NOT alert" cases taken from real incidents. Keep them.
- **Never commit real hostnames, IPs or e-mail addresses.** Use
  `example.com` and RFC 5737 addresses in tests and docs.

## Commands

```sh
make check    # go test -race, golangci-lint, govulncheck, dependency allowlist, go mod tidy -diff
make e2e      # end-to-end: built binary, fake commands, in-process SMTP relay
make build    # ./lhc, static
make report   # preview on this machine
go test ./internal/report -update   # rewrite the report golden files after an intended change
```

Toolchain: Go from `go.mod`; golangci-lint v2 (`version: "2"` config);
gofumpt + goimports (`golangci-lint fmt`).

## Layout

| Path | What |
|---|---|
| `cmd/lhc` | `main`; the only `os.Exit` |
| `internal/cli` | one file per subcommand; `run.go` holds the run pipeline |
| `internal/check` | Status, Section/Row/Alert, the Check interface, registry, `RunAll` |
| `internal/checks/<name>` | one package per check |
| `internal/checks/all` | blank-imports every check: the only list of checks |
| `internal/checktest` | fake Runner, in-memory state, Env builder |
| `internal/runner` | the real Runner (fixed PATH, `LC_ALL=C`, timeouts, process-group kill) |
| `internal/config` | YAML schema, defaults, strict loader, `config set` that keeps comments, the embedded example |
| `internal/state` | one JSON file per owner, atomic writes, read-only mode, run lock |
| `internal/alerts` | fingerprinting and the notify/remind/resolve state machine |
| `internal/report` | one Model, three renderers (HTML template, 78-column text, JSON), goldens |
| `internal/notify`, `notify/smtp` | delivery planning, subject lines, SMTP + MIME, `smtptest` fake relay |
| `internal/schedule` | random delay, systemd timer / crontab writers, backend detection |
| `internal/host`, `internal/paths` | host identity and package manager; file locations |

## Adding a check

1. `internal/checks/<name>/<name>.go`: `type Check struct{}`,
   `func init() { check.Register(Check{}) }`, `Meta()` with a unique `Order`
   (multiples of 10; see the existing ones), and a `Config` struct embedding
   `check.Toggle` with `yaml:",inline"`. Add `Validate() error` on `*Config`
   if values must be in range.
2. Add one blank import to `internal/checks/all/all.go`.
3. Add a commented `checks.<name>:` block to `internal/config/config.example.yaml`.
   `TestTheExampleIsExactlyTheDefaults` fails until it matches `Defaults()`.
4. Tests with `checktest.NewEnv`: feed real command output to the fake
   Runner and assert what must **not** alert as well as what must.
5. If the report output changed, `go test ./internal/report -update` and
   review the golden diff.

Separator rows are `s.Separator("Title")`. Do not HTML-escape in a check;
the renderer escapes. Keep labels and alert messages stable: an alert's text,
with digits removed, is its identity in the alert history. Rewording it makes
every open condition look new once.

## Adding a notifier

Implement `notify.Notifier` (`Send(ctx, Message) error`). Return nil only when
the message was accepted: `run.go` commits the alert history only then.

## Config, state and paths

- Two layers: compiled defaults, then the file. Unknown keys are errors.
  Durations accept `168h`, `7d`, `90m`, `2h30m`, or a bare number of hours.
- State: `<state_dir>/<owner>.json`, envelope `{version, saved_at, data}`.
  A corrupt or other-version file reads as "first run". Bump
  `state.Version` only for an incompatible change.
- `lhc report` and `--dry-run` use a read-only state directory.

## Commits and releases

Conventional commits: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`,
`ci`; `!` marks a breaking change. Every merge to `main` is released by
`.github/workflows/release.yml` (version from the commit messages, tag,
GoReleaser). Nothing is committed back to `main`: the version is injected
with `-ldflags`.

Commit messages carry no AI attribution trailers.
