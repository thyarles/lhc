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
U=https://raw.githubusercontent.com/thyarles/lhc/main/install.sh
(curl -fsSL $U || wget -qO- $U) | bash -s -- \
    --set smtp.host=relay.example.com \
    --set email.daily_recipients=ops@example.com \
    --set email.alert_recipients=team@example.com
```

The `curl || wget` form matters on old hosts: a curl too old for GitHub's
TLS fails at once, and wget takes over. `install.sh` does the same for its
own downloads.

The installer downloads the release archive, verifies its SHA-256, installs
`/usr/local/bin/lhc`, writes `/etc/lhc/config.yaml` and schedules the run:
a **systemd timer** where systemd 229+ is available, otherwise a **crontab**
entry (RHEL 7). Re-running it upgrades in place and keeps the config, state
and reports; on a hub collector it also restarts `lhc-collect`, so the
collector runs the new version too. Check the mail settings straight away with `lhc test-mail`:
it takes seconds, where a first `lhc run` can take minutes on a host whose
package repositories are slow or unreachable. `--help` lists the options (`--time 06:30`, `--every 6h`,
`--no-schedule`, a specific version, ...).

Without root, or to try it first: download `lhc_linux_amd64.tar.gz` from the
[releases](https://github.com/thyarles/lhc/releases), unpack it and run
`./lhc report`. Nothing is installed or changed.

### If the download fails

- **`curl: (35) Peer reports incompatible or unsupported protocol version`**:
  the host's curl is too old for GitHub's TLS. The command above falls back
  to wget by itself.
- **A certificate error from curl or wget** (`certificate verify failed`,
  `unable to locally verify the issuer's authority`): the host's CA bundle is
  out of date. Update it (`yum update ca-certificates`, or the distribution's
  equivalent), or add a current bundle under
  `/etc/pki/ca-trust/source/anchors/` and run `update-ca-trust extract`.
  **Do not** turn the check off (`curl -k`, `wget --no-check-certificate`):
  `install.sh` fetches the binary and its checksums the same way, so the
  SHA-256 check would then prove nothing, for a binary that runs as root.
- **No internet on the host at all**: on a host that has it, download the
  archive and `checksums.txt` from the release, check the archive, copy it
  over, and install from there:

  ```sh
  # on a host with internet access
  V=v0.9.1   # the release you want
  B=https://github.com/thyarles/lhc/releases/download/$V
  curl -fsSLO $B/lhc_linux_amd64.tar.gz && curl -fsSLO $B/checksums.txt
  grep ' lhc_linux_amd64.tar.gz$' checksums.txt | sha256sum -c -
  scp lhc_linux_amd64.tar.gz root@db01.example.com:/tmp/

  # on the host
  tar -xzf /tmp/lhc_linux_amd64.tar.gz -C /tmp lhc
  install -m 0755 /tmp/lhc /usr/local/bin/lhc
  lhc config set smtp.host=relay.example.com email.daily_recipients=ops@example.com
  lhc install
  ```

  With many such hosts, an internal mirror of the archive and
  `checksums.txt` works too: `BASE_URL=https://mirror.example.com/lhc` makes
  `install.sh` download from there.

### Upgrading many hosts

Re-run the same command on each host: the config, state and reports are
kept, so the `--set` options are not needed again, and a hub collector
restarts `lhc-collect` on the new binary by itself. From a host that reaches
the others as root over ssh:

```sh
U=https://raw.githubusercontent.com/thyarles/lhc/main/install.sh
for h in web01.example.com web02.example.com db01.example.com; do
    echo "== $h"
    ssh root@$h "(curl -fsSL $U || wget -qO- $U) | bash 2>&1 | grep -E 'Installed|collector service|ERROR'"
done
```

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
(and the first `lhc config set` or install) writes the example with every
setting commented out: `#port: 25`, its default, under what it is for. The
file holds only what you change. `lhc config set key=value` uncomments that
one line, and every other setting follows the default of the installed
version, so a new or changed default arrives with the upgrade. Unknown keys
are errors, so a typo cannot silently do nothing.

Files written by older releases held every default of the day they were created,
which pinned old defaults after an upgrade. `lhc install`, and therefore
every upgrade with install.sh, comments out each setting that only repeats a
default, today's or an earlier release's, and says which old defaults it
let go:

```
  ✓ checks.processes.zombie_caution: 10 was the old default, now follows the default (2500)
```

A value that was never a default is your choice and stays. The previous file
is kept as `config.yaml.bak`, and `lhc config show --diff` then lists only
your real changes.

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

### Site-specific log patterns

The `logs` check counts today's matches of its built-in patterns (OOM
kills, I/O and filesystem errors, kernel panics, segfaults, machine checks,
SSH probing, USB devices, sudo). `checks.logs.extra_patterns` adds your own,
checked after those:

```yaml
checks:
  logs:
    extra_patterns:
      - label: NFS Timeouts
        pattern: "nfs: server .* not responding"
        source: system
        severity: caution
        min_count: 3
```

- `label`: the row and alert name. It must be unique and must not reuse a
  built-in label. It is part of the alert's identity, so renaming it makes
  an open condition look new once.
- `pattern`: an extended regex (`grep -E`), matched case-insensitively.
- `source`: `system` (the journal, or syslog/messages/kern.log) or `auth`
  (auth.log/secure). Default `system`.
- `severity` (required): `info`, `caution` or `unhealthy`. The row and an
  alert take it once `min_count` matches are seen today; fewer matches show
  as INFO.
- `min_count`: 0 counts only and never escalates. At most 200, because the
  scan keeps the last 200 matches.

Only today's lines count, like the built-in patterns, so a match from last
week does not keep the report yellow. Edit the file by hand for this one
(`lhc config set` cannot write a list of patterns), then run
`lhc config validate`.

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

