# ADR 0004: How a rendered configuration is put on the host

- Status: Accepted (2026-09-03)

## Context

Four backends now turn a validated configuration into text and nothing else: the
systemd-networkd files ([ADR 0012](0012-networkd-rendering.md)), the nftables table
([ADR 0013](0013-nftables-ruleset-shape.md)), pppd's two options files per session
([ADR 0014](0014-pppd-credentials-in-a-second-options-file.md)), and the one dnsmasq
configuration a host's `DHCPServer` and `DNSForwarder` resources add up to. Each of them
is a pure function, each of them leaves writing files, running commands and reclaiming
what an earlier pass left behind to "the apply engine", and none of them says what that
is.

[ADR 0008](0008-delegate-to-existing-implementations.md) closes by naming the question
outright: the apply model has to decide the ordering between reloading networkd and
applying nftables. Three more arrived with the renderers.

- The hairpin half of a port forward matches on the address an uplink is holding, so the
  rendered ruleset goes stale when a session redials with a different one.
- `KeepConfiguration=yes` on the PPPoE link's `.network` is what stops networkd taking
  away the address and default route pppd installed. Whatever order apply uses must not
  put a reload where that guarantee does not yet hold.
- pppd and dnsmasq are processes. Somebody has to start them, notice when they die, and
  restart them, and no ADR has said who.

Two more are properties of this deployment rather than of any backend. The host has
**one uplink and no redundancy**, so apply runs over the line it is reconfiguring. And
`spec.global` — forwarding, reverse-path validation, redirects, SYN cookies — lands on
"the kernel" in [the configuration format](../spec/configuration.md) and has no renderer,
because it is not configuration handed to another implementation. It is a write to
`/proc/sys`, which makes it apply's.

## Decision

### Apply is a function of the configuration and of what only the host knows

The two inputs are the validated configuration and a small set of values that exist only
at apply time: the address a provider's AFTR name resolves to, the contents of a DUID
file, the addresses each uplink link is currently holding, and the two credentials behind
a PPPoE session. Apply collects those, hands them to the renderers, and puts the result
on the host. **Nothing else is an input**, which is what makes applying the same
configuration twice a no-op the second time.

Collecting them is behind interfaces — a filesystem, a command runner, a name resolver, a
reader of link addresses — so that the whole engine is exercised by unit tests that need
neither root nor a network, as `make test` requires.

**A credential is read, used, and dropped.** It is never put in the plan structure that
dry-run and diagnostics walk, so there is no path by which printing a plan can print one
([ADR 0003](0003-secrets-out-of-configuration.md)).

**Whether a file holds one is decided by the directory it is in**, not by whichever part
of the engine happens to be moving it. A credentials file being reclaimed is a credentials
file, and so is one being written; the same is true of the ordering a unit needs. Hanging
these on the directory is what keeps a path from acquiring half its rules when a new one
is added.

**The AFTR name must resolve to an IPv6 address, and a name that does not is an error.**
The tunnel being configured is what carries IPv4, so the resolver has to be reachable over
IPv6 and the answer has to be an IPv6 address; falling back to IPv4 would mean resolving
over the very thing that is not up yet. Missing runtime values are not all fatal, though,
and which is which follows the backend: a DUID that cannot be read stops the rendering,
because networkd would otherwise send a different one and the delegated prefix would
change silently, while an uplink whose address is not known yet is ordinary — the line is
not up, and the rules that depend on it are left out and say so.

*Amended by [ADR 0016](0016-converging-on-the-accepted-declaration.md), which is decided and
not built: with a loop there is another attempt in a minute, so the general answer is that
the artifact depending on a missing value is left out and picked up on a later turn — the
disposition this paragraph gives an uplink address, given to the rest. **What is left out is
the whole artifact, and what a previous turn wrote is left alone**, which is what keeps the
reasoning above about the DUID true: a prefix delegation written without one is not a
smaller version of it.*

### Idempotence is measured per artefact, and "nothing changed" means no command runs

- A file whose rendered content and mode already match what is on disk is not rewritten.
  Rewriting it would move its timestamp, and a timestamp is what several things on a
  host watch.
