# ADR 0012: What regied writes into /etc/systemd/network

- Status: Accepted (2026-09-02)

## Context

[ADR 0008](0008-delegate-to-existing-implementations.md) hands links, addresses, MTUs,
bridges, static routes, prefix delegation, router advertisement, the ip6tnl tunnel and
the routing half of policy routing to systemd-networkd, and
[ADR 0011](0011-target-platform.md) fixes the version that has to accept them. Turning a
validated configuration into those files raises questions neither ADR answers.

Four of them matter beyond this backend, because something else in regied depends on the
answer: which file wins when networkd picks one, what a renderer is allowed to know,
which name a link ends up with, and who configures a link networkd did not create.

## Decision

**One file per resource, named `50-regied-<resource name>` under
`/etc/systemd/network/`.** The prefix is the ownership marker
([ADR 0009](0009-ownership-boundary.md)): what carries it is regied's to rewrite and to
reclaim, and what does not is somebody else's. The number decides which `.network` takes
a link, because networkd sorts the candidates by file name and the first match wins. 50
leaves both directions open — ahead of the 80- files systemd and the distribution ship,
so the links regied declares are configured the way regied says; behind the 10- range
hand-written overrides and other renderers conventionally use, so an operator can put a
file in front of one of ours without editing it.

**Rendering is a pure function of the configuration and of the values that exist only at
apply time.** Those values — the address a provider's AFTR name resolved to, and the
contents of a DUID file — are arguments, not something the renderer goes and fetches.
Every test is then plain Go, and the same rendering can be produced for a host other than
the one running it. Writing the files, reloading networkd, and diffing against what is
already there belong to the apply engine.

**A link is named after the resource that declares it.** A `DSLiteTunnel` and a
`PPPoESession` carry no interface name, so the resource name is the kernel name. The
firewall and the policy routing name links, and this is what lets them.

**networkd is given the PPPoE link for the routes that leave by it, and nothing else.**
pppd creates the link, names it, addresses it and installs its default route. But a route
has to live on the link it leaves by, and pppd's option file cannot carry one, so regied
writes a `.network` for that link holding the routes and nothing else, with
`KeepConfiguration=yes` so that networkd drops nothing pppd installed. A session that
declares no route and that no policy names gets no file at all and its link stays
unmanaged.

Both kinds of route go that same way: the static routes a `PPPoESession` declares, which
are the same thing an `Interface`'s are, and the default route a policy's table needs.
The alternative for the static ones was for the apply engine to install them over netlink
once the link came up, which would mean regied watching a link it has already rendered
and putting the routes back after every redial — the structure ADR 0009 avoids, and the
lifecycle ADR 0008 declined to write. It would also make how a route is installed depend
on which kind of uplink it leaves by, which is one more thing to know during an outage.

**The routing policy rule's priority is the table number.** Both are derived together and
are unique by construction, and the range they are allocated from sits between the rule
the kernel keeps for the local table and the one it keeps for main, which is where a rule
has to be to have any effect. The `priority` an `EgressRoutePolicy` carries orders the
nftables match, not the kernel's rules, and reusing it here would put an operator's number
in a place where 0 replaces the local table.

**The DHCPv6 client asks for the delegation and nothing else.** The schema has no field
for taking an address from DHCPv6, so the client does not take one, and the interface's
own global address comes from the router advertisement. That is also what makes the
tunnel's local address well defined: `slaac` is the only kind of address the underlay
has.

**A declaration systemd 257 cannot render is reported, not dropped.** Rendering returns
warnings alongside the files. Two exist today, both from
[ADR 0011](0011-target-platform.md)'s missing directive:

- a `localAddressFrom` naming anything but the underlay, which renders as the underlay's
  own address instead
- `validLifetime` and `preferredLifetime` on an interface that advertises only a
  delegated prefix, whose lifetimes come from the delegation

## Consequences

- Reclaiming what an earlier apply left behind is a glob over the prefix. The apply
  engine never has to keep a list of what it wrote.
- The nftables backend and the apply engine can build interface names from resource names
  without asking this package.
- `regied render` and `--dry-run` have somewhere to put a warning, and the two above
  reach the operator before an apply rather than after one. If a deployment needs either
  of them to work as written, that is a reason to revisit the platform, in the sense ADR
  0011 already gives it — not a reason to build the address lifecycle here.
- The whole rendering of `config/example.yaml` is held as a golden file. The apply engine
  and the integration tests take that output as the shape they handle, so a change to any
  of it shows up in a diff and has to be argued for.
- Giving networkd a `.network` for the PPPoE link is the one place where two writers meet
  on one link. It is deliberate and it is narrow — the file carries routes and
  `KeepConfiguration=yes`, and never an address — but it is where to look first if a
  redial ever comes back without its routing.
