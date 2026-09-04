# ADR 0013: One nftables table, and the order the chains run in

- Status: Accepted (2026-09-02)

## Context

[ADR 0008](0008-delegate-to-existing-implementations.md) leaves regied the firewall, NAT,
and the *matching* half of policy routing. [ADR 0009](0009-ownership-boundary.md) says
regied rebuilds only its own tables and never flushes the ruleset, because on a host that
is not the router it arrives somewhere that already has a great deal of nftables state.
[The configuration format](../spec/configuration.md) adds one constraint that decides the
layout: the chain that sets a firewall mark runs **after** nat prerouting.

`hack/netns/router/reference.sh` is a known-good ruleset for one hardcoded configuration.
It uses four tables — `ip pbr`, `ip nat`, `inet filter`, `inet mangle` — marks before
translation, and copies the LAN route into every policy routing table so that a
hairpinned packet does not follow the default route out. It is the shape to learn from,
not the shape to generalise: it names its interfaces, and it was written knowing there
was exactly one uplink pair.

What has to be decided is how many tables regied owns, which chains hang off which hooks,
and where the mark is set relative to translation.

## Decision

### One table, `inet regied`

Everything regied emits lives in a single table in the `inet` family. Its name says whose
it is, so an operator reading `nft list ruleset` can tell what to leave alone.

- **One table is one transaction.** `nft -f` replaces it atomically: the file adds the
  table, deletes it, and declares the new one, so an apply never leaves the host with
  half a firewall and a re-apply of the same configuration is the same operation again.
  Splitting filter from NAT would give two units that can succeed separately.
- **`inet` covers both families.** Zones, policies and rules may name a family or leave
  it out, and one table can express both. Separate `ip` and `ip6` tables would mean
  emitting every family-less rule twice and keeping the two in step.
- NAT chains have been allowed in `inet` since Linux 4.18, well below the floor
  [ADR 0011](0011-target-platform.md) sets.

### Six base chains, and one regular chain per policy

| Chain | Hook | Priority | What is in it |
|---|---|---|---|
| `prerouting_mark` | prerouting | `filter` (0) | The `EgressRoutePolicy` matches, each setting a mark |
| `prerouting_nat` | prerouting | `dstnat` (-100) | Port forwards, and their hairpin twins |
| `postrouting_nat` | postrouting | `srcnat` (100) | The hairpin source translation, then each `SourceNAT` |
| `forward_mss` | forward | `mangle` (-150) | MSS clamping |
| `input` | input | `filter` (0) | Dispatch to the policies whose `to` is `self` |
| `forward` | forward | `filter` (0) | Port forward openings, then dispatch by zone pair |

A `FirewallPolicy` becomes a regular chain jumped to from `input` or `forward` by
interface set. Zones and address sets become named sets. Nothing in the type system knows
the words `wan` or `lan`.

### The mark is set after translation, not before

`prerouting_mark` sits at priority `filter`, which is 100 after `dstnat`. By the time a
policy's `excludeDestinations` is considered, a packet a port forward readdressed is
already carrying the address of the host inside, so it matches the local exclusion the
operator wrote anyway and stays local. **Hairpin falls out of the ordering** rather than
out of a rule, and no uplink's global address has to appear in the routing tables.

The reference marks first and pays for it by copying the LAN route into every policy
table. That works for one hardcoded pair of uplinks; generated from a configuration it
would mean every table carrying a copy of every locally reachable prefix.

### The filter chains exist only for a host that declared a firewall

A configuration with no `FirewallPolicy` gets no `input` and no `forward` chain. Once one
policy exists, both are `policy drop` and a pair nobody wrote down is dropped, which is
what the schema says. The threshold is what ADR 0009 asks for: a host that declared no
firewall should not acquire a default-drop one and fall off the network.

`input` opens with an unconditional accept for the loopback interface. It is not a policy
and there is no field for it: a host that cannot talk to itself is broken in a way no
configuration intends.

### `masquerade` carries no flags

`random` and `fully-random` spread one host's connections over different external ports,
which is what stops a NAT mapping being endpoint-independent. Everything behind the NAT
that has to be reachable — anything doing its own hole punching — needs it to be. Plain
`masquerade` keeps the source port where it can, so the mapping a host gets does not
depend on who it is talking to.

### Hairpin is identified by state, not by knowing the LAN

The hairpin source translation matches `ct status dnat` and an input interface that is
**not** the uplink. That identifies exactly the connections a port forward readdressed
which did not arrive from outside, without the LAN's prefix or the router's own address
being written anywhere; `masquerade` then takes the source address from the link the
packet leaves by. Traffic from a client outside keeps its address, which is what the
target should go on seeing.

### What only the running host knows arrives as an argument

*To be amended by [ADR 0015](0015-uplink-addresses-in-sets.md), which is decided and not
yet built: the uplink address is to stop being an argument to the renderer. The hairpin
rules will match on a named set per uplink and family, and the address will reach the set
at run time. **Until that is built, this section describes what the code does.***

Rendering reads no kernel, runs no command, and looks at no interface. The one value it
needs that a configuration cannot hold — the address an uplink is holding, which the
hairpin translation has to match on — is passed in. An uplink that is not up yet is not
an error: what depends on the address is left out, and a comment where it would have been
says so. The apply engine fills the argument in; a test fills it in with a documentation
address.

## Consequences

- The apply engine gets one table to replace and one file to hand to `nft -f`. Its
  idempotence rests on the ruleset being a function of the configuration alone, which is
  what makes the renderer pure and its output ordered by name and priority rather than by
  the order the document happens to list resources in.
- Rules carry `counter` and an nftables `comment` holding the name from the
  configuration, so `nft list ruleset` answers "did anything hit this" in the vocabulary
  the operator wrote.
- Resource names become nftables identifiers behind a per-kind prefix. A name nftables
  cannot read back is a render error rather than text that would parse as something else.
- **A port forward's reply is routed by source range, not by the uplink it arrived on.**
  The reply from a published host is marked by whichever `EgressRoutePolicy` covers that
  host's address, so on a host whose port forward is on one uplink and whose target sits
  in a range routed to another, the reply leaves by the wrong one. Fixing it means
  remembering the ingress uplink on the connection and restoring it on the reply, which
  needs a mark per uplink where the schema derives one per policy — several policies may
  name one uplink, and nothing says which mark such a connection should carry. That is a
  decision about `EgressRoutePolicy`, not about rendering, so it is recorded here and
  left to the schema. `config/example.yaml` is one of the configurations affected.
