# ADR 0026: regied does not fail over between uplinks

- Status: Accepted (2026-10-23)
- Keeps [`docs/scope.md`](../scope.md) as it is, and adds one sentence to it when accepted

## Context

A host with two uplinks — the worked example has a PPPoE session and a DS-Lite tunnel —
sends its LAN's IPv4 out of one of them by an `EgressRoutePolicy`. When the tunnel stops
carrying traffic, the traffic it was given is lost, though the other uplink is up and could
carry it. The request is that the host switch to the other uplink on its own.

Two things about the current design are worth stating, because they are easy to assume
otherwise.

- **There is no failover now, not even an accidental one.** A policy's table holds the
  default route through the uplink it names and nothing else. The main table may well hold
  default routes through both uplinks with different metrics, but the traffic a policy marks
  never consults it.
- **The kernel cannot tell that a DS-Lite tunnel is broken.** An `ip6tnl` link stays up, and
  its routes stay installed, when its local address is gone or the AFTR stops answering. A
  second, worse route in the policy's table would therefore never be used: nothing ever
  withdraws the first.

So any failover has to *observe* that the tunnel does not carry traffic — probe something
through it — and then change routing. [`docs/scope.md`](../scope.md) says regied does no
failover; [ADR 0016](0016-converging-on-the-accepted-declaration.md) says a turn sees
drift, not health, and that a reachability watchdog "is a real thing to want, it is a
different record, and it has to answer the questions above before it can be written": how
long is too long, what counts as reachable, and what to do, on a host with one line, when
the answer is "not".

This is that record, and it answers the question by declining.

## Decision

**regied does not switch traffic between uplinks on its own.** The scope stays as it is.
Detection lives outside regied; switching is a person submitting a declaration.

| Part | Where it lives |
|---|---|
| Noticing that an uplink does not carry traffic | A monitor outside regied: a timer on the host, or anything that already watches it, probing through each uplink and notifying |
| Deciding to switch | A person, notified |
| Switching | Submitting a declaration whose `EgressRoutePolicy` names the other uplink, with `regied apply --confirm`, and confirming once traffic flows |
| Switching back | Submitting the original declaration, the same way |

The alternate declaration is a file in the same repository as the usual one, differing in
one `egressRef`. Keeping it there, beside the original and tested with it, is what makes the
switch a one-command act during an outage rather than an edit.

### Why not inside

- **A probe has a policy, and the policy is the deployment's.** What to probe, through which
  uplink, how often, how many failures, how long to wait before switching back: each answer
  is right for one line and wrong for another, and every one of them would become a field.
  [ADR 0002](0002-configuration-schema.md)'s caution about interfaces added before anybody
  needs them applies with force: this one would be hard to withdraw.
- **It is a second source of truth for routing.** A turn renders the policy's table from the
  declaration. A failover inside regied would have to render something else whenever the
  probe said so, and then the declaration no longer says where the LAN's traffic goes; the
  probe does. Outside regied, routing changed behind the loop's back is drift, and the loop
  takes it back within a resync — which is why a monitor must never change routing itself
  while the daemon runs.
- **The case that prompted it has gone away.** The outage that raised this was a tunnel
  whose local address the host no longer held. [ADR 0022](0022-dslite-local-address-follows-the-delegation.md)
  makes the tunnel follow the delegation, and
  [ADR 0025](0025-resolicit-after-the-pppoe-session-returns.md) makes the delegation follow
  the provider. What is left is the provider's side of the tunnel failing, which is rarer
  and is the case where a person deciding is least costly.
- **The person is the safer actor here.** Switching sends the LAN out of a different
  provider path, with different addressing seen from outside. On a host reached over the
  line it reconfigures, that is a submission, and submissions have `--confirm`.

### What would reopen it

A record that supersedes this one has to answer ADR 0016's three questions in the
declaration's terms: what reachability means and how it is probed, what it costs a line that
is up, and what happens on a host with no daemon running. A monitor that has run outside
regied for long enough to show which answers hold is the evidence it would be written from.

### The sentence `docs/scope.md` gains

Under "Multi-node, high availability, and cloud integration", after "no failover": a host
with two uplinks does not move traffic from one to the other when one stops carrying it;
detecting that belongs to monitoring, and switching is submitting a declaration that names
the other uplink — this record.

## Alternatives considered

**A fallback route with a higher metric in the policy's table.** Free, and useless: the
tunnel's route is never withdrawn, so the fallback is never used.

**An `EgressRoutePolicy` field naming a standby uplink, with a probe.** The shape a failover
inside regied would take. Rejected for now, for the reasons above; it is what the
superseding record would add.

**A monitor that rewrites routing itself.** It fights the loop, which puts the policy's
table back on the next turn, and it fights it silently. If a deployment wants unattended
switching before regied has a record for it, the monitor should submit the alternate
declaration, so the loop is converging on what the monitor chose — and the deployment owns
that choice and its risk.

**Stopping regied and switching by hand.** It works, and it is the stop lever's purpose
([ADR 0016](0016-converging-on-the-accepted-declaration.md)). Submitting the alternate file
is better because the loop keeps everything else in place meanwhile.

## Consequences

- **`docs/scope.md` keeps "no failover"**, with a sentence saying what that means for a host
  with two uplinks.
- **A deployment with two uplinks keeps an alternate declaration**, and its monitoring
  notifies a person who submits it.
- **regied gains no probe and no field.** Nothing in this record is built.

## Implementation units

None in regied beyond the sentence in `docs/scope.md` (English and Japanese), which lands
with the acceptance of this record. The monitor and the alternate declaration belong to the
deployment.
