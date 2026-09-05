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
  the configuration file. The loop and the boot unit read the declaration the host
  accepted, so an unfinished edit never reaches the host on a timer.
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
| The binary | `/usr/bin/regied` — the shipped units expect it there | the operator |
| The configuration | `/etc/regied/config.yaml` — the default of `regied apply` and `regied render`; `--config` names another | the operator |
| Credentials | files under `/etc/regied/secrets/`, at the paths the configuration names | the operator |
| The two systemd units | `/etc/systemd/system/`, copied from [`dist/systemd/`](dist/systemd/) | the operator |
| The accepted declaration | `/var/lib/regied/accepted/declaration.yaml` | regied, when a submission is accepted |
| The report of the last turn | `/var/lib/regied/turn/report.yaml` | regied, when what it says changes |
| The control socket | `/run/regied/control.sock` | the resident process |
| networkd, dnsmasq, pppd files and the pppd hooks | the distribution's own directories, under regied's name and ownership marker | regied |

regied creates its own directories. The operator supplies the first four rows.

**Credentials.** The configuration cannot hold a secret; it names the path of a file that
does ([ADR 0003](docs/adr/0003-secrets-out-of-configuration.md)). Keep those files owned
by root with mode `0600`, in a directory only root can enter. regied reads a credential on
the turn that needs it and never writes it into the record, a dry-run, a diff or a log
line. The consequence worth using: **the configuration file is safe to keep in version
control**, and keeping it there is the whole of the rollback story below.

### The systemd units

Two units ship in `dist/systemd/`. Until there is a package, copy them into
`/etc/systemd/system/`, reload, and enable both.

```sh
cp dist/systemd/regied-reconcile.service dist/systemd/regied.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now regied-reconcile.service regied.service
```

| Unit | Runs | Role |
|---|---|---|
| `regied-reconcile.service` | `regied reconcile` once at boot, then exits | Puts the firewall and everything else back after a reboot. Fails, visibly, if the turn ended failing |
| `regied.service` | `regied serve`, resident | The reconciliation loop, the confirmation trials, and the control socket |

Both read the accepted declaration and nothing else. The resident unit is ordered after
the boot unit but does not require it: a boot turn that failed is exactly when the loop's
retrying is wanted. There is no timer, and the package ships none. **The loop lives in
`regied.service` alone, which is what makes stopping it one operation**
([ADR 0016](docs/adr/0016-converging-on-the-accepted-declaration.md)).

Two things on the host must be true before the units are enabled.

- **The distribution's `nftables.service` is disabled.** Its unit file starts by flushing
  the entire ruleset, which takes regied's table with it, and a second owner of the
  ruleset is one the loop would fight with every minute. The units are ordered after it
  as a guard against the case where it is enabled anyway; that ordering is not a blessing
  ([ADR 0007](docs/adr/0007-resident-process.md)).
- **networkd is enabled and owns the links.** `/etc/network/interfaces` is empty and
  NetworkManager is not installed ([ADR 0011](docs/adr/0011-target-platform.md)).

```sh
systemctl disable --now nftables.service
systemctl enable --now systemd-networkd.service
```

### Commands

Six verbs. The table says what each one reads, because that is the property the design
rests on: **only `regied apply` reads the configuration file**. The full table, with
flags, is in [`docs/spec/configuration.md`](docs/spec/configuration.md#how-a-declaration-reaches-the-host).

| Command | Reads | Does |
|---|---|---|
| `regied render` | the configuration file | Prints what every backend would be given. Touches no host |
| `regied apply --dry-run` | the file and this host | Prints what an apply would change, and changes nothing |
| `regied apply` | the file and this host | Records the declaration as accepted, then runs one turn toward it |
| `regied apply --confirm <duration>` | the file and this host | Starts a trial with a deadline instead of writing the record. Needs `regied.service` running |
| `regied reconcile` | the record and this host | One turn toward the accepted declaration. Exits non-zero if it ended failing |
| `regied serve` | the record and this host | The loop: a turn every resync interval and on every kernel address change |
| `regied confirm` | the control socket | Makes the trial the accepted declaration. Stops the clock |
| `regied cancel` | the control socket | Drops the trial and converges on the previous declaration now |

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
- **A host with no accepted declaration does nothing, and says so.** So does a host
  whose record this version of regied no longer accepts. Neither converges toward empty.

Every turn ends in one of three states, and the state is what to read.

| State | Meaning | Exit status of `apply` / `reconcile` |
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

1. **Prepare the host.** Build (`make build-arm64`), copy the binary to `/usr/bin/regied`,
   write the configuration to `/etc/regied/config.yaml` and the credential files under
   `/etc/regied/secrets/`, meet the prerequisites above, install and enable the two units.
   Until a declaration is accepted, both units report that there is none and change
   nothing. That is expected.
2. **Look before touching anything.** `regied render` prints what each backend would be
   given. `regied apply --dry-run` prints what the host would change, file by file and
   command by command, and does none of it. Credentials are reported as "would be written,
   with this mode", never with their content.
3. **Apply with a deadline.**

   ```sh
   regied apply --confirm 10m
   ```

   This is refused, and says why, if `regied.service` is not running: nobody would be
   holding the clock. The trial is held in the daemon's memory; the record is untouched.
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
7. **Reboot once** before moving clients over. `regied-reconcile.service` should bring
   the firewall and everything else back from the record with no file involved. A trial
   never survives a reboot; only a confirmed declaration does.

A plain `regied apply` during a trial ends the trial and writes its own declaration to
the record, with no deadline left. The command says so. That is choosing to have no net.

## Test

Tests are split by the privileges they need.

| target | command | requires |
|---|---|---|
| unit tests | `make test` | the Go toolchain, nothing else |
| netns integration tests | `make test-netns-docker` | Docker (starts a privileged container) |
| netns integration tests, directly | `make test-netns` | root / CAP_NET_ADMIN and `nft`, `pppd`, `pppoe-server`, `socat` |

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
assembled by hand out of `ip` and `nft`. The contract is in
`docs/adr/0010-netns-testbed.md`.

To put a different implementation under test, write one script that satisfies that
contract and point the variable at it. The tests stay as they are.

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