- The nftables table is replaced in one transaction, so applying the same text is the
  same operation again. It is handed to `nft` when the text differs from what regied
  recorded at its last apply, **or when the table is not in the kernel at all** — which
  is the case after a reboot, and after somebody flushed it. Asking the kernel has three
  answers, not two: present, absent, and *could not be asked* — which is what a machine
  with no nft says, and equally what nft failing says, whatever the reason. Reading the
  third as the second would make a preview taken away from the host claim it would
  install a ruleset the host already has, so it is reported instead
  ([ADR 0006](0006-dry-run-and-rendering.md)) and the ruleset is left alone. The probe
  asks nft to list every table and looks for its own, so that "absent" is only ever
  something nft said with a successful exit. [ADR 0005](0005-apply-rollback.md) depends
  on that: absent is the one answer that lets a rollback delete the table.
- A kernel switch is read before it is written and written only if it differs.
- A reload or a restart runs only when something the process in question reads actually
  changed.

An apply that changes nothing therefore runs no command at all, which is what makes
`regied apply` safe to run from a timer, from a boot unit, and by hand during an outage.

*Amended by [ADR 0016](0016-converging-on-the-accepted-declaration.md), which is decided and
not built: this property is what the reconciliation loop is built out of — a turn of that
loop is exactly what this section describes — but the loop is not built out of a timer. A
timer would run `regied apply`, which reads the configuration file, so an unfinished edit
would reach the host on a schedule; and a second mechanism would mean a second thing to
stop. The statement above stays true, and an operator who wants such a timer can have one,
knowing what it reads.*

### Two stages: stage everything, then commit

Writing a file is not an effect. networkd does not act on one until it is reloaded, pppd
reads its options when it starts, dnsmasq when it is told to, and `nft` never reads a file
by itself. **Running a command is the effect**, so apply is split at that line.

The **staging stage** collects the runtime values, renders all four backends, computes
the plan, writes every file, reclaims what an earlier apply left behind, and makes every
check that can be made without changing anything — including handing the ruleset to
`nft --check`, which parses it and validates it against the kernel without installing it.

The **commit stage** runs the commands, in order.

The point of the split is that everything knowable is found out while a rollback still
costs nothing: a configuration that will not render, a credential that cannot be read, a
name that will not resolve, a ruleset nft refuses. What is left in the commit stage is
the small set of failures that only running the command can reveal.

### The order

| # | Phase | What it does |
|---|---|---|
| 1 | Firewall | `nft -f` the one table regied owns |
| 2 | Kernel switches | forwarding, reverse-path validation, redirects, SYN cookies |
| 3 | networkd | reclaim, then `networkctl reload` |
| 4 | Process configuration | pppd's files, dnsmasq's file, the units — then `systemctl daemon-reload` |
| 5 | Processes | start or restart the sessions, then dnsmasq |
| 6 | Firewall again | only if an uplink address appeared or changed |

*Amended by [ADR 0015](0015-uplink-addresses-in-sets.md), which is built: there is no
phase 6. The ruleset holds no uplink address, so there is nothing to render again. What
the last phase was for happens in phase 1 instead: right after the table goes in, the
uplink sets that do not hold what the links are holding are written — and they are
whether or not the table went in, because that is decided against the kernel and not
against the text, the way everything else in this record is.*

**The firewall goes first because nothing should be able to move a packet before the
rules that filter it exist.** Enabling forwarding and then installing the filter leaves a
window with the opposite property, and on a host being brought up for the first time that
window is the whole of its exposure. Nothing in the ruleset needs a link to exist first:
zones are sets of interface *names*, so a rule naming a link that is not there yet loads
and matches nothing.

**networkd comes before pppd** because the `.network` file for the PPPoE link has to be
in place before the link appears, or networkd meets an unmanaged link and the routes
ADR 0012 put in that file are never installed. Reload, never restart: restarting networkd
takes the links down. `KeepConfiguration=yes` is what makes a reload safe on a link pppd
has already brought up, and the ordering never reloads networkd after a session was
restarted without it being in place first.

**dnsmasq comes last** because it binds to the addresses of the links phase 3 configured.

