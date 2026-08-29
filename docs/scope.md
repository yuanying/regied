# Scope: what regied does not do

regied stays small on purpose ([ADR 0001](adr/0001-why-build-our-own.md)). Everything
listed here is a deliberate omission, not a gap waiting to be filled. If one of these
turns out to be necessary, it should arrive as a new decision record, not as a quiet
addition.

## Not implemented

### UPnP IGD and NAT-PMP

Dynamic port mapping requested by LAN clients is not supported.

The usual reason to want it is game consoles reporting a restrictive NAT type. What
actually matters there is the NAT mapping behavior — whether the external port stays the
same when the destination changes — and Linux `conntrack` NAT is endpoint-independent by
default. A plain nftables masquerade already gives the more permissive of the two common
NAT classifications, and most titles connect on that.

Where UPnP helps is one further step beyond that, which is a small return for a daemon
that accepts unauthenticated mapping requests from the LAN. When a specific host does
need a different path, the answer is to add its address range to the policy-routing
configuration, which is a one-line change.

If it is genuinely required later, `miniupnpd` runs as a separate process. There is no
reason for it to live inside regied.

### VPN termination

No IPsec, L2TP, WireGuard, or Tailscale. regied does not terminate tunnels other than the
IPv4-over-IPv6 tunnel it needs for the uplink itself.

A configuration that opens firewall holes for a VPN without anything terminating it is
worse than having neither, so the holes are not modeled either.

### Dynamic routing

No BGP, OSPF, or RIP. If dynamic routing is needed, it belongs to FRR, BIRD, or GoBGP
([ADR 0008](adr/0008-delegate-to-existing-implementations.md)). regied's job in that case
is to not fight with it: it removes only routes it installed, so learned routes survive
([ADR 0009](adr/0009-ownership-boundary.md)).

### Overlay networking

No VXLAN, no VRF. These belong to a different class of deployment than "one host, one
uplink".

### Multi-node, high availability, and cloud integration

regied is a daemon that looks after **one** node. It has no clustering, no leader
election, no failover, and no integration with a cloud provider's API.

**Distribution is explicitly out of scope.** How a configuration file reaches a node is
someone else's problem — a configuration management tool, a Git-based delivery pipeline,
an image build. Blurring this is the path by which a single-node daemon becomes a
multi-node manager.

### A web console

There is no GUI, and there is no write API for resources. Configuration has exactly one
source of truth: the YAML file. The HTTP API is read-mostly — apply state, link state,
leases, connection tracking summary, health — and any write endpoint stays at the level
of "reload" or "apply with dry-run". No resource CRUD.

A GUI that can change configuration would give the declarative model a second source of
truth, and the two would drift.

### Traffic classification and analytics

No deep packet inspection, no flow export, no telemetry to a vendor service.

### Package management and OS provisioning

regied does not install packages, manage repositories, or provision hosts. It expects the
tools it delegates to — systemd-networkd, dnsmasq, pppd, nftables — to be present.

### Login accounts and remote access

User accounts, password hashes, SSH keys, and the SSH daemon's configuration stay with
the operating system. regied does not model them, and they never appear in its
configuration.

## Not modeled, because it belongs to the OS

Some things a vendor router OS bundles into "the configuration" are, on a general-purpose
Linux system, already owned by something else. regied does not take them over.

| Area | Owner |
|---|---|
| System logging | journald / rsyslog |
| Time synchronization client | The system's NTP client |
| Login accounts and SSH | The operating system |
| Package repositories | The distribution's package manager |
| Base network on a non-router host | netplan or whatever already owns it |

Hostname, timezone, and NTP servers are close to the boundary. They may be modeled later
if the declaration is more useful than the OS's own mechanism, but they are not part of
the seven areas being built first.

## Hardware-dependent, and therefore deferred

Some vendor routers expose an integrated switch fabric that can be configured — port
membership, VLAN awareness, hardware offload. Whether the equivalent is available on a
given board depends on that board's DSA support in the kernel.

regied models a Linux bridge instead. Hardware switch configuration is deferred until
there is a specific board whose support is confirmed, at which point it becomes a
requirement rather than a guess.

## Deliberate behavioral changes

Porting a configuration is not the same as copying it. Two changes are worth naming
because they are improvements rather than omissions.

**TCP MSS clamping is applied to every reduced-MTU path**, not only to a single interface
type. A tunnel with a lower MTU than the LAN needs it just as much as a PPPoE link does,
and relying on path MTU discovery alone is fragile.

**ICMP redirects are not sent.** A vendor default that enables them is not a reason to
carry it forward; on a single-segment LAN there is nothing for a redirect to accomplish.
