# ADR 0021: Be the load balancer for the Kubernetes clusters behind this host

- Status: Proposed (2026-10-18)
- Widens [`docs/scope.md`](../scope.md); amends [ADR 0013](0013-nftables-ruleset-shape.md)
  and [ADR 0020](0020-port-forward-reply-on-arrival-uplink.md)

> **Nothing here is built, and the kind and field names are provisional.** This record
> decides a direction and proposes answers to the questions it raises. The field-level
> shape goes into [`docs/spec/`](../spec/) once the direction is accepted, and not before.

## Context

### The requirement

A router sits in front of one or more Kubernetes clusters on its LAN. Each cluster wants
`Service` objects of `type: LoadBalancer` to get an address that clients can reach — from
the LAN, from the IPv6 internet, and through the router's IPv4 uplink — and each wants a
stable address for its own API server (the *control-plane endpoint*) that every node and
every client uses.

The deployment assumed has these properties.

- **The router has one public IPv4 address, and it is dynamic.** It is held by a PPPoE
  session or a similar uplink, it changes on a redial, and nothing can be routed to a
  cluster through it. IPv4 reaches a cluster only by destination translation on the
  router, port by port.
- **IPv6 comes from a delegated prefix.** The router receives a prefix by DHCPv6 prefix
  delegation and can carve subnets out of it. There is no IPv6 NAT
  ([`docs/scope.md`](../scope.md)), so a published IPv6 address must be a routed address
  inside that prefix.
- **There is more than one cluster.** A production cluster and a development one, say,
  side by side behind one router. Neither may take the other's addresses or ports, and
  a mistake in one cluster's manifests must not be able to publish something on the
  other's behalf.
- **The control-plane endpoint must not depend on the cluster.** The usual way to hold it
  is an in-cluster load balancer — a controller that announces the address by ARP, NDP or
  BGP. On a cold start that is circular: the controller needs the API server to learn
  what to announce, and the API server is reached through the address the controller
  announces. A cluster brought down entirely can fail to come back up without a person
  stepping in.

### What an in-cluster load balancer leaves undone

An in-cluster implementation can give a `Service` a LAN address and announce it. It
cannot make the address reachable from outside on this kind of deployment.

- The public IPv4 address belongs to the router. Every published port needs a
  `PortForward` on the router, written by hand, kept in step with the cluster by hand, and
  written again for every cluster with its own port range.
- A published IPv6 address needs a firewall opening on the router, written by hand the
  same way.
- The control-plane endpoint keeps the circular dependency above.

The router is the one place that already holds the uplink, the delegated prefix and the
firewall. What it lacks is knowledge of what the clusters are asking for.

### What the existing records already say

**Reading another system to learn what this host should do is in scope.**
[ADR 0007](0007-resident-process.md) states that the line ADR 0009 draws is about hosts,
not about sources: "Reading another system's API to find out what this host should do is
on the near side of the line; telling another host what to do is on the far side." The
same record distinguishes *present*, *absent* and *could not ask*, and refuses to let the
third pass for the second.

**This exact case has been kept open.** ADR 0007's consequences name "the healthy
backends behind a load-balanced address a Kubernetes cluster asked for" as runtime state
that would sit where the uplink sets sit, written by the resident process, leaving the
rendered text alone. [ADR 0016](0016-converging-on-the-accepted-declaration.md) repeats it
in its last consequence. [ADR 0017](0017-submission-through-the-resident-process.md)
gives the resident process the only position of writer partly so that this state would
survive a replacement of the table: with one writer, the turn that replaces the table is
the turn that seeds every element the daemon knows.

**Runtime state goes in sets and maps, not in the text.**
[ADR 0015](0015-uplink-addresses-in-sets.md) keeps the uplink addresses out of the ruleset
so that the rendered text stays a function of the declaration alone. Its warning, repeated
in ADR 0007, 0016 and 0017, applies here word for word: the uplink sets are safe because
the kernel holds their answer, and an element whose source is not the kernel has no such
recovery. This record is the one that has to answer it.

