# ADR 0008: Delegate to implementations that already exist

- Status: Accepted (2026-08-23)

## Context

"Manage one host's network state declaratively" also describes tools that already exist:
netplan, NetworkManager, nmstate, and systemd-networkd. Checking on Ubuntu 26.04, several
of the seven areas we target turned out to be things networkd already models
declaratively.

| Area | systemd-networkd |
|---|---|
| DS-Lite (ipip6) | Yes. `Mode=ipip6`, and `Local=` accepts `dhcp_pd` |
| Policy routing | Yes. `RoutingPolicyRule` has `FirewallMark=` |
| Static routes (v4 / v6, with table selection) | Yes |
| DHCPv6-PD | Yes. `SubnetId=` and `Token=` |
| RA / SLAAC advertisement | Yes. `IPv6SendRA` |
| NAT | `IPMasquerade` only. No port forwarding, no hairpin |
| nftables firewall | No |
| dnsmasq (DHCP server, conditional DNS) | No |
| PPPoE | **No** |

**Amended 2026-08-31.** The table above was measured on Ubuntu 26.04. The platform is now
Debian 13, whose systemd is 257, and every row holds there except one: `Local=dhcp_pd`
arrived in systemd 258, so a DS-Lite tunnel takes its local address from the underlay
instead ([ADR 0011](0011-target-platform.md)).

The same reasoning applies at other layers. There is no need to implement BGP ourselves:
if the cluster's load balancer later speaks BGP, that belongs to FRR, BIRD, or GoBGP.

## Decision

**Own only the layers nobody else owns, and hand generated configuration to the
implementations that already exist.**

What regied owns:

- The nftables firewall model (zones, address sets, exceptions, IPv4 / IPv6)
- NAT (masquerade, port forwarding, hairpin)
- The **matching** half of policy routing — source ranges, destination exclusions, sets —
  expressed as nftables marks
- PPPoE: generating `pppd` configuration and supervising it, because networkd has none
- Generating dnsmasq configuration and supervising it
- The model that ties the seven areas into a single declaration, plus dry-run and diff,
  rollback, and a state API

What regied hands to systemd-networkd:

- Interfaces, addresses, MTU, bridges
- Static routes (v4 / v6, including table selection)
- `RoutingPolicyRule` (`FirewallMark=` → table)
- ip6tnl (DS-Lite)
- DHCPv6-PD, and RA / SLAAC advertisement

Policy routing straddles the boundary on purpose. **The hard part of the match — ranges,
negation, sets — is what nftables is good at, and the wiring from a mark to a routing
table is what networkd already owns.**

## Not going through netplan

netplan is a renderer for networkd, but **its vocabulary is narrower**. It cannot emit
`Local=dhcp_pd` for a tunnel, nor `SubnetId=` / `Token=` under `DHCPPrefixDelegation`.
DS-Lite and prefix delegation — the two things we least want to lose in the port — are
exactly what falls off there.

It would also stack two translation layers under a single uplink, which adds one more
place to read during an outage. netplan's output directory is also touched by cloud-init,
so ownership becomes ambiguous (→ ADR 0009).

So regied writes into `/etc/systemd/network/` directly. networkd searches `/etc` ahead of
`/run`, where netplan renders, so on a router regied's files win.

**On hosts that are not the router, regied does not touch the base network.** netplan
keeps it, and regied owns only its own nftables tables.

## Consequences

- We avoid writing the layer that hurts most when broken: tunnel, route, and prefix
  delegation lifecycles.
- The codebase stays smaller, which is what ADR 0001 is about.
- The apply model has to decide the ordering between reloading networkd and applying
  nftables.
- Generated artifacts need explicit ownership marks (→ ADR 0009).
