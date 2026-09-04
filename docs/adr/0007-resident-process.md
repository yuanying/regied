# ADR 0007: The resident process answers questions and follows addresses. It does not apply

- Status: Deferred (2026-09-04). **Decided, and then set aside: nothing here is being
  built.** The HTTP API turned out not to be wanted yet, and the work of building it is
  not scheduled. It becomes a decision to act on when a host is found to need something
  resident.

> **There is no resident process on the host, and none is planned for now.** `regied` has
> two commands, `render` and `apply`; there is no `serve`, no listener, and no unit that
> runs anything at boot. The only thing an apply leaves behind is the text of the ruleset
> it installed ([ADR 0004](0004-apply-model.md)), written under regied's state directory
> on success. None of what follows exists: no record of an apply, no endpoints, no socket,
> no units, no address watcher.
>
> **Everything below is therefore a decision about how to build this, not a description of
> anything running.** That includes the two questions that were still open when it was
> written — where it listens, and whether it has any write endpoint at all. They are
> settled at the time it is built, against whatever is true then, and not now.
>
> **The record is kept rather than withdrawn, for two reasons.** The first is that a host
> which does turn out to need something resident should start from a design to argue with
> rather than from nothing. The second is what writing it turned up in the apply engine,
> which is worth not having to find twice: **the apply records the text of the ruleset it
> installed and nothing else.** No revision, no timestamp, no outcome, no trace that an
> apply was rolled back. So nothing on a host can say which declaration is running or
> whether the last attempt succeeded — and that gap is real whether or not anything ever
> serves it over HTTP.
>
> **One decision here does not wait on the rest**: the unit that runs an apply at boot,
> which is needed by any host that installs regied. It is argued for in its own section
> below, and the reasoning there does not depend on the resident process existing.
>
> The record the daemon half rests on, [ADR 0015](0015-uplink-addresses-in-sets.md), is
> itself decided and not built. Read both as what is to be, not what is.

## Context

The task this project came from asks for a read-mostly HTTP API, and names what it must
answer: which declaration is applied and whether the last apply failed, the state of the
PPPoE and DS-Lite uplinks, the DHCP leases, a conntrack summary, and health and
readiness. It also draws a line around it — writes may go as far as reload or apply, and
no further; there is no CRUD over resources, because the declaration in the file is the
source of truth and an API that edits it would make two.

Two earlier records left work here on purpose.

[ADR 0004](0004-apply-model.md) decided that the boot-time nftables install is an apply,
because the ruleset is kernel state and the distribution's `/etc/nftables.conf` is
neither regied's file nor able to hold what regied renders. It then said the unit that
runs that apply "belongs to the daemon's unit of work" and stopped. That unit is decided
here.

[ADR 0015](0015-uplink-addresses-in-sets.md) took the uplink address out of the ruleset
and put it in a per-uplink, per-family set, and named three writers of those sets: the
apply, which seeds from the kernel; the pppd hooks, which cover a redial for IPv4; and
"a daemon, from kernel address events, for every uplink and both families", which it
described as the general mechanism the other two stand in for. That daemon is decided
here, including whether it is this process at all.

Building the apply engine also made one gap sharp. **Everything the status endpoint is
asked for about an apply, the host cannot answer.** The engine records exactly one thing
— the ruleset text — and it records it only on success. There is no revision, no
timestamp, no outcome, no record that an apply was rolled back. An operator reading a
console during an apply sees all of it ([ADR 0006](0006-dry-run-and-rendering.md)); five
minutes later nothing on the host remembers. That is not a limitation of the API. It is
a missing artifact, and deciding its shape is most of this record.

## Decision

### One resident process, one command: `regied serve`

The status API and the address watcher live in the same process. They want the same two
things — what the last apply did, and which links are uplinks — and they are both small.
Splitting them would double what has to be supervised, and would give a host with one
uplink two regied daemons of which one can be dead invisibly.

