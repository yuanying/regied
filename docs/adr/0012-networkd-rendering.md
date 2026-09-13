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

*Extended by [ADR 0020](0020-link-settings-applied-by-udev.md): an Interface that declares a
setting of its NIC also gets a `.link` file under the same prefix. udev reads it, not networkd,
and that record says what it matches on and how an apply makes it take effect.*

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

**The DHCPv6 client asks for an address alongside the delegation.** The schema has no
field for taking an address from DHCPv6, and the first rendering wrote `UseAddress=no` so
the client would ask for the prefix and nothing else. In networkd, though, that setting
also decides whether the client puts IA_NA into its Solicit at all: requesting the address
and using it are one switch. A provider exists that never answers a Solicit carrying only
IA_PD, while the router being replaced, which sent IA_NA and IA_PD together, held its
delegation the whole time. So the upstream `.network` writes `UseAddress=yes`, networkd's
default, spelled out because the file should show the intent rather than lean on it. The
usual reply carries no address in the IA_NA and the prefix in the IA_PD; on the rare line
that does hand out an address, it lands on the upstream link and harms nothing. The
interface's own global address still comes from the router advertisement, which is what
keeps the tunnel's local address well defined: `slaac` remains the address the underlay
has. No field exposes this; should a line ever need the address request withheld,
`dhcpv6.requestAddress` is the field to add. The IAID is likewise left at networkd's
default; a provider that binds the delegation to it would get a field, not a constant.

**The IAID is carried over the same way the DUID is.** The paragraph above left the IAID
at networkd's default and promised a field should a provider ever bind the delegation to
it. One does. It holds the binding under the DUID and the IAID as a pair, and to a request
carrying any other IAID it does not delegate a different prefix: it does not answer at
all, and the client keeps soliciting. networkd derives its default IAID from the
interface, so a host replacing a router is all but guaranteed not to match by accident,
exactly as it would not match the DUID without `duidFile`. So `prefixDelegation.iaid`
joins `duidFile` as the second value carried over from the replaced router's
configuration, and this paragraph supersedes the sentence above that left it at the
default. The field is optional: a line being brought up for the first time has nothing
to carry over, and leaving it out still hands the choice to networkd. A declared `0`,
which is the usual value, is distinct from leaving it out, which is why the schema holds a
pointer. It is a field rather than a constant `0` for the same reason the DUID is read
from a file: the value is whatever the replaced router sent, and a constant would only
move the mismatch to the next router. This too surfaced on a real line. The replaced
router's client software, given its DUID and IAID, drew the same delegation on the first
Solicit; networkd with the same DUID and its own IAID drew silence, and with the IAID
declared it drew the delegation.

**And it asks whether or not the router advertisement invites it.** networkd starts the
DHCPv6 client, by default, only when an advertisement carries the managed or
other-configuration flag, and `WithoutRA=solicit` covers only the line where no
advertisement arrives. A provider can advertise the default route with neither flag set
and still delegate prefixes over DHCPv6; on such a line the client never sends a Solicit
and the delegation never comes. So the upstream `.network` sets `DHCPv6Client=always`
beside `WithoutRA=solicit`. The flags are the provider's statement about the hosts on
that link, not about whether it delegates, and regied does not read them as one. This
surfaced on a real line, not on the netns testbed, whose WAN-side IPv6 is placed
statically and advertises nothing.

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
