# lhc — Linux Health Check

One static binary that inspects a Linux host every day, remembers what it saw,
and mails a report to two audiences:

- **the daily list** gets every run's report: "the check ran, here is the
  state of the machine";
- **the alert list** hears only about conditions it has **not already been
  told about**: a new problem, one that got worse, or a reminder that one is
  still open.

A disk that has sat at 91% for three weeks is CAUTION in every report, but it
interrupts the broad list once, not every morning. That is the whole point:
an alert that fires daily teaches people to ignore it.

lhc is the Go successor of
[linux-health-check](https://github.com/thyarles/linux-health-check) (Python).
Same checks and the same alert behaviour, with no interpreter to install.

## What it checks

| Check | What it looks at |
|---|---|
| system | name, OS, kernel, uptime, **reboot since the last run** |
| cpu | load average against the core count, per-core utilisation (summarised) |
| memory | RAM and swap, with the change since the last run |
| disk | every real filesystem, de-duplicated (kubelet/container bind mounts are not disks) |
| processes | top 5 by memory and CPU, zombie pile-ups |
| services | failed systemd units, units stuck activating across two runs |
| docker | fresh crashes and restart loops (old stopped containers are not news) |
| kubernetes | node readiness/pressure/cordons, broken pods, restart growth, lost PVCs, node-level events |
| updates | pending (and security) updates: dnf, yum, apt, zypper |
| users | logged-in users, root logins **today** |
| auth | failed SSH attempts today, top sources, sudo use |
| fail2ban | jails and bans (bans are fail2ban working, not a problem) |
| ports | **new** listening sockets since the last run (loopback hidden) |
| crontabs | new crontab entries |
| suid | new SUID binaries |
| packages | packages installed or removed since the last run |
| etc | changes under /etc in the last 24 h, security-relevant files flagged |
| logs | OOM kills, I/O and filesystem errors, kernel panics, segfault storms, SSH probing, today only |
| rootkit | rkhunter (if installed), known rootkit paths, processes hidden from `ps` |
| network | per-interface traffic, connection summary |
| tools | which helper tools are missing, and how to install them |

Every check is built to stay quiet when nothing changed. A missing subsystem
(no Docker, no systemd, no Kubernetes) becomes a single "not present on this
host" line, never a warning.

## Install

As root, on Debian/Ubuntu, RHEL/Rocky/Alma/CentOS (7 and later) or SUSE/openSUSE
Leap, amd64 or arm64:

```sh
curl -fsSL https://raw.githubusercontent.com/thyarles/lhc/main/install.sh | bash -s -- \
    --set smtp.host=relay.example.com \
    --set email.daily_recipients=ops@example.com \
    --set email.alert_recipients=team@example.com
```

The installer downloads the release archive, verifies its SHA-256, installs
`/usr/local/bin/lhc`, writes `/etc/lhc/config.yaml` and schedules the run:
a **systemd timer** where systemd 229+ is available, otherwise a **crontab**
entry (RHEL 7). Re-running it upgrades in place and keeps the config, state
and reports. `--help` lists the options (`--time 06:30`, `--every 6h`,
`--no-schedule`, a specific version, ...).

Without root, or to try it first: download `lhc_linux_amd64.tar.gz` from the
[releases](https://github.com/thyarles/lhc/releases), unpack it and run
`./lhc report`. Nothing is installed or changed.

## Use

```sh
lhc report                    # run the checks and print the report; sends and changes nothing
lhc report --format html > r.html
lhc run                       # what the schedule runs: check, save, mail
lhc run --dry-run             # same, but print instead of mailing

lhc config show --diff        # what this host changes from the defaults
lhc config set schedule.time=06:30 checks.disk.caution=85
lhc config validate

lhc install --every 6h        # re-schedule (00:07, 06:07, 12:07, 18:07)
lhc uninstall                 # remove the timer/cron entry, keep everything else
lhc tools [--install]         # which helper tools are missing
lhc paths                     # where the config, state, reports and log live
lhc serve                     # foreground scheduler, for hosts with neither systemd nor cron
```

`lhc report` and `lhc run --dry-run` never touch the saved baselines, so a
preview cannot hide tomorrow's "new port" or "new SUID file".

## Configuration

`/etc/lhc/config.yaml` (root) or `~/.config/lhc/config.yaml`. `lhc config init`
writes a fully commented example: every setting with its default and what it
is for. The file only needs what differs from the defaults, and a setting
added in a later release arrives with its default. Unknown keys are errors,
so a typo cannot silently do nothing.

The settings that matter on day one:

```yaml
smtp:
  host: relay.example.com
  port: 25
  tls: none            # none | starttls | implicit
email:
  daily_recipients: [ops@example.com]
  alert_recipients: [team@example.com]
alerts:
  notify_all_on: unhealthy   # or caution
schedule:
  time: "00:07"
  random_window: 8h          # each run starts at a random moment in this window
```

**Why the random window?** A fleet installs the same time, so every host
would start scanning at 00:07, often on top of the backups, and then report
the CPU spike it caused itself. Each scheduled run waits a random slice of
the window first (systemd's `RandomizedDelaySec`, or the same thing in
process under cron). A run you start by hand is never delayed.

## How alerting works

Each finding has a fingerprint: its message with the numbers removed, so
"84 pending updates" is the same condition as "83 pending updates". lhc keeps
the fingerprints it has already notified about:

| Situation | Daily list | Alert list |
|---|---|---|
| nothing wrong | heartbeat, "all clear" | — |
| new condition at or above `notify_all_on` | the alert | the alert |
| same condition, unchanged | heartbeat, "no new issues" | — |
| condition got worse (CAUTION → UNHEALTHY) | the alert | the alert |
| still open after `remind_caution` (7 d) / `remind_unhealthy` (24 h) | the alert | the alert (reminder) |
| condition cleared | mentioned once | — |

A condition is only recorded as notified after the relay **accepted** the
mail. If the relay is down, the next run treats it as new and tries again.

## Files

| | root | user |
|---|---|---|
| config | `/etc/lhc/config.yaml` | `~/.config/lhc/config.yaml` |
| state (baselines, alert history) | `/var/lib/lhc/*.json` | `~/.local/state/lhc/` |
| saved reports (`reports.keep`, default 30) | `/var/lib/lhc/reports/` | `~/.local/state/lhc/reports/` |
| log of scheduled runs | `/var/log/lhc.log` | `~/.local/state/lhc/lhc.log` |

## Moving from the Python version

lhc does not read `healthcheck.conf` or the Python state files; it starts its
own baselines on the first run. Translate the few settings you changed with
`lhc config set` (e.g. `[thresholds] disk_caution` is now
`checks.disk.caution`), install lhc, and once it has run for a while remove
the Python cron entry (the line marked `linux-healthcheck-managed`). The
installer tells you when that entry is still present; it never removes it.

## Development

Go 1.27, standard library plus [cobra](https://github.com/spf13/cobra) and
[yaml](https://github.com/yaml/go-yaml). Nothing else is allowed in, and CI
enforces that.

```sh
make check    # tests (-race), golangci-lint, govulncheck, dependency allowlist
make e2e      # the real binary against fake commands and a fake SMTP relay
make report   # build and preview a report on this machine
```

Every merge to `main` cuts a release: conventional commits decide the
version (`feat:` → minor, `fix:` and others → patch, `!` → major), and
GoReleaser publishes the archives and `checksums.txt`. See
[CLAUDE.md](CLAUDE.md) for the codebase conventions, including how to add a
check.

This project was developed with the help of AI tools.

## License

MIT, see [LICENSE](LICENSE).
