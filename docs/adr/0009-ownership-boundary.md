# ADR 0009: Own only what was declared

- Status: Accepted (2026-08-23)

## Context

regied shares a host with other writers.

- **systemd-networkd and netplan** — per ADR 0008, regied hands configuration to
  networkd. On a general-purpose host, netplan keeps the base network.
- **Routing daemons** — if the cluster's load balancer later speaks BGP, FRR or BIRD will
  install learned routes into the kernel routing table. regied never declared those.
- **kube-proxy, Cilium, Calico, Docker** — bringing regied to a host that is not the
  router means arriving somewhere that already has a large amount of nftables and
  iptables state.

If apply meant "make it match the declaration and delete everything else", it would wipe
out BGP-learned routes on every pass and break cluster networking on a node.

## Decision

**Own only what was declared, and do not touch what was not.**

- For nftables, rebuild **only our own tables**. Never flush the whole ruleset.
- For routing tables, remove only routes we installed. Never remove learned or
  third-party routes.
- Mark generated artifacts with an ownership marker, and only reclaim what carries it.
- Place networkd configuration as files under our own prefix. Never rewrite someone
  else's file.

## Consequences

- Adding BGP later is a matter of adding a `BGPPeer`-shaped resource kind and its
  configuration generation. It does not collide with learned routes. Policy routing can
  point statically at a CIDR that contains the address pool, so it does not need to
  change as virtual addresses come and go — **dynamic policy routing is not required**.
- A non-router host's firewall becomes expressible in regied. Because the base network is
  left alone, the cluster is not disturbed.
- **Distribution is not regied's job.** regied is a daemon that looks after one node; how
  configuration files get to nodes belongs to something else. Blurring this is how a
  single-node daemon slides into being a multi-node manager.
- The firewall model must not bake `wan` and `lan` into the type system as proper nouns.
  Hooks (input / forward / output) and interface sets are the primitives; `wan` and `lan`
  are just names of sets. Otherwise the model cannot travel to a non-router host.