**A unit is taken away after what runs from it has been stopped.** Writing a file is not
an effect, and reclaiming one usually is not either — but systemctl resolves an instance
through its template, so a template that is already gone makes the stop fail. On a
configuration that took its last session away, that failure would roll the whole apply
back and put the session's configuration back on the host. So the units regied owns are
the one thing not reclaimed while the files are being written: they are reclaimed by a
step at the end of phase 5, after the stop, and systemd is told again afterwards.

**What phase 5 does to a process is decided from two things and nothing else: whether
the configuration declares it, and whether anything it reads was written.** Not from the
kind of change one particular file had. A process the configuration no longer declares
is stopped and disabled if anything of it is still on the host — a unit, an options
file, a credential — whichever it is. A process the configuration declares and nothing
of which was on the host before is enabled and started. One whose unit had to be put
back — somebody deleted it — is enabled and restarted, because what runs from it may
still be running from systemd's copy of the old one, and a start would do nothing.
Otherwise it is restarted if anything it reads was written, and left alone if not. The
rule was first written the other way, one file at a time, and every file that could go
missing on its own became a case that was not there.

### Supervision belongs to systemd

pppd and dnsmasq are long-running processes regied configures. **regied does not fork
them, does not hold them as children, and does not write a restart loop.** It writes a
systemd unit for each and asks systemd to start it.

- One template unit for PPPoE sessions, instantiated per session, so that a session's
  name is the instance name and `systemctl status` answers in the vocabulary of the
  configuration.
- One unit for regied's own dnsmasq. It is **not** the distribution's `dnsmasq.service`:
  that one reads a file regied did not write and belongs to whoever installed it, and
  ADR 0009 rules out taking it over.
- Redialling is pppd's `persist` and the LCP echoes beside it, which ADR 0014 already
  put in the peer file. Restarting after a crash is systemd's `Restart=`. regied declares
  both and then stays out of the way.

Three details of those units follow from decisions already made and are easy to get
wrong in the other direction.

- **What counts as a session's own configuration** — the rule
  [ADR 0005](0005-apply-rollback.md) rests on — is its two options files and the template
  it runs from, and nothing else. Some other unit being written is not a reason to
  restart a line.
- **The session unit is ordered after `systemd-networkd.service`.** The Ethernet it dials
  over is networkd's to configure, which is what phase 3 being ahead of phase 5 says. The
  obvious alternative, `network-pre.target`, is reached *before* networkd has configured
  anything and would order the session ahead of its own underlay.
- **regied's dnsmasq unit offers no reload**, and a changed configuration restarts it.
  dnsmasq re-reads `/etc/hosts`, its lease file and `resolv.conf` on `SIGHUP`; it does not
  re-read its configuration file. A unit that declared a reload would let systemd choose
  the reload, and the configuration just written would sit there unapplied.

This is ADR 0008 applied one layer up: process supervision is a solved problem on this
platform. It also means the state API can ask systemd whether something is running rather
than track it, and that a restart of regied does not disturb a session.

The units are generated in the apply engine rather than in a renderer, because what they
say is how the processes are run, which is this record's subject and not a backend's
configuration.

### The nftables ruleset is re-applied at boot by regied, not by a file

A ruleset is kernel state, not a file, so something has to install it after every boot.
The distribution's answer is `/etc/nftables.conf` and `nftables.service`. regied does not
use it.

- That file holds the whole host's ruleset, not one table, and regied did not create it.
  Writing into it is exactly the case ADR 0009 rules out.
- It would be stale in the one place that matters. The hairpin rules carry the address an
  uplink was holding when the file was written, and after a reboot the line has not been
  dialled yet, so the address in the file is the previous boot's. *(Amended by
  [ADR 0015](0015-uplink-addresses-in-sets.md): the ruleset carries no address, so this
  reason is gone. The two around it are not, and they are enough on their own.)*
- It would run before regied and before the links exist, so nothing would ever correct
  it.

