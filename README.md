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
| updates | pending (and security) updates: dnf, yum, apt, zypper; **a reboot waiting** for an installed kernel or core library, dated from that update: CAUTION after 14 days, UNHEALTHY after 30 (`reboot_caution_days`, `reboot_unhealthy_days`) |
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

Every report opens with the same **vital signs** strip, in the same order on
every host, so the numbers people look for first are always in one place:

```
Up 132 days · Updates 14 (3 security) · ! Reboot required · Disk 71% /var · RAM 45% · Load 0.42 / 8 cores
```

A value is coloured (and marked `!` or `✕`) only when its check flagged it. A
disabled check just drops its entry. In the JSON report the strip is the
top-level `vitals` list of `{key, value, status}`.

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
and reports. Check the mail settings straight away with `lhc test-mail`:
it takes seconds, where a first `lhc run` can take minutes on a host whose
package repositories are slow or unreachable. `--help` lists the options (`--time 06:30`, `--every 6h`,
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
lhc test-mail [--to ADDR]     # send a short test message through the relay; runs no checks

lhc config show --diff        # what this host changes from the defaults
lhc config set schedule.time=06:30 checks.disk.caution=85
lhc config validate

lhc install --every 6h        # re-schedule (00:07, 06:07, 12:07, 18:07)
lhc uninstall                 # remove the timer/cron entry, keep everything else
lhc tools [--install]         # which helper tools are missing
lhc paths                     # where the config, state, reports and log live
lhc serve                     # foreground scheduler, for hosts with neither systemd nor cron

lhc hub keygen              # once per hub: the shared key for collectors
lhc hub status              # on a collector: every host's latest report and its age
lhc hub forget HOST         # on a collector: drop a decommissioned host
lhc hub digest [--dry-run]  # on a collector: mail (or print) the daily digest now
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

**Why the random window?** Hosts are all installed with the same time, so
every one would start scanning at 00:07, often on top of the backups, and then report
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
| no alert, and a hub collector accepted the report (`email.heartbeat: auto`) | nothing: the collector has it | — |

A condition is only recorded as notified after the relay **accepted** the
mail, and the baselines (ports, SUID files, packages, crontabs) move only
then too. If the relay is down, the next run compares against the last
delivered run, finds the same changes and tries again. A relay that answers
"try again later" (a 4xx reply) gets one more attempt a minute later.
An interrupted run (Ctrl+C, or the service stopped mid-run) prints, sends
and saves nothing.

`email.heartbeat` decides the daily list's mail on a run with no alert:
`always` sends it, `never` drops it, and `auto` (the default) drops it only
when at least one hub collector accepted the report, so a host that cannot
reach its hub still mails. Without `hub.collectors`, `auto` is `always`.
Alerts are mailed in every mode.

## Hub

With many hosts, one mail per host per day stops being read, and a host that
stops reporting is invisible among the rest. Hosts can also send their report
to **collectors**: ordinary lhc hosts that also run `lhc collect` and keep the
latest report of every host in `/var/lib/lhc/hub/`.

```sh
# once, anywhere: the hub's shared key
lhc hub keygen --out ./hub.key

# on each of the two collectors (systemd needed)
curl -fsSL .../install.sh | bash -s -- --hub-key-file ./hub.key --collector

# on every host, collectors included
curl -fsSL .../install.sh | bash -s -- --hub-key-file ./hub.key \
    --set hub.collectors=http://192.0.2.10:8461,http://192.0.2.11:8461
```

- **Two collectors** are the expected setup. Each host sends to every one,
  and each collector is independent, so losing one loses nothing.
- **Separate networks.** Where VLANs must not talk to each other, give each
  VLAN its own pair of collectors and list only those on its hosts.
- **One way only.** A collector answers `POST /v1/report` with 204 or an
  empty 400, and 404 to anything else. It never sends a host anything to act
  on.
- **Encrypted end to end.** Each report is gzipped and sealed with AES-256-GCM
  under the shared key, with the host name and the time bound to it. Plain
  HTTP is therefore enough. Without the key a report can be neither read nor
  forged, a recorded request cannot be replayed, and a clock more than 10
  minutes off is refused.
- **Never in the way.** The report goes to the collectors before the mail,
  10 seconds at most for each. A collector that is down is logged and never
  changes the run's exit status. When one accepted the report, the daily
  heartbeat mail is skipped (`email.heartbeat`); alerts are always mailed.
- A refusal's reason is only in the collector's journal:
  `journalctl -u lhc-collect`.
- Open port 8461/tcp on the collectors to the hosts that report to them.

### The daily digest

Every day at `collector.digest_time` (09:00) each collector mails one
digest, tagged `[hub]`, to `collector.digest_recipients` (or
`email.daily_recipients`):

```
[hub] 20 hosts · 14 OK · 5 CAUTION · 1 SILENT · via lhc-c1.example.com
```

It lists, in this order: the collectors, and whether each peer still
reports; the **silent** hosts, with no report for `collector.silent_after`
(26h); every host at CAUTION or worse with its first two alerts, marked
`new` when it was OK in the previous digest; hosts seen for the first time;
and one line for everyone else, with uptimes, pending updates and pending
reboots summed. A host's own `[ACTION]` alerts still go out from the host:
the digest is a summary, never the alert path.

With several collectors each sends its own digest. List the others in
`collector.peers` on each one: when a collector dies, the others' digests
say so in the subject (`· peer lhc-c2.example.com SILENT`). A relay that
refuses the digest is retried every 30 minutes until the next day's.
`lhc hub digest --dry-run` prints the digest as it stands.

## Files

| | root | user |
|---|---|---|
| config | `/etc/lhc/config.yaml` | `~/.config/lhc/config.yaml` |
| state (baselines, alert history) | `/var/lib/lhc/*.json` | `~/.local/state/lhc/` |
| saved reports (`reports.keep`, default 30) | `/var/lib/lhc/reports/` | `~/.local/state/lhc/reports/` |
| log of scheduled runs | `/var/log/lhc.log` | `~/.local/state/lhc/lhc.log` |
| hub key | `/etc/lhc/hub.key` (`hub.key_file`) | |
| collector: latest report per host | `/var/lib/lhc/hub/<host>.json` | |

## Moving from the Python version

lhc does not read `healthcheck.conf` or the Python state files; it starts its
own baselines on the first run. Translate the few settings you changed with
`lhc config set` (e.g. `[thresholds] disk_caution` is now
`checks.disk.caution`), install lhc, check the relay with `lhc test-mail`
(lhc sends as `lhc@<host>`, the Python version as `healthcheck@<domain>`; set
`smtp.from` if your mail filter only trusts the old sender), and once it has
run for a while remove the Python cron entry (the line marked
`linux-healthcheck-managed`). The installer tells you when that entry is
still present; it never removes it.

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