U=https://raw.githubusercontent.com/thyarles/lhc/main/install.sh

# on each of the two collectors (systemd as root, any version: RHEL 7 too)
(curl -fsSL $U || wget -qO- $U) | bash -s -- --hub-key-file ./hub.key --collector

# on every host, collectors included
(curl -fsSL $U || wget -qO- $U) | bash -s -- --hub-key-file ./hub.key \
    --set hub.collectors=http://192.0.2.10:8461,http://192.0.2.11:8461
```

The `192.0.2.x` addresses are placeholders from the documentation range: put
your collectors' real addresses there. lhc refuses the documentation ranges
(RFC 5737 and RFC 3849) in `hub.collectors`, so a copied example fails at once.

- **Two collectors** are the expected setup. Each host sends to every one,
  and each collector is independent, so losing one loses nothing.
- **Separate networks.** Where VLANs must not talk to each other, give each
  VLAN its own pair of collectors and list only those on its hosts.
- **One way only.** A collector answers `POST /v1/report` (and, from another
  collector, `POST /v1/digest-sent`) with 204 or an empty 400, and 404 to
  anything else. It never sends a host anything to act on.
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

Every day at `collector.digest_time` (09:00) the hub mails one digest,
tagged `[hub]`, to `collector.digest_recipients` (or
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

List the other collectors in `collector.peers` on each one: when a
collector dies, the digest says so in the subject
(`· peer lhc-c2.example.com SILENT`). A relay that refuses the digest is
retried every 30 minutes until the next day's. `lhc hub digest --dry-run`
prints the digest as it stands.

**One digest per hub** (`collector.digest_mode: one`, the default). The
collectors take turns by name: each sorts its own name and its peers', so
they all agree without any per-host setting.

- The first sends at `digest_time`. Once the relay accepted it, it posts an
  encrypted "digest sent" notice to every address in `hub.collectors`, on
  the hub port and under the hub key.
- The second's turn is `collector.takeover_after` (15m) later, the third's
  15 minutes after that. At its turn, and before every retry, a collector
  that holds the day's notice stays quiet. One that holds none sends the
  digest and tells the others in turn.
- The digest's footer says who sent it: `Sent by lhc-c1.example.com ·
  standby lhc-c2.example.com, lhc-c3.example.com`, or after a takeover,
  `Sent by lhc-c2.example.com: no digest from lhc-c1.example.com by 09:15`.
- **It fails toward an extra mail, never toward none.** A lost notice, an
  older lhc on a peer (it answers 404 and sends its own), or peer lists that
  disagree can only mean a second copy.
- "Sent" means the relay accepted the mail. A digest lost after that (a
  spam folder, a mailbox rule) is not resent.
- Each collector's nightly run checks it could send if its turn came: it
  connects to the relay, says hello, logs in when a username is set, and
  hangs up, never sending a message. Its report gets a `Digest Relay` row,
  and the alert `Digest relay unreachable from this collector` at CAUTION
  when that fails. The digest marks that collector `relay unreachable`,
  so a broken standby shows up before it has to take over.
- `lhc hub status` shows the order, this collector's turn, who sent today's
  digest and the last relay check. To see a takeover, stop `lhc-collect` on
  the first collector: the second sends at 09:15.

Give every collector of a hub the same `digest_time` and `takeover_after`.
`collector.digest_mode: each` goes back to one digest per collector.

After an upgrade, the digest shows who was left behind. When the hosts do
not all run the same lhc, "everyone else" ends with a line such as
`lhc 0.10.0 on 45 hosts · older: db01.example.com (0.9.1)`, naming up to
five hosts. A collector whose running `lhc collect` is older than the
installed binary says `running lhc X, installed Y: restart lhc-collect` at
CAUTION. `lhc hub status` has a `VERSION` column for every host.

### OOM kills in the digest

The `logs` check counts OOM **kills**, not log lines (one kill writes about
four), and says what was hit:

- **container limit**: a container reached its own memory limit. That is a
  workload sizing problem, at CAUTION.
- **host**: the whole node ran out of memory, at UNHEALTHY.

The host's report lists each killed process. When the process ran in a
Kubernetes pod, the report names the pod (`namespace/name`, looked up
locally with `crictl`), or gives its UID when `crictl` cannot say. The alert
text stays `OOM Killer: N kill(s) today` whichever pod was hit, so a new pod
name after every deploy does not read as a new alert.

The digest adds up to three lines per host, one per OOM-killed pod:

```
OOM team-a/api-6b7c9d-x2k4p · container app · 3 kills · limit 512Mi · Deployment team-a/api
OOM pod UID 1a2b3c4d-… · 2 kills · pod no longer exists
```

The memory limit and the owning workload come from the cluster API. On
the Kubernetes VLAN, the collectors are control-plane nodes with `kubectl`
and the rke2 kubeconfig, so `collector.kubernetes_lookup` (on by default)
asks the API once per digest, and only when some host reported an OOM-killed
pod.

The lookup only ever runs `kubectl get pods`. lhc's only way to run kubectl
refuses any other verb, impersonation (`--as`), raw API paths and manifest
files. A collector without kubectl or a kubeconfig skips the lookup without
a word. The digest lines then show what the host sent: pod, process, count
and `container limit`. If the API call fails, the digest still goes out,
with `Kubernetes lookup failed: …` in its footer. `lhc hub status` shows
whether the lookup is on, off or unavailable.

The rke2 kubeconfig (`/etc/rancher/rke2/rke2.yaml`) is cluster-admin. To
have RBAC enforce the read-only promise as well, point
`checks.kubernetes.kubeconfig` at a kubeconfig whose role allows only
`get` and `list` on pods (and what the `kubernetes` check reads: nodes,
PVCs, events).

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