**A write to another system's API has a precedent.**
[ADR 0019](0019-uplink-address-in-dns-records.md) lets regied write DNS records at a
provider, on the grounds that it tells the provider about *this* host and takes no
configuration from it.

**Replies to a translated connection already go back the way they came.**
[ADR 0020](0020-port-forward-reply-on-arrival-uplink.md) saves the arrival uplink on the
connection and restores it on the reply, so a destination translation from an uplink to
an address inside answers by that uplink whatever the policies say.

### What gets in the way

[`docs/scope.md`](../scope.md) says regied looks after one node and has "no failover",
and that dynamic routing belongs to FRR, BIRD or GoBGP. Health-checking backends and
moving traffic off a failed one reads like failover, and installing routes that change at
run time reads like dynamic routing. [ADR 0008](0008-delegate-to-existing-implementations.md)
asks whether an existing implementation already owns the layer. Both are answered below.

## Decision

### regied is the load balancer for the clusters it is told about

**regied's resident process watches the API of each declared cluster, read-only, and
makes the `LoadBalancer` services that cluster asks for reachable within the bounds the
declaration sets for it.** It allocates their addresses, routes the addresses to healthy
nodes, translates the uplink's ports to them, opens the firewall for them, and holds the
cluster's control-plane endpoint.

**The declaration says how much is delegated; the cluster says what to do inside it.** A
person writes, once per cluster, the bounds — which addresses, which uplink ports, which
nodes, which control-plane endpoints. Everything inside the bounds is decided at run time
from what the cluster's API says, and is runtime state in the sense of ADR 0015: the
rendered text depends on the bounds and not on any `Service`.

**Pull, not push.** The cluster does not reach regied. No endpoint is added to regied, no
write API exists ([`docs/scope.md`](../scope.md)), and nothing in the cluster needs to know
how to talk to the router. What reaches regied is what it reads with a credential the
operator gave it.

### Why it is in scope, and where the line stays

- **It is still one node.** regied manages this host's routes, translation and firewall.
  It reads a cluster's API to learn what this host should do, which ADR 0007 already put
  on the near side of the line. It tells no host what to do, ships no configuration
  anywhere, and does not join the cluster.
- **Health-checking a backend is not failover of regied.** The router is still a single
  point; nothing here makes regied redundant, elects a leader or moves its state. What
  moves is which backend a packet goes to, which is what any load balancer does and what
  a destination translation already is.
- **Runtime routes are not a routing protocol.** regied speaks no protocol to any peer and
  learns no route from one. It computes routes from two sources — a cluster's API and its
  own health checks — and owns them the way it owns every other route it installs
  ([ADR 0009](0009-ownership-boundary.md)). BGP, OSPF and RIP stay with FRR, BIRD or
  GoBGP.
- **One write to the cluster, and only one** — the address of each service regied
  serves. It is settled below, because it is the part that is closest to the line.

### Why not an existing implementation

ADR 0008's test is whether someone else already owns the layer. Here the layer has three
parts.

| Part | Owned elsewhere? |
|---|---|
| Destination translation, routes and firewall on the router | No. ADR 0008 lists NAT and the firewall among what regied owns, and the routes are this host's |
| Health-checking backends and choosing among them | Yes: keepalived with IPVS. But keepalived does not watch a cluster, and its configuration would have to be generated from runtime state |
| Watching a cluster's API to learn services and nodes | Only inside the cluster: MetalLB, kube-vip, Cilium. They announce on the LAN and do not reach the router's uplink or firewall |

**keepalived and IPVS** were the serious alternative. Rendering a keepalived configuration
from the services a cluster asks for would make a rendered artifact a function of runtime
state, which ADR 0015 exists to prevent, or would need a second writer generating it
beside the turn, which ADR 0017 exists to prevent. IPVS would also be a second packet path
next to nftables, with its own state to read during an outage. IPVS's direct routing
keeps a connection on its backend when the set of backends changes, which plain routes do
not (see the consequences); that is the one property worth revisiting it for, and it is
not needed to start.

