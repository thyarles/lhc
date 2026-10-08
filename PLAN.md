# Plan: rewrite linux-health-check in Go as `lhc`

**Status (2026-10-07):** milestones 0–4 are implemented and committed locally; all 21 checks are ported, and `make check`, `make e2e` and a smoke run on debian:12, rockylinux:9, centos:7 and opensuse/leap:15 containers are green. Not done yet: creating the GitHub repository and the first release (needs the owner's go-ahead), the RHEL 7 VM check, the week-long side-by-side runs against the Python version (milestones 2 and 4), and milestone 5 (cutover). See "Implementation notes" at the end for where the build differs from this plan.

## Context

`/home/charles/git/lhc-go` is currently a byte-identical copy of `thyarles/linux-health-check` v2.2.0 (Python, stdlib-only, ~3,800 runtime lines, 21 checks, INI config, JSON state, SMTP mail, cron). It is not a git repo. Go is not installed on this machine. The Python copy will be deleted and the Go project started from scratch; the Python reference stays at `../linux-health-check`.

Goal: a new project **`lhc`** shipping one static binary `lhc` for Debian/Ubuntu and the RHEL family, with the same product behaviour (two-audience alerting, dedup state machine, noise-filtered checks, HTML+text reports) but: checks as modules, notifiers behind an interface, a built-in scheduler for more than one run a day, and CI/CD where merging to `main` cuts a release.

### Target systems

| Target | Package manager | Notes |
|---|---|---|
| Debian 10+, Ubuntu 18.04+ | apt-get | primary |
| RHEL 7+, Rocky, Alma, CentOS Stream, Oracle | yum / dnf | primary; RHEL 7 has kernel 3.10 and systemd 219 |
| SLES / openSUSE Leap 15+ | zypper | package-manager table, install commands and the pending-updates parser (`zypper --non-interactive list-updates` / `list-patches --category security`) |

WSL2 is a development and test environment only, not a target. No WSL2-specific code. The generic rule "a missing subsystem (systemd, journald, cron, docker) collapses to one INFO line, never a crash" already covers it.

### Prior art (checked 2026-09-24): no reason to give up

| Tool | Overlap | Why it does not replace this |
|---|---|---|
| Lynis | security audit | point-in-time hardening audit; no run-to-run diff, no alert routing |
| logwatch / logcheck | daily log digest | logs only; no disk/k8s/ports/suid state |
| 0xgruber/linux-health-checks (Python) | 35+ checks, SMTP | no state between runs, no dedup, no Kubernetes |
| dawgmon (Python) | state diff of suid/ports/packages | unmaintained, no report or routing |
| osquery, netdata, monit | host telemetry | daemons with servers/dashboards; different model |

Nothing in Go does "one binary, daily snapshot + change detection + two-audience email routing + Kubernetes-node awareness". Proceed.

### Decisions locked with the user

1. New repo `thyarles/lhc` (public; planned as `lhc-go`, renamed on 2026-10-08 because the language suffix means nothing to users), module `github.com/thyarles/lhc`, binary `lhc`. Python project stays for legacy hosts.
2. Clean break: YAML config, new state layout, no importer.
3. Scheduling: `lhc install` → systemd timer when usable, else crontab; `lhc serve` built-in daemon. One run path.
4. Dependencies: standard library + `spf13/cobra` + `go.yaml.in/yaml/v3` + `golang.org/x/*`, enforced in CI. Kubernetes is read by running `kubectl`, not by linking the Kubernetes client library (see glossary: client-go). `CGO_ENABLED=0`, linux/amd64 + arm64.
5. SMTP only in v1, behind a `Notifier` interface.
6. Checks self-register; Kubernetes is one module.
7. Default schedule: **00:07 with an 8h random window**, so the run lands between 00:07 and 08:07, as in the Python version (confirmed).
8. SUSE (zypper) is a supported target alongside Debian and the RHEL family.

### Verified facts

- Go 1.27.1 is current (2026-09-01). Go 1.24+ needs kernel ≥ 3.2; RHEL 7 (3.10) is fine. A static binary does not care which glibc the host has.
- RHEL 7 ships systemd 219: timers exist but `RandomizedDelaySec`/`Persistent` need 229 ⇒ use cron there.
- golangci-lint config schema is `version: "2"`.
- The `tmp` file in the Python copy contains real recipient addresses: it is deleted with the rest and never committed.

---

## Glossary (plain language)

- **client-go** is the official Go library for talking to a Kubernetes API server directly. It is huge (over a hundred transitive modules) and version-coupled to Kubernetes releases. "Shell out to kubectl" means `lhc` runs the `kubectl` command that is already on the node and parses its output, exactly as the Python version does. Same behaviour, no giant dependency, and the node's own kubeconfig and permissions apply.
- **cobra** is the command-line framework used by kubectl, docker and gh. It gives `lhc run`, `lhc config set`, `--help`, and shell completion.
- **Static binary / CGO_ENABLED=0** means the binary has no dependency on the host's C library. One file copied anywhere runs.
- **go:embed** compiles a file (the example config, the HTML template) into the binary, so there is nothing to ship beside it.
- **-ldflags -X** injects the version, commit and build date into the binary at build time, so no file in the repo has to be edited on release.
- **GoReleaser** builds the binaries for every architecture, packs the archives, computes `checksums.txt` and creates the GitHub release.
- **golangci-lint** runs many static checkers at once. **depguard** and **forbidigo** are two of them, used here to enforce the dependency allowlist and to forbid running commands anywhere except one package.
- **govulncheck** is the Go team's scanner that compares the code paths the binary actually calls against the Go vulnerability database.
- **Dependabot** opens pull requests when a dependency or a GitHub Action has a newer version.
- **Jitter / random window** is the existing "wait a random slice of 8h before starting" behaviour.
- **Golden file test** stores the expected output (a report) in `testdata/`; the test fails if the rendering changes, and `-update` rewrites it deliberately.

---

## Repository layout

```
lhc/
  PLAN.md                       this plan
  go.mod                        module github.com/thyarles/lhc ; go 1.27
  cmd/lhc/main.go               cobra root; os.Exit only here
  internal/cli/                 one file per subcommand: run, report, install, uninstall, serve, config, tools, paths, version
  internal/version/             Version/Commit/Date set via -ldflags
  internal/config/              YAML schema, defaults, load, set/show/validate, embedded config.example.yaml
  internal/paths/               root vs non-root resolution of config/state/reports/log
  internal/runner/              Runner interface + real impl (os/exec, fixed PATH, LC_ALL=C for children, timeouts, process-group kill)
  internal/state/               Store: one JSON file per check, {version,saved_at,data} envelope, atomic write, ReadOnly wrapper
  internal/host/                Label() (kernel hostname, never getfqdn), MailDomain(), OSRelease(), PkgManager(), InstallCmd()
  internal/check/               Status, Row, Section, Tool, Info, Check interface, Env, registry, RunAll
  internal/checktest/           fake Runner (substring rules, once), in-memory State, Env builder, helpers
  internal/checks/all/all.go    blank-imports every check package — the ONLY list of checks
  internal/checks/<name>/       one package per check; kubernetes/ has discover/nodes/pods/pvcs/events/images files
  internal/alerts/              fingerprint, Evaluate, Decision, Commit (port of hc/alerts.py)
  internal/report/              Model builder; html.go (html/template, embedded), text.go (strings.Builder, 78 cols), json.go
  internal/notify/              Notifier interface, Message, PlanDelivery, Subject
  internal/notify/smtp/         net/smtp + tls, hand-built MIME
  internal/schedule/            window parsing, jitter, backend detection, systemd unit/timer + crontab writers
  internal/textwrap/            wrap/meter/fit helpers shared by text report and tests
  test/e2e/                     builds the binary, fake PATH of shell scripts, in-process SMTP fake
  scripts/check-deps.sh         dependency allowlist gate
  install.sh  .goreleaser.yaml  .golangci.yml  .github/{workflows/check.yml,workflows/release.yml,dependabot.yml}
  CLAUDE.md  README.md  LICENSE (MIT, carried over)  Makefile
```

**One package per check** (not one file in a shared package): the linters can then forbid `os/exec` outside `internal/runner`, so no check can bypass the fakeable Runner; helpers don't collide; each check keeps its own `testdata/`; no import cycle because `internal/check` never imports a check and only `checks/all` imports them all.

## Core interfaces

```go
// internal/check
type Status int            // OK < Info < Caution < Unhealthy; String() "ok","info","caution","unhealthy"
type Row struct { Label, Value, Detail string; Status Status; Separator bool; Meter *float64; Delta string }
type Alert struct { Status Status; Msg string }
type Tool struct { Name, RHELPkg, DebPkg, SUSEPkg string; Optional bool }   // host.PkgManager(): dnf | yum | apt-get | zypper
type Section struct { Name, Title string; Status Status; Applicable bool; Rows []Row; Alerts []Alert; MissingTools []Tool; Err error }
// methods: Add, Separator, NotApplicable, Alert, NeedTool

type Info struct { Name, Title string; Order int; Tools []Tool; Default bool }
type Check interface {
    Info() Info
    Defaults() any                               // *cpu.Config with defaults; decoded from checks.<name> in YAML
    Run(ctx context.Context, env *Env) *Section  // panics recovered by RunAll into Section.Err
}
func Register(c Check); func All() []Check; func Lookup(name string) (Check, bool)

type Env struct { Runner Runner; State State; Log *slog.Logger; Now func() time.Time; Host HostInfo }
func (e *Env) Config(out any) error   // strict decode of checks.<name>
type Runner interface {
    Run(ctx context.Context, name string, args ...string) Result   // no shell
    Shell(ctx context.Context, script string) Result                // sh -c, only for real pipelines
    LookPath(name string) (string, bool)
    Readable(path string) bool
}
type Result struct { Code int; Stdout, Stderr string; Err error }
type State interface { Load(out any) (found bool, err error); Save(v any) error }
```

Orchestration: `RunAll` runs enabled checks sequentially (parallel runs would cause the CPU spike the tool then reports), overall = worst status, alerts concatenated in `Order`. `Applicable=false` sections collapse to one line as today. Order is explicit via `Info().Order`, never Go init order.

**Adding a check** = new package with `init(){ check.Register(&Check{}) }` + one blank import in `checks/all/all.go` + `checks.<name>:` block in the example YAML. CLAUDE.md documents this.

## Config (YAML, two layers: struct defaults < file)

```yaml
hostname: ""                     # blank = kernel hostname
smtp:   { host: relay.example.com, port: 25, tls: none, username: "", password: "", from: "" }   # tls: none|starttls|implicit
email:  { daily_recipients: [], alert_recipients: [], html_mode: inline }
alerts: { notify_all_on: unhealthy, remind_caution: 168h, remind_unhealthy: 24h, forget_after: 72h }
schedule: { time: "00:07", every: "", random: true, random_window: 8h }
reports: { keep: 30 }
paths: {}                        # optional state_dir, report_dir, log_file
checks:
  cpu:        { enabled: true, caution: 80, unhealthy: 95, load_caution_mult: 1.0, load_unhealthy_mult: 2.0 }
  disk:       { enabled: true, caution: 90, unhealthy: 95, ignore: [] }
  kubernetes: { enabled: true, scope: auto, kubeconfig: "", nodes: true, pods: true, pvcs: true, events: true, images: true, list_images: false, pending_minutes: 15, restart_delta_caution: 3, evicted_recent_hours: 24, max_pods: 2000 }
  # ... every check has its own block; thresholds live with the check that owns them
```

- Paths: `--config` > `$LHC_CONFIG` > `/etc/lhc/config.yaml` (root) > `~/.config/lhc/config.yaml` > next to binary. State `/var/lib/lhc` (or `~/.local/state/lhc`), reports under it, log `/var/log/lhc.log` (or under state dir).
- `config init` (0600, embedded commented example; a test asserts example == struct defaults), `config show [--diff]`, `config set k.v=x` (edits the YAML tree so comments survive, validates, keeps `.bak`), `config validate` (strict, unknown keys fail). `prune` is dropped (no base file to prune against).
- `config.Duration` accepts `168h`, `7d`, `90m`, bare number = hours (same grammar as the Python window parser).

## State, alerts, notify

- State: atomic write (temp + fsync + rename); corrupt, missing or version-mismatched file ⇒ `found=false` (Python's `None`). `lhc report` and `lhc run --dry-run` wrap all stores in `ReadOnly` (port of freeze_state).
- Alerts: port `hc/alerts.py` verbatim (fingerprint digits→`#` lowercased; new/escalated/ongoing/reminder/resolved; `Commit()` only after SMTP accepted or nobody to send to). Port all 17 behaviours from `tests/test_alerts.py` and the 13 routing/subject tests from `tests/test_routing.py`.
- Notify: `Notifier.Send(ctx, Message) error`; SMTP with none/starttls/implicit TLS, PLAIN + LOGIN auth, hand-built `multipart/alternative` (inline) or `multipart/mixed` (attachment/both), RFC 2047 subject (it contains `⚠`/`·`), quoted-printable bodies. Tested against an in-process scripted SMTP server.

## Reports

`report.Build(...)` → one `Model` (worst-first ordering, triage block, at-a-glance grid) feeding three renderers: `html/template` (embedded, `color-scheme: only light`, inline colours, chips with dot+word), text (`strings.Builder`, W=78, Python wrap rules), JSON (`{"schema":1,"version","host","generated_at","overall","triage":{...},"sections":[...]}`). Golden files under `internal/report/testdata`. CLI: `lhc report [--format text|html|json]` (frozen state), `lhc run [--format json]`.

## Scheduling

- `schedule.ParseWindow` / `Draw` port `hc/schedule.py` + its 15 tests (malformed window falls back to 8h with a warning, never to 0).
- `Detect()`: systemd iff `/run/systemd/system` exists and `systemctl --version` ≥ 229; else cron if `crontab` exists; else advise `lhc serve`.
- `lhc install [--time HH:MM] [--every 6h] [--no-random] [--backend systemd|cron]`: writes config first, then either `lhc.service` (oneshot, `run --scheduled --no-random`, Nice=10) + `lhc.timer` (`OnCalendar`, `RandomizedDelaySec`, `Persistent=true`) or a marker-tagged crontab line (`7 12,18 * * *` style for `--every 6h`) with in-process jitter. Idempotent; prints the schedule in words. `lhc uninstall` removes either.
- `lhc run --scheduled`: append to log file, announce delay and target time before sleeping, context-cancellable sleep.
- `lhc serve [--every 6h] [--at HH:MM] [--now]`: `signal.NotifyContext`, SIGHUP reloads config, overlapping run skipped, never exits on check failure.

## Testing

- `checktest.Runner` mirrors Python's `FakeShell`: `Expect(substr, stdout).Code(n).Once()`, `Tool(name)`, `File(path)`, `Ran(substr)`; unmatched ⇒ `{0,"",""}`.
- Per check: table tests + one-to-one port of `tests/test_checks_noise.py` (72) and `tests/test_checks_kubernetes.py` (40), written as "must NOT alert".
- Host identity: pure `host.Label(kernelName, resolve)`; port the 17 cases from `tests/test_host_identity.py`.
- E2E (`test/e2e`, also in CI): build binary, fake `bin/` of shell scripts, `PATH` override honoured only via `LHC_PATH_OVERRIDE`, assert JSON output and that `alerts.json` is committed only after SMTP `250`.
- `go test -race ./...` everywhere.

## CI/CD

- `check.yml` (PR, push main, workflow_call): `test` (go test -race, go vet), `lint` (golangci-lint v2), `vuln` (govulncheck), `deps` (`scripts/check-deps.sh` + `go mod tidy` diff), `build` matrix amd64/arm64 with `CGO_ENABLED=0 -trimpath`, run `./lhc version`; `smoke` job (from milestone 3) runs the amd64 binary's `lhc report --format json` inside `debian:12`, `rockylinux:9`, `centos:7` and `opensuse/leap:15` containers to prove every package-manager branch parses.
- `release.yml` (push main): needs check → `paulhatch/semantic-version` with today's patterns (`feat:`→minor, `!:`→major, else patch) → skip if tag exists → tag + push → GoReleaser `release --clean`. No version commit back to main: version comes from `-ldflags`, and install.sh uses `releases/latest/download/...` (a redirect, not the rate-limited API).
- `.goreleaser.yaml`: linux amd64/arm64, `-s -w -X internal/version.{Version,Commit,Date}`, archives `lhc_linux_<arch>.tar.gz`, `checksums.txt` sha256, changelog grouped feat/fix.
- `.golangci.yml` v2: errcheck govet staticcheck unused ineffassign errorlint gocritic revive misspell unconvert unparam noctx gosec depguard forbidigo; formatters gofumpt goimports. depguard allowlist = stdlib, self, cobra, pflag, yaml, golang.org/x. forbidigo: `exec.Command|exec.LookPath|os.Hostname` only in runner/host.
- `scripts/check-deps.sh`: reject any `go.mod` require not matching `^(github.com/spf13/cobra|github.com/spf13/pflag|github.com/inconshreveable/mousetrap|go.yaml.in/yaml/v3|gopkg.in/yaml.v3|golang.org/x/)` (pflag and mousetrap are cobra's indirect deps and must be allowed).
- Dependabot: gomod + github-actions weekly, grouped. Actions pinned by SHA.

## CLAUDE.md contents

Purpose and non-negotiables (single static binary, allowlisted deps, checks never call os/exec directly); conventional commits (`feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`; `!` for breaking; merge to main releases); commands (`make check` = test+lint+vuln+deps, `make build`, `make report`); how to add a check (package, Register, blank import, YAML block, noise tests, golden update); how to add a notifier; config/state/path conventions; "must not alert" regression philosophy; release process; what never to commit (real hostnames/recipients).

## install.sh

`curl -fsSL https://raw.githubusercontent.com/thyarles/lhc/main/install.sh | bash -s -- [vX.Y.Z] [--set k=v ...] [--time HH:MM] [--every 6h] [--no-schedule]`: root check; curl→wget fallback (keep the TLS-1.0 rationale); arch map; fetch `lhc_linux_<arch>.tar.gz` + `checksums.txt` from `latest/download` or the tag; verify sha256 (grep fallback for RHEL 7 coreutils without `--ignore-missing`); `install -m 0755` to `/usr/local/bin/lhc`; `lhc version` must run; `config init` if absent; apply `--set`; `lhc install`; print a hint about removing the old Python cron entry (marker `linux-healthcheck-managed`), do not act.

## Milestones (each independently releasable)

0. **Clean slate + environment** — delete everything in the working copy except `LICENSE`; write `PLAN.md` (this plan); `git init` with a Go `.gitignore`; install Go 1.27.1 under `$HOME` (tarball to `~/go-sdk`, `~/go/bin` on PATH, no sudo), `golangci-lint`, `govulncheck`, `goreleaser` (for local snapshot builds).
1. **v0.1 skeleton** — go.mod, cobra root, version, paths, config, runner, state, check model/registry/RunAll, report text/html/json + goldens, checks `system` + `cpu`, CI + release + GoReleaser + allowlist + Dependabot, CLAUDE.md, README stub. Done when `lhc report` works locally and the released static binary runs on a RHEL 7 VM.
2. **v0.2 daily-mail parity on a plain VM** — `memory disk processes services tools` (+ `lhc tools [--install]` replacing bootstrap), alerts engine, notify + SMTP, `lhc run` (save report, `reports.keep`, commit-after-send), `--scheduled` jitter, `install/uninstall`, `serve`, install.sh, e2e test. Done when one VM runs `lhc` instead of Python for a week with identical alerts.
3. **v0.3 security/inventory** — `users auth fail2ban ports crontabs suid packages etc logs rootkit network updates docker`, all 72 noise regressions ported; zypper branch in `host.PkgManager`, `InstallCmd`, the `updates` check and the `packages` inventory (`rpm -qa` works on SUSE too). Done when Debian, RHEL-family and openSUSE hosts (or containers in CI) run it.
4. **v0.4 kubernetes** — one package, discovery over fixed candidate paths via `LookPath`/`Readable`, 40 tests ported. Done when an rke2 node runs both for a week with the same alerts.
5. **v1.0 cutover** — docs, README, Python repo points to lhc.

Implementation order is 0 then 1. Neither starts in this session (see the scope note at the top).

## Risks and gotchas to bake in

- Minimal PATH under cron/systemd: Runner sets a full PATH including `/var/lib/rancher/rke2/bin`, `/opt/bin`, `/snap/bin`; `HOME` may be unset under systemd ⇒ assume `/root`.
- Set `LC_ALL=C` for child processes only, so `df`, `ss`, `systemctl`, `dnf` output parses on every locale; keep `TZ`.
- Pure-Go resolver: wrap the FQDN lookup in a 2s timeout so a dead resolver cannot hang a scheduled run.
- `exec.CommandContext` kills only the direct child: use `Setpgid` and kill the group for `Shell()` pipelines; keep kubectl `--request-timeout`.
- Report retention is new (`reports.keep`) because `--every 6h` would otherwise fill the disk.
- Absent subsystems (systemd, journald, cron, docker daemon down) always collapse to one INFO line; this is what makes a laptop or WSL2 a valid test box without special code.

## Verification (milestone 1)

1. `make check` green locally: `go test -race ./...`, `golangci-lint run`, `govulncheck ./...`, `scripts/check-deps.sh`.
2. `go build -trimpath ./cmd/lhc && file lhc` shows a statically linked binary; `./lhc version` prints version/commit.
3. `./lhc config init --config /tmp/x.yaml && ./lhc config validate --config /tmp/x.yaml && ./lhc report --format text|html|json` produce the system+cpu sections; JSON matches the documented shape; goldens match.
4. Push to GitHub: `check.yml` green on the PR; merge to main produces `v0.1.0` with `lhc_linux_amd64.tar.gz`, `lhc_linux_arm64.tar.gz`, `checksums.txt`.
5. On a RHEL 7 host (kernel 3.10, systemd 219): download the release, run `lhc version` and `lhc report`; confirm `lhc install` chooses cron.

---

## Implementation notes (2026-10-07)

Where the implementation differs from the plan above, and why:

- **`check.Meta`, not `check.Info`.** `Info` is already the name of a status constant.
- **State is keyed.** The plan had `Load(out)`/`Save(v)`. Several checks keep more than one baseline (kubernetes keeps four), so `State.Load(key, &out)`/`Save(key, v)` write one file per check: `<state>/<check>.json`.
- **The Runner also reads files** (`ReadFile`, `ReadDir`, `Exists`, `IsSocket`, `Readable`). That way /proc, /etc and log files are fakeable in tests too.
- **`AGENTS.md` replaces `CLAUDE.md` in git.** It is the vendor-neutral name. A git-ignored local `CLAUDE.md` holds `@AGENTS.md`, so the repository does not name the AI tool used.
- **`kubernetes.scope: off` is gone.** Every check has `enabled: false` instead.
- **New settings:** `smtp.tls_skip_verify`; `logs.extra_patterns` for site-specific patterns; `paths.*`; `reports.keep`.
- **Delivery failure exits 2.** It is logged, and the alert history is not committed, as before.
- **Scheduled runs write their own log** (`paths.log_file`, rotated at 5 MB). A state-directory lock stops two runs from overlapping.
- **The alert engine** now reports a condition as cleared again if it comes back and clears a second time. The Python version stayed silent the second time.
- **Many parsers moved from awk/grep pipelines into Go**, and several Python bugs were fixed along the way. Each one has a test:
  - zombies in `Z+`/`Zs` state were missed;
  - the root-login date check took "Jan 1" to mean "Jan 15";
  - a partial SUID scan re-baselined, flagging everything it missed as new;
  - the netstat UDP column was misread;
  - Docker disk usage labels were wrong.
- **The smoke CI job uses `docker run`**, not a job container. GitHub's actions cannot start inside centos:7 because its glibc is too old.
