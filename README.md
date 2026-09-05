# regied

A daemon that builds one Linux host's network policy from a single declarative YAML file.

The name comes from the French *régie* — the control room. **It does not perform; it cues
the performers.** systemd-networkd owns links and routes, dnsmasq owns DHCP and DNS, pppd
owns PPPoE. regied directs them, owns the layers nobody else does (the nftables firewall,
NAT, and the matching half of policy routing), and ties the whole thing into one
declaration.

A router is one kind of host regied applies to, not the only one.

> **Status: built, not yet deployed.** The engine, the resident loop and the confirmation
> workflow are implemented and pass their tests. regied has not yet run on real hardware
> with a real uplink. The configuration schema is settled in shape; individual field
> names may still move before the first release.

## Language

English is canonical. 日本語版は [README.ja.md](README.ja.md) にあります。

## What it owns, and what it delegates

This split is the point of the project, so it comes first. The reasoning is in
[ADR 0008](docs/adr/0008-delegate-to-existing-implementations.md).

| Area | Owner |
|---|---|
| Interfaces, addresses, MTU, bridges | systemd-networkd |
| Static routes (IPv4 / IPv6, with table selection) | systemd-networkd |
| Routing policy rules (firewall mark → table) | systemd-networkd |
| ip6tnl tunnels (DS-Lite) | systemd-networkd |
| DHCPv6 prefix delegation, RA / SLAAC advertisement | systemd-networkd |
| DHCP server, RA options, conditional DNS forwarding | dnsmasq |
| PPPoE session | pppd |
| **nftables firewall (IPv4 / IPv6)** | **regied** |
| **NAT: masquerade, port forwarding, hairpin** | **regied** |
| **Policy-routing match: source ranges, destination exclusions, sets** | **regied** |
| **Generating and supervising pppd and dnsmasq configuration** | **regied** |
| **One declaration over all of the above: dry-run, a record of what was accepted, a loop that keeps the host at it, and applying with a deadline** | **regied** |

Two consequences of that split are worth stating up front.

**regied owns only what it declared.** It rebuilds its own nftables tables rather than
flushing the ruleset, and it removes only routes it installed. Routes learned by a
routing daemon, and state owned by a container runtime or a CNI, are left alone
([ADR 0009](docs/adr/0009-ownership-boundary.md)).

**Distribution is not regied's job.** regied looks after one node. Getting configuration
files to nodes belongs to something else.

## Target configuration

regied is being built against a working configuration with these seven areas:

- **PPPoE** — the primary uplink
- **DS-Lite** — ipip6 tunnel, IPv4 over IPv6
- **Policy routing** — pick an uplink by source range, with destination exclusions
- **NAT** — masquerade, port forwarding, hairpin
- **nftables firewall** — IPv4 and IPv6, with named address sets
- **DHCP and DNS** — static leases, RA / DHCPv6, conditional forwarding
- **Static routes** — IPv4 and IPv6