**In-cluster implementations** answer a different question. They decide where an address
lives on the LAN. The router would still need a hand-written `PortForward` and a firewall
opening for everything published, per cluster, and the control-plane endpoint would keep
its circular dependency.

### The resource: one kind per cluster

**One new kind, provisionally `KubernetesLoadBalancer`, one resource per cluster.** It
carries the cluster's connection, its bounds, and its control-plane endpoint.

ADR 0002's tests point this way. The resource has a lifecycle of its own — a watch that
connects, drops and reconnects, and health checks that run — and it repeats with entries
meaningful on their own, one per cluster. The parts were considered as kinds of their own
and folded in, because nothing names them: a cluster connection is used by exactly one set
of bounds, and a control-plane endpoint belongs to exactly one cluster.

| Field (provisional) | What it holds |
|---|---|
| `kubeconfigFile` | Path to a kubeconfig holding the cluster's API address and a credential ([ADR 0003](0003-secrets-out-of-configuration.md)) |
| `addresses.ipv4` | A prefix from which IPv4 service addresses are allocated |
| `addresses.ipv6` | A prefix from which IPv6 service addresses are allocated: a literal, or `fromDelegatedPrefix` with `interfaceRef` and `subnetID`, in the form `Interface.addresses[]` already uses |
| `nodes.within[]` | Prefixes a node's address must fall in to be used as a next hop. Both families, if both are served |
| `publish[]` | Per uplink: `egressRef` and the uplink ports this cluster may publish on |
| `controlPlane.address` | The control-plane endpoint, inside `addresses.ipv4` |
| `controlPlane.port` | Default 6443 |
| `controlPlane.endpoints[]` | The control-plane nodes' addresses, written by hand |
| `controlPlane.healthCheck.path` | Default `/readyz` |

A second cluster is a second resource. Nothing in the kind refers to another.

### Addresses

**IPv4 service addresses come from a private range outside every LAN segment.** Placed on
a segment, an address would be on-link to every client there: clients would ask for it by
ARP, the router would have to answer for addresses it does not hold — by adding them to
the link or by proxy ARP, either of which puts addresses on a link that the link's owner
did not declare — and a node answering a client on the same segment would answer it
directly, so every LAN client would need source translation and the service would lose
the client's address. Outside every segment, clients send to their default gateway, which
is the router, and routing does the rest.

**IPv6 service addresses come from a subnet of the delegated prefix that no link
carries.** It is routed to nodes, not advertised on a segment. Written as
`fromDelegatedPrefix`, it follows a change of delegation with no edit: the host part of
every allocated address is kept, and the prefix is replaced.

**Allocation is regied's, and it is recorded in the cluster.** A `Service` that asks for no
particular address gets the lowest free one in the range. The address is written to the
service's status (below), which is where regied reads it back after a restart, so an
allocation is stable for as long as the service exists. The control-plane endpoint is
reserved and never allocated.

**Which services are served** is decided by `spec.loadBalancerClass`. regied serves a
service whose class is its own, in the cluster it watches; a service with no class, or
another class, is left to whatever else is in that cluster. That is what lets an
in-cluster implementation keep its services while they move over one at a time.

### How traffic reaches a service

**A service address is routed to the healthy nodes, and the node's kube-proxy takes it
from there.** kube-proxy installs its rules for every address in a service's
`status.loadBalancer.ingress`, so a packet arriving at a node for that address is handled
as it would be from any load balancer.

| `externalTrafficPolicy` | Next hops | Health check |
|---|---|---|
| `Cluster` | Every healthy node | kube-proxy's health endpoint on the node (`:10256/healthz`) |
| `Local` | Only nodes that have a ready endpoint for this service | The service's `healthCheckNodePort` on the node, `/healthz` (200 with a local endpoint, 503 without) |

