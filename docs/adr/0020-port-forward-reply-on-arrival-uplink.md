# ADR 0020: Put a port forward's reply back on the uplink it arrived on

- Status: Accepted (2026-09-13)
- Amends [ADR 0013](0013-nftables-ruleset-shape.md)

## Context

A `PortForward` names the uplink a connection arrives on. The host inside answers, and
the reply is routed by that host's source address, because routing happens before the
reverse translation turns the source back into the uplink's address. On a host with one
uplink that is the main table and there is nothing to say. On a host with two, an
`EgressRoutePolicy` covering the target's address marks the reply and sends it out
whichever uplink the policy names — which is the uplink the target's own traffic leaves
by, and not necessarily the one the connection arrived on. The translation happens, the
packet reaches the target, the reply leaves by the other uplink, and the connection is
never established. Nothing in the configuration looks wrong.

[ADR 0013](0013-nftables-ruleset-shape.md) recorded this and left it to the schema:
fixing it needs a mark per uplink, where the schema derives one per policy, and several
policies may name one uplink. Until now the way round it has been to keep every forward's
target inside a source range a policy sends out the forward's uplink. That works, and it
is a structure that has to be kept true by hand: a forward and an address range in two
places, agreeing. Adding a forward to a host outside the range produces a service that is
reached and does not answer, and a reachability check from outside cannot see it.
Validation warned about it, which surfaced the coupling without removing it.

The kernel already has the fact that is missing. Connection tracking sees the first
packet of a connection arrive on an uplink, and a **connection mark** can hold that on the
connection rather than on the packet. The mark survives the reverse translation because
it is on the connection, not derived from an address. A reply carrying it can be routed
by it before any policy looks at the source address.

Two shapes were considered.

- **Fold it into `PortForward`.** A forward already names the uplink it is published on.
  Remembering that uplink on every connection the forward readdresses, and restoring it on
  the reply, needs nothing the resource does not already say.
- **A new kind of `EgressRoutePolicy`** that says "reply by the uplink the connection
  arrived on". It is more general, and it can be left out: a host that declared the
  forward and not the policy is exactly as broken as today, with the same two places to
  keep in step.

## Decision

**A `PortForward` puts its replies back on the uplink it is published on, and nothing has
to be declared for it.** The mechanism has three parts.

### A mark and a table per uplink and family

For every pair of uplink and address family that at least one `PortForward` is published
on, regied derives one routing table and one firewall mark. They are allocated by the
same allocator as an `EgressRoutePolicy`'s, after the policies: pinned policy values
first, then the unpinned policies in evaluation order, then the uplinks by family and
name. A pinned value is reserved before anything is allocated, so the two cannot
collide. The uplink's pair cannot be pinned: nothing outside regied should depend on it,
and no field is added for it ([ADR 0002](0002-configuration-schema.md)).

The table is not shared with a policy's table for the same uplink, even though both hold
the same default route. Sharing would tie the reply path to a policy that may be removed,
which is the coupling this record removes; a forward's return path outlives every policy
that happens to name its uplink.

The pair is derived whether or not the host has any `EgressRoutePolicy`. On a host with
one uplink it changes nothing — the table's default route is the main table's — and it
keeps the rule from being conditional on something else in the document.

### Two kinds of rule at the head of the mark chain

`prerouting_mark` runs at priority `filter`, after nat prerouting, as ADR 0013 placed
it. Before any policy's rule it now holds:

1. **One restore rule.** A packet in the reply direction of a connection that carries a
   mark has the connection's mark copied onto it, and the chain returns. Returning is what
   keeps every policy behind it from touching the reply: a policy that covers the whole
   LAN would otherwise mark the reply for its own uplink. The connection's mark takes
   precedence over any source match.

   It matches the reply direction only. The original direction — the packets from
   outside, readdressed to the host inside — must not carry the uplink's mark: the
   uplink's table holds a default route and nothing else, and a packet routed by it
   would be sent back out. ADR 0013 rejected copying the LAN's routes into every table
   for this reason. A reply is addressed to the peer outside, and the uplink's default
   route is the right route for it.

2. **One save rule per uplink and family.** The first packet of a connection that
   arrived on the uplink, in that family, and that a port forward readdressed — `ct
   status dnat` — has the uplink's mark written onto the connection. The DNAT status is
   what limits this to port forwards; a connection to the host itself is not marked. The
   rule does not return: the packet goes on to the policies, where an outside source
   matches nothing, as it does today.

The chain exists once there is a `PortForward`, whether or not there is a policy.

### The table and the rule, on the uplink

The uplink's `.network` file gains a default route in the derived table and a
`[RoutingPolicyRule]` selecting the table by the derived mark, written the same way a
policy's are ([ADR 0012](0012-networkd-rendering.md)). A PPPoE session that has no
static route and no policy but is published on gets a file for this alone.

`sourceValidation` needs no new rule. A reply enters on the LAN link with a LAN source
address, and the main table has a route back to it, which is what strict reverse path
filtering checks.

### What is out of scope

**Connections to the host itself are not covered.** Their replies are generated in the
output hook with the uplink's own address as the source, which is a different situation:
the address is already the uplink's, and whether the main table sends it out the right
link is a question about the host's default route, not about a forward. Should a host
need its own replies pinned, that is a restore rule in an output chain and a decision
of its own.

**Hairpin is unchanged.** A connection from inside to the uplink's address arrives on the
LAN link, so the save rule does not see it and the connection carries no mark. Its reply
matches no restore rule and stays inside, through the policy's local exclusion and the
hairpin source translation, exactly as ADR 0013 and
[ADR 0015](0015-uplink-addresses-in-sets.md) built it.

## Consequences

- **A forward's target no longer has to be inside any policy's range.** The validation
  warning that said so is removed, along with what it was built on: its premise, that a
  reply is routed by the answering host's source address, is no longer true of a reply
  regied handles. `config/example.yaml` says the opposite of what it used to about its
  targets.
- **A target's own outbound traffic is not affected.** The mark is saved on connections
  that arrived on the uplink and on nothing else. A host that was kept inside a
  policy's range only so that its forwards would work can now leave by whichever uplink
  the rest of its segment does, and a host that should leave by the published uplink
  for its own reasons says so in an `EgressRoutePolicy`, as before.
- **A connection that was established before this ruleset went in carries no mark.** Its
  reply is routed the old way, by source address; on a host that relied on a policy to
  keep the reply on the uplink, and that removed the policy in the same change, such a
  connection breaks. An operator making that change should expect it. From then on a
  policy change does not move established connections: the mark is on the connection,
  not on the packet.
- **A declaration written the old way still works.** With a policy still sending the
  target's range out the forward's uplink, the reply is restored first and returns before
  the policy sees it, and the target's own traffic follows the policy. The result is the
  same in both halves, so the policy can be removed later or kept.
- **The numbers move.** An uplink's pair is allocated after the policies, so removing a
  policy renumbers what comes after it, as removing a policy always did. Nothing outside
  regied should depend on the values, and nothing in regied depends on their being
  stable across such an edit.
- **The netns testbed gains a forward whose target is outside the policy's range**, and
  checks that it is reached from outside, that it hairpins, and that the target's own
  traffic still leaves by the other uplink. The reference router carries the same
  mechanism, marking before translation as it does everything, so that the testbed can
  be shown to judge the check ([ADR 0010](0010-netns-testbed.md)).
- ADR 0013's last consequence is amended by this record and carries a note.