There is no HTTP API. The design for a read-only one is recorded and deferred
([ADR 0007](docs/adr/0007-resident-process.md)); what the host is doing is answered by the
report of the last turn and by the journal, described under [Operating](#operating).

The assumed deployment has **exactly one uplink and one machine**. There is no
redundancy, and the operator reaches the host over the line it is reconfiguring. That
assumption drives the safety requirements
([ADR 0016](docs/adr/0016-converging-on-the-accepted-declaration.md)):

- **Apply is idempotent.** A turn that finds nothing to change runs no command.
- **`--dry-run` shows what would change** before anything is touched.
- **Nothing rolls back on its own.** A submission that fails stops at the step that
  failed, leaves the host at whatever prefix of the work got done, and says so. Every
  prefix is a safe state: the firewall goes in before forwarding is enabled. Going back is
  a person applying the previous file.
- **The host converges on what was accepted, not on the file.** Only `regied apply` reads
  the configuration file. The resident process reads the declaration the host accepted,
  so an unfinished edit never reaches the host on a timer.
- **A change can be applied with a deadline.** `regied apply --confirm` reverts to the
  previous declaration unless the operator, still able to reach the host, confirms it.
  This is the one automatic revert in the design, and it exists for lockout.

What regied deliberately does not do is in [docs/scope.md](docs/scope.md).

## Platform

regied is built and run on **Debian 13 (trixie)**. It installs nothing: systemd-networkd,
dnsmasq, pppd and nftables come from the distribution, networkd has to be enabled, and
nothing else may own the router's links. [ADR 0011](docs/adr/0011-target-platform.md)
records the versions that assumes, and the one networkd directive that is not in trixie
yet.

## Configuration

Configuration is a single YAML file listing resources, in the style of Kubernetes custom
resources: `kind: NetworkConfig`, host-wide switches in `spec.global`, and eleven
resource kinds in `spec.resources[]`.

- [`docs/spec/configuration.md`](docs/spec/configuration.md) — the document, references
  between resources, and what lands in which backend
- [`docs/spec/kinds.md`](docs/spec/kinds.md) — the kinds, field by field
- [`config/example.yaml`](config/example.yaml) — a worked example of a two-uplink host

The kinds and the division of responsibility are settled
([ADR 0002](docs/adr/0002-configuration-schema.md)); individual field names may still move
before the first release, which is what `v1alpha1` says.

Two properties of the schema are worth knowing before reading it.

**No field can hold a secret.** Credentials are named by the path of a file that holds
them, so a configuration file can be published or pasted into a bug report without review
([ADR 0003](docs/adr/0003-secrets-out-of-configuration.md)).

**No field accepts an uplink's own global address.** NAT and port forwarding refer to the
uplink resource instead, and the chain that marks traffic for policy routing runs after
destination translation. Together those mean hairpin works without anybody writing down
an address that changes.

## Build

```sh
make build              # build for the host
make build-arm64        # cross-compile for an arm64 SBC
```

The deployment target is an arm64 single-board computer running Debian 13
([ADR 0011](docs/adr/0011-target-platform.md)). Development and testing happen on amd64
Linux. The cross-build is static (`CGO_ENABLED=0`), so the binary copies onto the target
with no runtime dependency.

## Operating

Everything below assumes a host that meets the platform prerequisites: Debian 13 with
`systemd-networkd` enabled and nothing else owning the links
([ADR 0011](docs/adr/0011-target-platform.md)). regied is run as root; every command
below is.

### Where things go

| What | Where | Written by |
|---|---|---|
| The binary | `/usr/bin/regied` — the shipped unit expects it there | the operator |
| The configuration | `/etc/regied/config.yaml` — the default of `regied apply` and `regied render`; `--config` names another | the operator |
| Credentials | files under `/etc/regied/secrets/`, at the paths the configuration names | the operator |
| The systemd unit | `/etc/systemd/system/regied.service`, copied from [`dist/systemd/`](dist/systemd/) | the operator |
| The accepted declaration | `/var/lib/regied/accepted/declaration.yaml` | regied, when a submission is accepted |
| The report of the last turn | `/var/lib/regied/turn/report.yaml` | regied, when what it says changes |
| The control socket | `/run/regied/control.sock`. Root's, with no group: reaching it is the capability of reconfiguring the host | the resident process |
| networkd, dnsmasq, pppd files and the pppd hooks | the distribution's own directories, under regied's name and ownership marker | regied |

regied creates its own directories. The operator supplies the first four rows.

**Credentials.** The configuration cannot hold a secret; it names the path of a file that
does ([ADR 0003](docs/adr/0003-secrets-out-of-configuration.md)). Keep those files owned
by root with mode `0600`, in a directory only root can enter. regied reads a credential on
the turn that needs it and never writes it into the record, a dry-run, a diff or a log
line. The consequence worth using: **the configuration file is safe to keep in version
control**, and keeping it there is the whole of the rollback story below.

### The systemd units

One unit ships in `dist/systemd/`. Until there is a package, copy it into
`/etc/systemd/system/`, reload, and enable it.

```sh
cp dist/systemd/regied.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now regied.service
```

| Unit | Runs | Role |
|---|---|---|
| `regied.service` | `regied serve`, resident | The reconciliation loop, the control socket, the confirmation trials, and the convergence at boot |

It reads the accepted declaration and nothing else. There is no separate boot unit: the
first turn the daemon runs after any start — a boot, an upgrade, a `systemctl restart` —
is what puts the firewall and everything else back from the record. That turn is an
unattended one, so restarting the daemon never restarts a session
([ADR 0017](docs/adr/0017-submission-through-the-resident-process.md)). There is no timer,
and the package ships none. **The loop lives in `regied.service` alone, which is what
makes stopping it one operation**
([ADR 0016](docs/adr/0016-converging-on-the-accepted-declaration.md)).

The unit is `Type=notify`. The daemon hands systemd its state after every turn, so
`systemctl status regied` says `converged`, `waiting` or `failing`, and names the first
thing it is waiting on or failing at, without anyone opening the journal.

**Stop it, never disable it.** Stopping the daemon stops the converging and nothing else,
which is what the outage procedure below relies on. Disabling it also gives up the
convergence at boot, and the session units regied wrote are systemd's to start: on the
next boot they dial with no table in front of them.

Two things on the host must be true before the unit is enabled.

- **The distribution's `nftables.service` is disabled.** Its unit file starts by flushing
  the entire ruleset, which takes regied's table with it, and a second owner of the
  ruleset is one the loop would fight with every minute. The unit is ordered after it
  as a guard against the case where it is enabled anyway; that ordering is not a blessing
  ([ADR 0007](docs/adr/0007-resident-process.md)).
- **networkd is enabled and owns the links.** `/etc/network/interfaces` is empty and
  NetworkManager is not installed ([ADR 0011](docs/adr/0011-target-platform.md)).

```sh
systemctl disable --now nftables.service
systemctl enable --now systemd-networkd.service
```

### Commands

Five verbs. The table says what each one reads, because that is the property the design
rests on: **only `regied apply` reads the configuration file**, and **only the resident
process writes the host**. The full table, with flags, is in
[`docs/spec/configuration.md`](docs/spec/configuration.md#how-a-declaration-reaches-the-host).

| Command | Reads | Does |
|---|---|---|
| `regied render` | the configuration file | Prints what every backend would be given. Touches no host |
| `regied apply --dry-run` | the file and this host | Prints what an apply would change, and changes nothing |
| `regied apply` | the file | Sends the declaration to the resident process, which records it as accepted and runs one turn toward it. Prints where the turn left the host |
| `regied apply --confirm <duration>` | the file | Sends the declaration as a trial with a deadline instead of writing the record |
| `regied serve` | the record and this host | The resident process: a turn every resync interval and on every kernel address change, and the control socket |
| `regied confirm` | the control socket | Makes the trial the accepted declaration. Stops the clock |
| `regied cancel` | the control socket | Drops the trial and converges on the previous declaration now |

`regied apply`, with or without `--confirm`, needs `regied.service` running. On a host
where it is not, the command is refused before anything on the host is read, and says
what to do:

```
regied: the resident process is not running: ...
  start regied.service and try again
```

It does not fall back to running the turn itself; the resident process is the host's only
writer. `render` and `apply --dry-run` need no daemon.

Each verb answers `-h` with its flags.

### What the loop does, and what it does not

The resident process runs a **turn** once a minute (`--resync`), and sooner when the
kernel reports that an address was added or removed on any link. A turn renders every backend from
the accepted declaration, reads what the host holds, and moves only what differs. A turn
that finds nothing differing runs no command.

What an operator needs to know about it:

- **An unattended turn never takes down something that is up.** It writes files, replaces
  regied's nftables table, sets kernel switches and starts declared units that are not
  running. It does not restart a running PPPoE session, even one whose options it has just
  rewritten, and it does not stop something the declaration no longer declares. It reports
  those and waits for a person. The way to ask for them is to run `regied apply` again:
  an apply is idempotent, so re-running it does the remaining steps and nothing else.
- **It sees drift, not health.** A declared unit that is not active is drift. A unit that
  is active is at the declaration, whatever it is doing: a session that is up and passing
  no traffic is not something the loop notices. Keeping a session alive is pppd's own
  `persist` and systemd's `Restart=`, not regied's.
- **It retries under backoff, per target.** A dnsmasq that will not start does not stop
  the table from being repaired. The comparison keeps running while the command is
  slowed, so the report stays current.
- **It never exits because it cannot converge.** It stays up and keeps saying what is
  wrong.
- **Some things converge only on a later turn, by design.** A link that a submission
  creates — the DS-Lite tunnel — does not exist when that submission plans its kernel
  switches, so the reverse-path setting declared for it lands on the resident process's
  first turn after the link appears: the submission reports `waiting`, the next turn
  reports `converged`. A PPPoE link is the other case: networkd sees it before pppd has
  given it an address, so the hook pppd runs on ip-up asks networkd to reconfigure the
  finished link, and the policy route for that uplink appears then.
- **A host with no accepted declaration does nothing, and says so.** So does a host
  whose record this version of regied no longer accepts. Neither converges toward empty.

Every turn ends in one of three states, and the state is what to read.

| State | Meaning | Exit status of `apply` |
|---|---|---|
| `converged` | The host holds the whole declaration | 0 |
| `waiting` | Everything that could be done was done; something is left out for want of a value that only exists at apply time, such as an AFTR name that has not resolved. What it waits on is named | 0 |
| `failing` | Something was tried and did not work, or something differs that this turn was not allowed to fix. What, and why, is named | non-zero |

`waiting` is the ordinary state of a host that has just booted. `failing` that persists
is the one to act on. Both are in the report of the last turn and in the journal.

### Putting it on a host for the first time

The procedure assumes what the project was built against: **the existing router stays in
place, and the new host is brought up beside it.** Recovery from anything below is moving
a cable back. Do not skip the deadline on the strength of that; the deadline is what
covers the case where the cable is somewhere else.

**One thing to check in the declaration before anything else.** A host that declares any
`FirewallPolicy` gets an `input` chain whose policy is drop, and a link that belongs to no
zone has no rule that lets anything in
([ADR 0013](docs/adr/0013-nftables-ruleset-shape.md)). So the interface through which you
reach the host must be in a zone, and that zone must have a policy toward `self` that
admits the management protocol — SSH at least, ICMP if you want to ping it. The first time
the acceptance suite was run against a virtual machine, the declaration named the LAN and
the uplinks and not the management link, and the apply locked the session out exactly as
the ruleset said it would. The deadline below is what covers that; declaring the link is
what avoids it.

1. **Prepare the host.** Build (`make build-arm64`), copy the binary to `/usr/bin/regied`,
   write the configuration to `/etc/regied/config.yaml` and the credential files under
   `/etc/regied/secrets/`, meet the prerequisites above, and install, enable and start
   `regied.service`. The daemon comes first because an `apply` is a submission to it.
   Until a declaration is accepted it reports that there is none and changes nothing,
   and `systemctl status regied` says so. That is expected.
2. **Look before touching anything.** `regied render` prints what each backend would be
   given. `regied apply --dry-run` prints what the host would change, file by file and
   command by command, and does none of it. Credentials are reported as "would be written,
   with this mode", never with their content.
3. **Apply with a deadline.**

   ```sh
   regied apply --confirm 10m
   ```

   Like every `apply`, this is refused, and says why, if `regied.service` is not running;
   for a trial there would also be nobody holding the clock. The trial is held in the
   daemon's memory; the record is untouched.
4. **Prove you can still get in, by the path the change could have broken.** Open a new
   session to the host over the network. Check from a LAN client that it gets an address
   and reaches the outside. Read `/var/lib/regied/turn/report.yaml` and
   `journalctl -u regied`. Spend the window on this; the daemon confirms whatever state the
   turn is in, so *waiting* on an AFTR name does not stop you, but it should be a decision.
5. **Confirm, or let it go.** `regied confirm` writes the trial to the record. `regied
   cancel`, or the deadline passing, converges back to what was there before — a submitted
   turn, allowed to restart sessions. Confirm over the network path, not from a serial
   console: a confirmation that arrives by a path the change cannot affect proves nothing.
6. **Commit the file you confirmed** to version control. That copy is the rollback.
7. **Reboot once** before moving clients over. The first turn of `regied.service` should
   bring the firewall and everything else back from the record with no file involved. A
   trial never survives a reboot; only a confirmed declaration does.

A plain `regied apply` during a trial ends the trial and writes its own declaration to
the record, with no deadline left. The command says so. That is choosing to have no net.

### When something goes wrong

The loop is a repair mechanism. During an incident that is the last thing you want
running while you work, so the procedure starts and ends with the daemon.

**Before touching anything by hand, stop the daemon. Start it when you are done. Stop,
never disable.**

```sh
systemctl stop regied     # the loop stops. Nothing else happens
# ... look, edit nftables by hand, restart a session, whatever the outage needs ...
systemctl start regied    # the loop resumes, and takes the hand edits back
```

Stopping the daemon stops the converging and nothing else: no file is reclaimed, no table
removed, no session stopped, no kernel switch put back. The host keeps running what is on
it, the record and the configuration file are untouched, and your hand edits stay for as
long as the daemon is stopped. **When it starts again, it takes them back, because the
accepted declaration is what the host is supposed to hold.** That is correct. If the hand
edit is the fix, put it in the file and submit it. Hand edits made while the daemon is
running are drift and are gone within a minute, which is the loop doing its job.

Two things follow from the daemon being the only writer. While it is stopped, `regied
apply` is refused, because there is nothing to submit to; start the daemon first. And
the lever is stop, not disable: a disabled unit does not come back at boot, which is
exactly when the host needs a turn, and the session units regied wrote still dial then
with no table in front of them.

If a trial was running when you stopped the daemon, it is gone: the next turn, when the
daemon starts again, converges on the declaration that was accepted before the trial.
That is the safe direction, and it is what to expect.

**Going back is applying the previous file.** There is no rollback command, because there
is nothing to roll back to except a file, and the file is in version control.

```sh
regied apply --config /path/to/the/previous/config.yaml
```

That is an ordinary submission: a submitted turn, allowed to restart sessions, so pick
the moment. What the new declaration already did is not undone by it. A session that was
restarted is a new session, leases dnsmasq handed out stay handed out, connections the
new ruleset dropped stay dropped. Applying the previous file gives you the previous
declaration, not the previous moment. Every `apply` prints the revision it recorded; the
revision is the digest of the file's bytes, so a copy in version control can be matched
to what the host accepted without regied.

**A submission that failed part way.** The record holds the new declaration and the host
holds as much of it as got done, up to the step that failed. The console said which step,
and the report says the same. The loop finishes what it may — writes what is missing,
replaces the table, starts what is declared and not running — and reports what it may
not: restarting a session that is running on old options, stopping what the declaration
dropped. Two ways forward, both ordinary submissions:

- fix the file and `regied apply` it, or
- `regied apply` the previous file.

Re-running the same `regied apply` is also the retry for the half the loop is not allowed
to do on its own: an apply is idempotent, so it does the remaining steps and nothing else.

**An `apply` that was interrupted** — Ctrl-C, an ssh session that dropped, a change that
cut your own path — did not interrupt the turn. The turn belongs to the daemon and runs
to its end; the command says so as it exits. The report and the journal say how it ended,
and the report is what answers whether the record was written.

**Locked out.** If the change went in with `--confirm`, wait: the deadline reverts to the
previous declaration with a submitted turn, sessions and all, and the host comes back.
If it went in without `--confirm`, nothing on the host will undo it. Reach the host by a
path the change cannot affect — a console — and apply the previous file from there, or
put the cable back on the router that was there before. A reboot converges on the record,
so it helps only if the record is the declaration you want; a trial never survives one,
an accepted declaration always does.

**Failing, and what to read.** There is no API. Three places say what is wrong, and they
name the thing rather than the mood.

| Where | What it says |
|---|---|
| `journalctl -u regied` | Every change of state, and each distinct drift when it appears and when it clears, with its backoff. Not every turn: a quiet log is a converged host |
| `/var/lib/regied/turn/report.yaml` | The state the last turn ended in and when it was entered, what it is waiting on or what is failing, the revision, warnings, whether a trial is active and its deadline. Rewritten only when what it says changes, so a daemon restart does not lose it |
| `systemctl status regied` | Whether the daemon is running at all, which is the first question when an `apply` is refused. On its status line, the state the last turn ended in and the first thing it waits on or fails at |

Read the state first. `waiting` names a value that has not arrived yet — a name that has
not resolved, a DUID file that could not be read — and usually resolves itself.
`failing` names the command that did not work or the drift the turn is not allowed to
fix, and is waiting for you. A host that reports it has no accepted declaration has had
nothing submitted to it yet, or has lost its state directory; the answer to both is
`regied apply`. A host whose record this version of regied no longer accepts has been
upgraded past its declaration: it keeps running what it has and asks for a declaration
the new version accepts.

There is no command that asks for a turn without submitting anything, because the loop
runs one every minute unasked, up to the line an unattended turn may not cross. Past that
line — a session to restart on new options, something to stop that the declaration
dropped — is a submission, and the way to ask for it is `regied apply` of the file. When
the file and the record differ and it is the record you want re-run with a person's
authority, apply the record: `/var/lib/regied/accepted/declaration.yaml` is an ordinary
declaration, and pointing `apply --config` at it is an ordinary submission of the same
revision.

## Test

Tests are split by the privileges they need.

| target | command | requires |
|---|---|---|
| unit tests | `make test` | the Go toolchain, nothing else |
| netns integration tests | `make test-netns-docker` | Docker (starts a privileged container) |
| netns integration tests, directly | `make test-netns` | root / CAP_NET_ADMIN and `nft`, `pppd`, `pppoe-server`, `socat` |
| netns integration tests, with regied under test | `make test-netns-regied REGIED_NETNS_MGMT_IF=<interface>` | root on a host with systemd, such as a Debian 13 VM: real systemd-networkd, pppd and nftables |

The integration tests build a pseudo-WAN out of network namespaces — a PPPoE server,
a DS-Lite AFTR, and reachability servers — run a router inside it, and check the
following seven things from the outside.

1. Outbound traffic gets through over PPPoE
2. Outbound traffic gets through over DS-Lite
3. Policy routing picks an uplink per source range
4. A port forward reaches a host inside from outside
5. Hairpin NAT reaches it from inside, against the router's own global address
6. The firewall drops traffic it does not allow
7. NAT mapping is endpoint-independent

They need root and commands that a development environment normally lacks, so they
sit behind the `netns` build tag; `go test ./...` does not pick them up. Use
`make test-netns-docker` in the usual case: it prepares a container with the tooling
and calls `make test-netns` inside it.

Where the tooling is already present, `make test-netns` runs directly. A missing
prerequisite fails the run and names what is missing, because a skip that prints `ok` is
indistinguishable from a pass. A bare `go test -tags netns` skips instead, and
`make test-netns REGIED_NETNS_REQUIRE=` asks the target for the same.

### Swapping the device under test

Everything that assembles the router lives in one script, selected by the
`REGIED_NETNS_ROUTER_SETUP` environment variable. The default is a reference router
assembled by hand out of `ip` and `nft`, and `make test-netns-docker` runs that one. It
stays as the proof that the testbed itself can judge the seven checks
([ADR 0010](docs/adr/0010-netns-testbed.md)), which is what makes a failure against regied
mean something. The contract a setup script has to satisfy is in the same record.

### Testing regied itself

The second setup script puts regied under test, and it needs a host with systemd on it: a
Debian 13 virtual machine is the intended one. It does not run in the container and it
does not run on a development machine, because what it tests is regied driving the real
systemd-networkd, the real pppd units, the real nftables and a real `regied serve`.

```sh
make test-netns-regied REGIED_NETNS_MGMT_IF=<interface>
```

Run it as root. The device under test is the host's own root network namespace
(`REGIED_NETNS_ROUTER_CONTEXT=root`); the pseudo-WAN, the client and the peers stay in
network namespaces, and the seven checks are exactly the same ones, still observed from
outside. The script renders and dry-runs the declaration before applying it, starts
`regied serve` against a test control socket, waits for the host to converge, runs the
checks, and tears everything down: the daemon, the generated units and files, the state
directory, the links, the namespaces, and regied's nftables table.

**The management interface has to be named**, and the target refuses to run without it.
The declaration under test carries firewall policies, and a host with any firewall policy
drops input on every link that is not in a zone — see
[the note under first deployment](#putting-it-on-a-host-for-the-first-time). The script
puts that link in a zone of its own and admits SSH and ICMP to the host, declares nothing
about its addresses or routes so that the platform's own networkd file stays
authoritative, records the management addresses and default route before the apply,
verifies them after it, and probes the management gateway. If any of that fails it
deletes regied's nftables table at once and nothing else. Building the VM is outside the
scope of this README.

### Looking around

`REGIED_NETNS_KEEP=1` leaves the topology up after a run. `make netns-shell` drops
into a shell in the same container, where `hack/netns/topo.sh up` builds it and
`hack/netns/topo.sh status` shows the addresses and routes of each namespace.
`hack/netns/topo.sh down` removes it.

## Documentation

- [`docs/spec/`](docs/spec/) — the configuration format and the resource kinds
- [`docs/scope.md`](docs/scope.md) — what regied does not do, and why
- [`docs/adr/`](docs/adr/) — architecture decision records. Read these before making
  changes; they record decisions that implementations should not quietly reverse.

## Prior art

The configuration model borrows its resource-kind naming and schema idioms from EdgeOS
and from [imksoo/routerd](https://github.com/imksoo/routerd). ADR 0001 records what was
measured and why we ended up building our own instead of adopting either.