A route is installed per service address, as a multipath route over its next hops. **The
whole range has an unreachable route**, derived from the declaration and rendered as
configuration. An address nothing claims is then answered by the router as unreachable;
without it, a packet for an unallocated address would follow the default route, reach a
node with no rule for it, be sent back by the node's default route, and loop until its
hop limit ran out.

Nodes are taken from the cluster's `Node` objects: a node is a candidate when it is
`Ready`, its `InternalIP` of the family is inside `nodes.within`, and it does not carry
the well-known label that excludes a node from external load balancers. `nodes.within` is
a safety catch: a compromised or misconfigured cluster cannot make the router send traffic
to an address outside it. An IPv6 address needs nodes with an IPv6 `InternalIP`; a route
cannot go through an IPv4 next hop.

The endpoints behind a `Local` service are learned from the health check, not from the
cluster's `EndpointSlice` objects. kube-proxy already answers the question regied would
compute from them, and answering it from the node keeps regied's reads to two resource
types.

**Multipath routes hash on the transport ports**, so that the connections of one client
spread over the nodes. regied sets the kernel's multipath hash policy for both families
when a `KubernetesLoadBalancer` is declared, as it already does for the switches in
`spec.global`.

### Publishing through the uplink

**A service is published by an annotation on it**, provisionally
`net.unstable.cloud/publish`, naming the uplink — an `egressRef` from the cluster's
`publish[]`. Nothing is published without it: a service with an address and no annotation
is reachable from inside only.

**The service's `port` is the uplink port.** A published TCP port 443 is the uplink's 443,
translated to the service address on 443. There is no table of uplink ports beside the
service's own: a second number would be one more thing to keep in step, and a service
that wants a different public port says so by having a different port.

| Family | What publishing does |
|---|---|
| IPv4 | A destination translation from the uplink's address and port to the service address on the same port; from there, the route above. The reply is put back on the uplink by ADR 0020's connection mark, and a LAN client reaching the uplink's address is hairpinned by ADR 0013's source translation, as with a `PortForward` |
| IPv6 | A firewall opening for the service address and port, for traffic arriving on the interface the range is delegated through |

**The firewall opens only what is published.** The bounds say how far the cluster may go;
the openings themselves are elements written at run time, for the addresses and ports of
published services and nothing else. Traffic from inside zones to a service address is
governed by the policies the declaration already has.

### The control-plane endpoint

**The control-plane endpoint is taken from the same IPv4 range, and nothing about it is
learned from the cluster.** Learning its backends from the cluster's API would be the
circular dependency this record exists to remove. Its endpoints are declared, and each is
checked by an HTTPS request to the health path on the declared port.

**It is translated, not routed.** A connection to the endpoint is translated to a healthy
control-plane node and its source is translated to the router, so that the reply comes
back through the router even though the client and the node may share a segment. It does
not go through kube-proxy, because kube-proxy cannot build its rules on a cold cluster
without the API server.

- **The health check does not verify the certificate.** It decides only whether to send a
  connection to that node; the connection itself is TLS between the client and the API
  server, and verifying would require the cluster's CA before the cluster exists.
- **When no endpoint passes, all of them are used.** An API server that is not ready yet
  still answers, and every client of it retries. Refusing every connection while a
  cluster is coming up would hold up the very components that make it ready.
- **The translation is also applied to traffic the host itself sends.** A kubeconfig
  naming the control-plane endpoint — the usual case, regied's own included — has to
  reach it from the router.

Because it needs nothing from the cluster, the order of installation is: declare the
bounds on the router, then install the cluster against the endpoint. The kubeconfig regied
reads can be placed afterwards; until it is, the control-plane endpoint works and nothing
is published.

### Overlap is refused statically; conflict is refused at run time