The read-only half is not a reason to split either. Reading the conntrack counters and
writing an nftables set both need privilege, so an unprivileged API process would have
nothing left to read. The process runs as root; narrowing it to a dedicated user holding
`CAP_NET_ADMIN` is a packaging refinement and not a decision this record needs to make.

**`serve` takes no configuration file.** Its inputs are the state directory and the
socket path. What it needs to know about the declaration, it reads from the record of the
apply — see below — because the set it maintains has to follow the configuration that is
*in effect*, not whatever is in the file at this moment. Someone half-way through editing
the declaration must not move the router's sets. The difference between the file and what
is applied is the question `regied apply --dry-run` exists to answer (ADR 0006).

### The apply writes down what it did, and that record is the interface

An apply gains a second artifact under regied's state directory, beside the recorded
ruleset: **one record of the apply itself.** It is written at the end of every apply,
whatever the outcome — including one that was rolled back — because "the last apply
failed and the host is running what it was running before" is precisely what nothing on
the host can say today.

It holds this and nothing more.

| Field | What it says |
|---|---|
| Revision | The digest of the declaration that was applied |
| Source | The path it was read from |
| Started, finished | When the apply ran |
| Outcome | Applied, nothing to do, rolled back, or stopped in a mixture |
| Failure | For an apply that stopped: the phase, the step, the cause, and what the rollback managed (ADR 0005) |
| Phases | Which of the six phases ran, and what each changed (ADR 0004's last consequence) |
| Notes | What failed after the commit stage had already succeeded (ADR 0005) |
| Warnings | What the renderers and validation said about the declaration (ADR 0006) |
| Uplinks | For each uplink the declaration named: the resource, its link name, and the set for each family |
| Runtime values | The non-secret values the apply collected: the address the AFTR name resolved to, and the DUID in effect |

**Credentials cannot appear in it.** The record is derived from the plan, and a
credential never enters a plan — it is read, used and dropped inside the staging stage
(ADR 0004). The rule ADR 0003 asks for is structural here, exactly as it is for the
dry-run output.

The uplink rows are why `serve` needs no declaration. They are the whole of what the
watcher acts on, they describe the configuration that is actually installed, and putting
them here means the daemon never parses YAML at runtime and the two halves can never
disagree about which links are uplinks.

**Writing the record is bookkeeping, and bookkeeping does not roll anything back.** A
successful apply that cannot write its record is a successful apply with a note, the same
disposition ADR 0005 gave the ruleset record: telling an operator that an apply failed
when the host is running the new configuration is worse than telling them what could not
be written down. A failed apply that cannot write its record has already said everything
on the console and in its exit code; the record simply stays stale, and the endpoint that
serves it says how old it is.

### The revision is the digest of the declaration's bytes, not its path and mtime

An mtime moves when nothing changed — a rewrite of identical content, a checkout, an
rsync — and it does not compare across machines. The engine was made idempotent on
content and not on timestamps (ADR 0004), and the revision has to agree with the engine
about when two configurations are the same one.

A digest also answers the question an operator actually asks in an incident: *is the
router running the file I have in front of me?* They can compute the same digest from any
copy. The path is recorded too, as a separate field, because the same bytes can be
applied from anywhere: the path is provenance, the digest is identity.

The digest covers the declaration document as read, and nothing it points at. A changed
PPPoE password does not change the revision, and **must not** — a digest that moved with
a secret would be a channel for one. The apply still installs the change, because what it
compares for a credentials file is the file it writes, not the revision (ADR 0004). If a
configuration ever arrives as more than one document, the revision is over all of them in
path order.

### The endpoints

| Method | Path | Answers |
|---|---|---|
| GET | `/healthz` | The process is up and serving. It reads nothing |
| GET | `/readyz` | A configuration is in effect on this host |
| GET | `/api/v1/status` | The record of the last apply: revision, source, when, outcome, phases, warnings, and how old the record is |
| GET | `/api/v1/uplinks` | One entry per declared uplink: whether its link is present, the addresses it holds per family, its MTU, what its set holds, and for a DS-Lite tunnel the AFTR name and the address it resolved to |
| GET | `/api/v1/dhcp/leases` | The leases dnsmasq has handed out |
| GET | `/api/v1/conntrack/summary` | The connection-tracking counters, not the table |

`/healthz` and `/readyz` carry no version prefix. They are conventions belonging to
whatever probes them, not to this API.

**`/api/v1/uplinks`, not `/api/v1/wan`.** The appendix to the task sketched the latter.
[ADR 0009](0009-ownership-boundary.md) decided that `wan` and `lan` are not proper nouns
in this system — they are names an operator may give a set of interfaces — and the schema
speaks of uplinks, which is what `egressRef` names. A path that says `wan` would be the
one place the vocabulary breaks, and it would be the place hardest to change later.

**Readiness is about whether a configuration is in effect, not whether the latest attempt
succeeded.** An apply that was rolled back leaves the host running the configuration it
was already running; that host is fine, and a probe that calls it unready would have
whatever watches it react to a bad edit by restarting things on a router with one uplink.
So `/readyz` answers no in two cases only: regied has never successfully applied on this
host, and the last apply stopped in a mixture its rollback could not resolve — because
then nobody knows what is in effect. Whether the *latest* attempt succeeded is
`/api/v1/status`, where it can be read with its reason attached.

**Readiness does not probe.** It does not ask nft whether regied's table is still in the
kernel. A probe costs a process per request, and readiness is polled. The question
"has something drifted since the last apply" is answered by running an apply, which is
idempotent and does nothing when nothing changed, or by `apply --dry-run`, which shows it
without touching anything.

**The conntrack summary is counters, never a walk of the table.** It is what the kernel
already keeps: the current count and the maximum, and the per-CPU statistics beside them
— found, invalid, insert failures, drops, early drops, search restarts. Listing
connections would cost time proportional to the traffic the router is carrying, on an
endpoint whose whole purpose is to be polled, and the answer would be enormous on exactly
the busy router where someone is asking. There is no per-connection endpoint.

**An endpoint that cannot answer part of what it was asked says so in that part.** It
does not fail the request and it does not answer zero. This is ADR 0004's three-valued
probe applied to the API: *present*, *absent*, and *could not ask* are different answers,
and collapsing the third into the second is how a reading taken from a broken source
comes to look like a fact. A link that is not there, a lease file that does not exist yet,
a conntrack counter that could not be read — each is reported as itself. A request for
something that does not exist is still an ordinary 404, and `/readyz` still answers
unready with a failing status, because those are what probes and clients read.

The leases are the one list that grows with the network. It is returned whole. If that
ever stops being reasonable, the answer is a filter on the endpoint, not an envelope
around every endpoint.

### Where each answer comes from

| Answer | Source |
|---|---|
| Health | The process itself |
| Readiness, revision, outcome, failure, phases, warnings | The apply record |
| Which links are uplinks, and which set belongs to each | The apply record |
| Link presence, addresses, MTU | The kernel, over netlink |
| What an uplink's set holds | The kernel, through nft |
| How long a session's process has been up | systemd, for that session's unit |
| How long an address has been held | The watcher's own observation |
| AFTR name and resolved address | The declaration and the apply record |
| DHCP leases | dnsmasq's lease database, under regied's state directory |
| Conntrack counters | `/proc` |

Two of these deserve their reasons written down.

**The lease database is regied's own path, not the distribution's.** regied writes the
dnsmasq configuration and that configuration names where the leases go — under regied's
state directory. The API reads the same path for the same reason the apply wrote it: it
is regied's file. A host where dnsmasq has never handed out a lease has no such file, and
that is an empty list rather than an error, the same disposition the engine gives a
directory nothing has been written to yet.

**Uptime is two questions, and answering them as one lies.** How long pppd has been
running is not how long the session has held this address; a redial replaces the second
without touching the first. So both are reported and neither is called uptime. The
process's side comes from systemd, which is already the supervisor (ADR 0004) and already
knows when the unit last entered its active state. The address's side is known only from
when this process started watching: an address that was already there when the watcher
came up is reported as held, with its age unknown. Inventing an age for it would make the
one number an operator reads during a line problem the one number that is wrong.

### It listens on a unix domain socket, and holds no authentication

The socket goes under `/run`, in regied's own directory, with a mode that lets root and
one group read it.

The API is read-only, so *who may connect* is the whole of its access control, and a
filesystem mode is that, enforced by the kernel, with nothing to write, no token to
store, and nothing to rotate. A socket cannot be reached from the network at all, which
matters more here than usual: the firewall that would have to protect a TCP port is
installed by regied itself, so a listener that depends on it is a listener that depends
on the thing it exists to report on.

What the API returns is not nothing. The lease list and the conntrack summary describe
the hosts on the network behind the router. Binding a port on a LAN address would hand
that view to every device the firewall is there to keep at arm's length, and binding
loopback would hand it to every local process without a permission check — less
protection than the socket, not more.

A human elsewhere reaches it by forwarding the socket over ssh. A metrics collector, when
there is one, reaches it locally. That need — scraping from another machine — is the real
case for a port, and when it is real the answer is a record that decides exposure and
authentication together, not a flag that decides only exposure. This record therefore
does not add one. Adding it later changes no schema and breaks no client.

Socket activation was considered and not used. The process has work of its own to do
whether or not anyone connects — it has to seed and follow the sets — so deferring its
start until the first request would defer the half that matters.

### There are no write endpoints

Not reload, not apply, not a dry-run.

The task allowed a reload as the one candidate, and the reason to decline it is what an
apply is. It is the sharpest thing regied does: it can restart the one uplink and it can
roll back (ADR 0005). Putting it behind an HTTP verb makes reaching the socket equivalent
to running it, which turns the listener's access control from an ergonomic question into
a safety one — and this record just decided that question on the grounds that the API is
read-only.

It also adds no capability. An apply is already available to anyone who can run `regied
apply` on the host, which is anyone who could usefully reach the socket. All an endpoint
would add is a second path to the same effect under different access control, which is a
thing to get wrong rather than a thing to have.

Serving the dry-run is the weaker case still. ADR 0006 decided that the dry-run's output
is written for a human at a console during an incident, and deliberately did not make it
a machine interface, pointing anything that wants structure at this API instead. An
endpoint that returned it would create the machine-readable plan that record declined,
by the back door.

**The resident process never applies.** Not on a signal, not on a timer, not when it
notices drift. Deciding to restart a router's only uplink is an act with a rollback
attached to it, and it belongs to an operator or to a boot unit, not to the judgement of
a process whose job is to answer questions.

### Two units, and regied does not write them

**`regied-apply.service`** runs `regied apply` once at boot and stays marked as done. It
is where ADR 0004's decision lands: the ruleset is kernel state, so somebody must install
it after every reboot, and that somebody is an apply rather than a file, because an apply
is idempotent and installs one table without flushing anybody else's. On a boot where
nothing changed it writes no files and runs one command.

**`regied.service`** is the resident process.

They are ordered after systemd-networkd, because the apply reloads it and pppd's ethernet
is its to configure (ADR 0004), and they are **not** ordered before `network-online.target`
— that target waits for the network to be configured, and this is the unit that
configures it. Ordering the apply against it deadlocks the boot.

The resident process is ordered after the apply and does not require it. When the boot
apply fails is exactly when someone needs to ask what happened, and a daemon that refuses
to start because the apply failed takes the answer away at the moment it is wanted.

If the distribution's `nftables.service` is enabled on the host, the apply is ordered
after it, because that unit's file begins by flushing the entire ruleset and would take
regied's table with it. The ordering makes the surviving state the right one. It is a
guard, not a blessing: a host running regied should not have that unit enabled, in the
same way ADR 0011 asks for `/etc/network/interfaces` to be empty. That is a prerequisite
the operator meets, not something regied enforces.

**Both units are shipped by the package. regied does not render them.**

- The unit that runs the first apply cannot be created by an apply.
- ADR 0009's ownership marker exists so regied can reclaim what it no longer needs.
  regied must never be in a position to reclaim the unit that runs regied.
- What these units say is how regied itself is run, which is packaging. Nothing in a
  declaration changes them. The units regied *does* write are different in exactly that
  way: whether there is a pppd template, and which sessions instantiate it, follows from
  what was declared (ADR 0004).
- They are enabled by the operator, in the same act as installing regied.

They belong in the directory a distribution's own units live in, which leaves
`/etc/systemd/system` to the operator's overrides and to the units regied writes there —
the same separation ADR 0012 made with file name prefixes.

**The boot-time apply unit stands on its own.** Everything argued for it here — that the
ruleset is kernel state somebody has to install after every reboot, that the somebody is
an apply rather than a file, the ordering against networkd and `network-online.target`,
the `nftables.service` hazard, and the package shipping it rather than regied rendering it
— follows from ADR 0004 and holds for any host running regied, whether or not there is
ever a resident process for it to sit beside. A host that installs regied and never serves
anything still needs it.

### The daemon half: following the sets

The watcher subscribes to the kernel's address notifications for both families and keeps
each uplink's set equal to the global addresses that uplink's link is holding. That is
the whole of it. It is what ADR 0015 named as the third writer, and it is the general
form of what the other two do in the cases they cover.

- **Global addresses only.** A link-local must not go into a set. pppd's IPv6 hook is
  handed a link-local and ADR 0015 already noted that the global comes by way of networkd;
  a hairpin rule matching a link-local matches nothing a client outside would have
  resolved, and putting one in makes the set assert something false.
- **It reconciles at startup, not only on events.** Events describe changes, and the
  process may have started after the addresses appeared, or been restarted. So it reads
  every declared uplink's current addresses and makes the sets agree — adding what is
  missing, removing elements no address backs.
- **Reconciling is safe because all three writers compute the same function.** A set's
  contents are exactly the global addresses its uplink's link is holding; the apply seeds
  it from the kernel, the pppd hook adds and removes what pppd hands it, and the watcher
  follows the kernel. None of them needs to know about the others, and any of them running
  is enough to make the answer right.
- **A missing table is ordinary, not fatal.** Before the first apply, or after somebody
  flushed it, there is nothing to add an element to. The watcher reports that in the
  status and keeps watching. It does not install the table; installing tables is applying.
- **It re-reads the apply record when the record changes**, and takes the uplink rows from
  it. That is how a `regied apply` that added or removed an uplink reaches the watcher
  without anyone signalling it, and it is why what reaches the watcher is the configuration
  that was applied rather than the one being edited.

### The host is correct while the daemon is dead

This is the relationship ADR 0015 set up, stated from this side.

The apply seeds every set from the kernel each time it runs, and the pppd hooks add and
remove on the PPPoE links as sessions come and go. Neither needs this process. So a host
whose daemon has never started, or has crashed, keeps the port forwards that were working
and keeps every rule that does not depend on an address that changed since.

What the daemon adds is the cases those two do not reach: the IPv6 global on a PPPoE link,
which arrives through networkd where there is no hook to run; a DS-Lite tunnel's endpoint
address changing under it; and any address change on an ordinary interface used as an
uplink. With the daemon dead, those sets hold what the last apply or hook put there until
the next apply. **A dead daemon degrades the freshness of one set. It does not take the
host down**, and nothing about recovery is more than starting it again — it reconciles
from the kernel on the way up.

### What this does not become

Multi-node management, and any distribution of configuration to other hosts: ADR 0009
already drew that line, and an API that starts answering for more than one host is the
slide across it. A web console. CRUD over resources. A metrics endpoint, until the record
that decides exposure exists. A machine-readable rendering of the dry-run. A
per-connection view of conntrack. Streaming of logs, which is `journalctl`'s. A daemon
that applies.

**That line is about hosts, not about sources.** What ADR 0009 refuses is regied managing
another machine or shipping configuration to it. It does not refuse regied learning from
somewhere else what *this* host should be doing — that record already puts regied beside a
cluster's own writers, next to a BGP daemon installing learned routes and on a host where
kube-proxy or Cilium already holds nftables state. Reading another system's API to find
out what this host should do is on the near side of the line; telling another host what to
do is on the far side.

The reason is the one ADR 0002 gives for not adding kinds ahead of need. An endpoint is a
promise about an interface, and the ones that are hard to withdraw are the ones added
before anybody needed them.

## Consequences

- **The engine gains one artifact and one rule.** The record is written at the end of
  every apply, and its failure is a note rather than a rollback. Everything else about the
  apply model is unchanged.
- **The status API can only answer for applies that went through the engine.** A ruleset
  someone installed by hand, a file someone edited, a session someone restarted: invisible.
  That is the same limit ADR 0004 already accepted when it decided to compare regied's own
  rendered text against regied's own record rather than parse `nft list` output.
- **The resident process is unit-testable without root or a network.** The record is a
  file, the lease database is a file, the conntrack counters are files, and netlink,
  systemd and nft sit behind interfaces the way the engine's host does — so `make test`
  stays pure, and the netns testbed exercises the real netlink half where a real kernel is
  already required (ADR 0010).
- **The record is regied's own file under regied's own directory**, so ADR 0009's
  ownership markers do not apply to it. Those exist for directories regied shares with
  other writers.
- **Two units means a host can have one enabled and not the other, and both states are
  legible.** Apply without daemon: the host is configured, the sets go stale between
  applies, and nothing answers questions. Daemon without apply: questions are answered and
  the sets are followed, but nothing installs the table after a reboot, so there is nothing
  to follow it into until someone applies.
- **The API's answers overlap the console's output without being the same text.** ADR 0006
  keeps the console for a human during an incident and points structure here; this record
  points the machine-readable plan back at that decision by not serving one.
- **A revision is comparable off the host.** Anyone with a copy of the declaration can
  compute the digest and compare it to what `/api/v1/status` reports, without regied and
  without ssh, which is what makes the field worth having in a fleet of one.
- **The shape generalises past uplink addresses.** ADR 0015 chose it for one kind of
  runtime state — the address a line is holding — but nothing in the shape is about
  addresses. The rendered text stays a function of the configuration alone, the resident
  process writes the elements, and the apply neither depends on them nor waits for them.
  Runtime state arriving from somewhere else entirely — the healthy backends behind a
  load-balanced address a Kubernetes cluster asked for, to name the case that prompted
  this paragraph — would sit in the same place, be written by the same half of the same
  process, and leave the rendered text alone. This is a property inherited from ADR 0015,
  not a plan made here: **no endpoint, no kind and no field is promised**, and one is added
  when something needs it and not before (ADR 0002).
- **What makes the uplink sets safe does not hold for runtime state in general.** ADR 0013
  replaces regied's table in one transaction, and a replacement empties every set in it.
  That costs an uplink's set nothing, because the apply re-seeds it from the kernel on the
  way past (ADR 0015): the kernel holds the answer, so any writer can recompute it at any
  time. An element whose source is not the kernel has no such recovery. The apply cannot
  re-seed what it cannot ask for, so every replacement of the table would open a window
  that stays dark until whatever does know the answer notices and writes it again. This
  record neither solves that nor needs to — it can only concern elements that do not exist
  yet, and solving it belongs to the record that adds them. It is named here so that the
  condition is explicit: the uplink sets are safe *because the kernel holds their answer*,
  and reading them as evidence that the problem is handled in general would be reading
  them wrongly.