*Amended by [ADR 0016](0016-converging-on-the-accepted-declaration.md), which is decided and
not built: what runs at start is a turn over the record of the declaration the host
accepted, not an apply over the configuration file. The argument below is unchanged — the
ruleset is kernel state and somebody has to install it after every boot — and what changes
is only the input, so that an edit left unfinished in the file does not become the host's
configuration at the next reboot.*

**regied applies at start instead.** The apply is idempotent, so on a boot where nothing
changed it writes no file and runs one command — the one that installs a table the kernel
does not have. The unit that runs it belongs to the daemon rather than to this record;
what this record fixes is that it runs `apply`, and that forwarding stays off until it
has, because the switch that enables it is one of the things apply owns and phase 1 is
ahead of phase 2.

### A changed uplink address re-runs the firewall phase and nothing else

*Amended by [ADR 0015](0015-uplink-addresses-in-sets.md), which is built: the ruleset
carries no uplink address, so there is nothing to re-render. Phase 6 became the seeding of
the uplink sets in phase 1, and the rule below about when the re-render must not run went
with it — a link read mid-redial can no longer take anything away, because nothing in the
text depends on what it answers. The rest of this record stands either way. **What this
section describes is what an apply did before that record.***

Of everything regied renders, **only the nftables table depends on the address an uplink
is holding**. When a session redials with a different one, the hairpin rules are the one
thing that has gone stale.

So the answer is not to re-apply. It is to re-render the table with the new address and
hand it to `nft` — one transaction, no reload, no process touched, nothing else on the
host disturbed. Phase 6 above is that step inside a normal apply, for an address that
appeared or changed while the apply was running. It is not how a cold start gets its
hairpin rules: a session phase 5 started has not dialled by the time phase 6 reads its
link, so on a host coming up the rules that depend on the address wait for the daemon,
or for the next apply. The apply says so.

**The re-render only ever adds.** Phase 5 may have restarted a session, and a link read a
few milliseconds after that answers that it is not there. Rendering *that* produces a
ruleset with the hairpin rules missing, and installing it would take a working port
forward away and record the result as what is in effect — the exact opposite of what this
phase is for. So an uplink that held an address when the apply started and answers none
now stops the re-render: the ruleset already installed stays, it still carries the address
that uplink had, and the apply says why it was left alone.

Waiting for the session to come back was the alternative and it is rejected. How long a
redial takes is the provider's business, and an apply that blocks on it is an apply that
can hang while holding a half-configured host. Leaving the working ruleset in place costs
nothing until the address actually changes, and noticing that it did is the daemon's job
anyway.

**What notices a redial outside an apply is the daemon**, and its unit of work is where
that lives. This record fixes what it must call and what it must not: re-render, compare
with the text last applied, and if it differs apply the table alone. Running `regied
apply` by hand has the same effect, because every apply collects the runtime values
afresh.

## Consequences

- The engine is testable without root, without a network, and without any of nft,
  networkctl, systemctl or pppd being installed, which is what keeps `make test` pure.
- Nothing depends on the output format of `nft list`. The comparison is between the text
  regied rendered and the text it recorded having applied, both of which are its own.
  The cost is that a table somebody edited by hand is not noticed until the next time the
  rendering changes or the table goes missing.
- One small piece of state survives an apply: the ruleset text last installed. It is the
  only thing regied has to remember, because for files the previous generation is the
  file itself.
- **A link that stops being declared is not taken down.** Reclaiming its file makes
  networkd stop managing it; the address and routes already in the kernel stay until the
  link goes down. Removing them would mean regied deleting kernel state it can no longer
  describe, which is the direction ADR 0009 refuses. An operator removing a link from the
  configuration should expect to reboot or take the link down.
- `spec.global` acquires a backend here, and it is the one place regied writes to
  `/proc/sys`. Values are read before being written, which is also what lets a failed
  apply put them back ([ADR 0005](0005-apply-rollback.md)). Reverse-path validation is
  written to every link the configuration names as well as to `all` and `default`,
  because the kernel filters by the larger of `all` and the link's own value: turning it
  off on `all` alone leaves it on for every link that was up before the apply. A link
  that is not there yet has no key to write; it takes `default` when it appears.
- Two more things become expressible for the state API: which phases ran on the last
  apply, and what each of them changed.