**Bounds must not overlap, and validation says so before anything is applied.** The
checks extend the one proposed for overlapping `PortForward`s (not yet merged, regied#43):

- The IPv4 and IPv6 ranges of two resources, and either of them with an address of any
  `Interface` or a static route.
- `nodes.within` with the ranges, since a next hop inside the range it serves would loop.
- The control-plane address outside its own IPv4 range.
- The uplink ports of two resources on the same uplink, and either with a `PortForward`
  on that uplink and protocol, by the same definition of overlap as that check.

**Inside the bounds, the earlier service wins.** Two services asking for the same uplink
port, or a service asking for an address or a port outside its cluster's bounds, are
refused at run time: the service created first keeps what it has, the later one is not
served for that port or address, and the reason is reported — in the turn's report
([ADR 0016](0016-converging-on-the-accepted-declaration.md)), and on the service itself,
in the per-port error that `status.loadBalancer.ingress[].ports[]` exists to carry.

### Where it sits in the ruleset and the loop

**The ruleset stays a function of the declaration.** The new rules name sets and maps
whose elements are written at run time; no service address, port or node address appears
in the text. ADR 0013's table gains:

- in `prerouting_nat`, the publish translation, matching the uplink's address set
  ([ADR 0015](0015-uplink-addresses-in-sets.md)) and looking the protocol and port up in a
  map to the service address; and the control-plane translation;
- a base chain on the output hook, at priority `dstnat`, holding the control-plane
  translation for traffic the host sends;
- in `postrouting_nat`, the source translation for connections translated to a
  control-plane node;
- in `forward`, after the port forward openings, the published openings, matching a set
  of address, protocol and port per family.

**The control-plane translation chooses among a fixed number of slots.** The rule hashes a
connection onto as many slots as there are declared endpoints, and a map holds which
endpoint each slot sends to: a healthy endpoint is in its own slot, and an unhealthy
endpoint's slot holds a healthy one. The number is in the declaration, so it can be in
the text; which endpoint fills each slot is runtime state. A rule whose divisor changed
with health would put runtime state back into the text.

**ADR 0020's mark is derived for every uplink a `KubernetesLoadBalancer` publishes on**,
the same as for an uplink a `PortForward` is published on, since a published service is a
destination translation from that uplink.

**A turn writes all of it.** A watch event and a change in a health check's result wake a
turn, the way a kernel address event does. The turn writes the map and set elements, the
routes and the service status that differ, in the same place it seeds the uplink sets,
and the same turn that replaces the table refills every element it holds — which is the
answer, for this state, to the warning in ADR 0007, 0016 and 0017: the turn that empties
the sets is the turn that knows what goes back in them. An unattended turn may do all of
this. Moving traffic onto what is healthy takes down nothing that was up; it is the
declaration's meaning applied to new facts, as seeding an uplink set is.

**What the cluster could not be asked keeps its last answer.** A watch that drops is
*could not ask*, not *absent*: the elements stay, the turn reports that it is waiting on
the cluster's API, and the health checks keep running, so a node that fails is still taken
out. A service that the API says is gone is *absent*, and its elements, routes and
openings go.

**Removing a resource from the declaration removes what it served**, and the turn that
does it clears the status it wrote on that cluster's services, as far as it can reach
them. Stopping regied leaves everything as it is, which is what the stop lever means.

### The route's owner

**Runtime routes carry a routing protocol number of regied's own**, which is ADR 0009's
ownership marker for routes. regied removes only routes that carry it, and never removes
one that does not.

**systemd-networkd removes foreign routes when it configures a link, unless told
otherwise.** `ManageForeignRoutes=` defaults to yes, and the only protocol it spares
unconditionally is `kernel`. A route regied installs at run time would be removed by the
next reload an apply causes, and put back by the turn — a gap on every networkd change.
regied therefore renders a `networkd.conf` drop-in turning `ManageForeignRoutes=` off, on
a host that declares a `KubernetesLoadBalancer`, under its own name prefix and with the
ownership marker, like its other files in shared directories.

### Proposed for approval

These are the choices this record makes where more than one answer was defensible. Each is
proposed, and each can be changed before the record is accepted.

#### 1. regied writes the service's status

**Proposed: regied writes `status.loadBalancer` of the services it serves, and nothing
else in the cluster.**

| Option | For | Against |
|---|---|---|
| **regied writes it** | The party that allocates and judges conflicts is the party that reports them: an address in status is one regied serves, and a refused port says so on the service. Status also becomes the record of an allocation, which keeps it stable across a restart without regied keeping a file | A write to another system's API, which needs an exception of the kind ADR 0019 made |
| A small controller in the cluster writes it | regied stays read-only | The bounds are written twice — in the declaration and in the controller's configuration — and the two must agree on allocation and on conflicts. When they disagree, status names an address the router does not serve |
| An in-cluster load balancer allocates without announcing | No new component | Same two places; and whether such an implementation allocates without announcing is not a documented behaviour to build on |

The exception is narrow in the way ADR 0019's is. regied writes the address *this host*
serves for a service — the cluster is being told about this host — and takes no
configuration from the cluster beyond what it was asked to read. The credential needs
read access to services and nodes and write access to the status subresource of services,
and no more.

#### 2. One class name, common to every cluster, and not a field

**Proposed: regied serves one `loadBalancerClass`, the same in every cluster, with no
field to change it.** Its name is under `net.unstable.cloud/`, not under the project's
name, for the reason the apiGroup is ([ADR 0002](0002-configuration-schema.md)); the exact
value is the spec's.

Which cluster a service belongs to is already fixed by which API regied read it from, so a
class per resource would carry no information, and would make a manifest moved from one
cluster to another need an edit. A field can be added when two routers serve one cluster
and need to tell their services apart, which is the case that would need it.

#### 3. After a restart, nothing is published until the cluster answers

**Proposed: regied keeps no file of what it last served. A resource whose cluster has not
answered since the process started publishes nothing and reports that it is waiting on
the cluster's API.** The control-plane endpoint is unaffected, because it needs nothing
from the cluster.

The alternative is to restore the last answer from a file and publish it until the
cluster answers. It covers one case: a cluster that is up and serving while its API
cannot be reached from the router. That case is rare once the control-plane endpoint is
the router's own, since the API is then reached through the router itself. Against it, a
file is one more artifact whose content is not the kernel's and not the declaration's, and
a service deleted while regied was down would stay published — firewall opening included
— until the cluster answered. Allocations are not lost either way, because they are in the
cluster.

#### 4. Routes carry regied's protocol number, and networkd leaves foreign routes alone

**Proposed: as in "The route's owner" above.** The alternative of keeping networkd's
default and reinstalling routes after every networkd reload leaves a gap on every apply
that touches a `.network` file. Turning `ManageForeignRoutes=` off also means networkd no
longer removes routes someone else left behind; on this host that is ADR 0009's rule
applied to networkd rather than a loss. `ManageForeignNextHops=` stays as it is while no
nexthop object is used.

#### 5. The ruleset holds sets, maps and a fixed number of slots

**Proposed: as in "Where it sits in the ruleset and the loop" above.** Service traffic is
routed, so it needs no per-backend rule; only the control-plane endpoint is translated to
a backend, and its slot count comes from the declaration.

#### 6. A minimal client for the cluster's API, not client-go

**Proposed: regied speaks to the cluster's API with its own small client.** It lists and
watches two resource types, resumes a watch from its resource version and lists again
when the version is too old, and patches one subresource. It reads a kubeconfig's server,
CA, and client certificate or bearer token; a kubeconfig that needs an exec plugin is a
validation error.

regied has one third-party dependency today, and it reaches Cloudflare with its own small
client for the same reason ([ADR 0019](0019-uplink-address-in-dns-records.md)). client-go
would bring in a large module tree to use a corner of it, and its informers solve a
problem — many controllers sharing caches of many types — that this does not have. The
loop is already level-triggered and resyncs on its own, which is the part of an informer
that matters here.

#### 7. The netns testbed gains a fake API and several nodes

**Proposed:** [ADR 0010](0010-netns-testbed.md)'s testbed gains a fake cluster API — a
small HTTP server serving lists and watches of services and nodes from fixtures and
accepting status patches — and several node namespaces, each accepting traffic for the
service addresses it is given and answering health checks on demand. It checks, from
outside the router: that a published service is reached through the uplink and answers
by it; that it hairpins; that an IPv6 address is reached; that a node failing its health
check stops receiving new connections; that connections spread over the nodes; that the
control-plane endpoint works with the fake API down; and that an unallocated address in
the range is answered as unreachable. The client, allocation and conflict rules are unit
tests against a fake API in `make test`. A real cluster is not part of the testbed.

#### 8. One kind and one record

**Proposed: one kind, as above, and this one record.** The connection, the bounds and the
control-plane endpoint are not named by anything else and share one lifecycle, so they
are one resource. The decisions are one feature, and splitting the record would separate
the scope change from the mechanism that justifies it.

### What the spec has to settle

The names of the kind, its fields, the class and the annotation; whether a service may ask
for a specific address and how; whether `publish[].ports` covers TCP and UDP alike or
takes a protocol; the health-check interval and thresholds; the protocol number; and the
exact text of the per-port errors.

## Consequences

- **Publishing becomes an operation on the cluster.** Once the bounds are declared,
  creating a service with the class and the annotation gives it an address, an uplink
  port, an IPv6 address and a firewall opening, and deleting it takes them away. The
  router's declaration does not change per service.
- **Clusters cannot step on each other.** Their bounds are checked against each other and
  against the rest of the declaration before anything is applied, and nothing a cluster
  says can reach outside its own.
- **The control-plane endpoint stops depending on the cluster**, and a cold start has no
  circular dependency left in it. In exchange, it depends on the router.
- **More traffic goes through the router.** Traffic from LAN clients to service addresses,
  and from nodes to the control-plane endpoint, now crosses it. A reboot of the router or
  an apply that breaks it reaches the clusters' control planes, not only their published
  services.
- **Traffic from LAN clients is asymmetric.** A client on the same segment as the nodes
  reaches a service address through the router, and the node answers it directly. Any
  firewall policy between those zones that relies on connection state will drop it; the
  policy for that path has to be stateless. The router sends no ICMP redirects
  ([`docs/scope.md`](../scope.md)), so the client keeps sending through it.
- **A change in the set of next hops can break established connections.** Multipath
  routing hashes each packet; when a node is added or removed, some flows hash to a
  different node, which has no connection state for them and resets them. The published
  IPv4 path is affected the same way, because its translation is to the service address
  and the route is consulted per packet. Resilient nexthop groups or IPVS's direct routing
  are what would reduce it, and neither is needed to start.
- **`Cluster` services lose the client's address**, because kube-proxy translates the
  source on the way to a pod on another node; `Local` services keep it. That is
  kube-proxy's behaviour and unchanged by this record.
- **The nodes must accept the health checks** from the router on kube-proxy's health port
  and on each service's `healthCheckNodePort`. A cluster that filters traffic to its nodes
  has to allow them.
- **The router keeps a credential for each cluster**, in a file, as it does for its other
  secrets. A cluster's operator grants it read access to services and nodes and write
  access to service status.
- **regied gains a client for a second external API**, a long-lived connection per cluster,
  health checks, runtime routes, a `networkd.conf` drop-in and two kernel switches it sets
  on its own. It is the largest single addition since the resident process, and the
  testbed grows with it.
- **ADR 0013's chain table and ADR 0020's derivation of uplink marks are amended**, and
  each carries a note when this record is accepted.
- [`docs/scope.md`](../scope.md)'s sections on dynamic routing and on multi-node, high
  availability and cloud integration are rewritten with this record.
