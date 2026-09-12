# ADR 0019: Keep DNS records at the uplink's address

- Status: Accepted (2026-09-11). Settled and built (2026-09-12).

> **The direction below was decided first and built afterwards.** The six questions this
> record left open are answered under [What was settled when it was
> built](#what-was-settled-when-it-was-built), and the shape those answers took is the
> `DNSRecordSet` kind in [`docs/spec/kinds.md`](../spec/kinds.md#dnsrecordset), with a worked
> example in [`config/example.yaml`](../../config/example.yaml). The spec is what regied
> accepts; this record is why.

## Context

### The requirement

A host that publishes a service through a `PortForward` is reached by a name, and the name
has to resolve to the address the uplink holds. On a dynamically addressed uplink that
address changes — on a redial, on a provider's maintenance, on a reboot — and a record
that still holds the old one makes the service unreachable from outside until somebody
edits it by hand. It fails the way the schema already goes out of its way to prevent:
something works until the address changes, and then stops in a way that looks like
something else.

The deployment assumed is one uplink or two, addressed by the provider, with the DNS
zone hosted at Cloudflare and edited through its API.

### What the schema already says about this address

The schema has no field that can hold an uplink's global address, and that is deliberate
([ADR 0002](0002-configuration-schema.md)). `SourceNAT` and `PortForward` refer to the
uplink and take the address from it; the hairpin rules match on a set that follows the
address rather than on the address itself ([ADR 0015](0015-uplink-addresses-in-sets.md)).
A DNS record holding the address is the same value in one more place, and the same
reasoning applies to it: it should follow the uplink, not be written down.

### What regied already knows

The resident process reads every uplink's addresses from the kernel on every turn, and a
kernel address event wakes a turn early ([ADR 0016](0016-converging-on-the-accepted-declaration.md)).
The address a record should hold is therefore already in hand, at the moment it changes,
from the source that is authoritative for it.

### What gets in the way

**Scope.** [`docs/scope.md`](../scope.md) lists "integration with a cloud provider's API"
among the things regied does not do, and says that anything on that list arrives as a
new decision record rather than as a quiet addition. This is that record.

**Delegation.** [ADR 0008](0008-delegate-to-existing-implementations.md) says regied owns
only what nobody else owns. Dynamic DNS clients exist, several of them speak Cloudflare's
API, and they are distribution packages.

**How those clients learn the address.** The common default is to ask an external service
what address a request arrived from. That answers with the address of whichever uplink
the host's *own* traffic leaves by, which is decided by `defaultRoute.metric` and not by
where the services are published. On a host shaped like the worked example — port
forwards on a PPPoE session, the host's own traffic on a DS-Lite tunnel — the answer is
the AFTR's shared address, which nothing can be published through. The clients can be
told to read an interface instead, and then they are correct for exactly as long as their
configuration is kept in step with the declaration by hand.

## Decision

### It is in scope, and the line stays about hosts

**regied may write DNS records at a provider so that they hold an address this host's
uplink holds.** That is the whole of the exception to `docs/scope.md`.

The line that section draws is about hosts: regied looks after one node, does not manage
another, does not distribute configuration, and does not depend on a provider's control
plane for its own configuration. None of that moves. Telling a provider what this host's
address is — outward, about this host, from a declaration a person submitted — is on the
near side of the line in the same way [ADR 0007](0007-resident-process.md) put reading
from another system on the near side: the line is about which host is being managed, not
about whether a network API is spoken to.

What stays out: taking configuration from the provider, managing records that describe
other hosts' uplinks, and any provider feature beyond the record holding an address.

### regied does it itself, not through a dynamic DNS client

ADR 0008's test is whether somebody else already owns the layer. Here the layer is two
things, and only one of them is owned elsewhere.

- **Talking to the provider's API** is owned elsewhere, and it is small: authenticate with
  a token, read a record, write its content.
- **Knowing which address the record should hold, and when it changed** is regied's
  already. It is the function every uplink set computes, from the kernel, woken by the
  kernel's own events.

Delegating would keep the small half out of regied and duplicate the half that matters: a
second program with its own idea of the address, its own polling interval, and its own
configuration file carrying the token, which regied would have to render and supervise
for the declaration to stay the single source. That is more to read during an outage, not
less, which is what [ADR 0001](0001-why-build-our-own.md) is about.

**One provider, and no provider abstraction.** Cloudflare is what this record is for. An
interface that admits a second provider is designed when a second provider is needed,
for the reason ADR 0002 gives for not adding kinds ahead of need.

### What follows from records already decided

These are not open. Each follows from a decision that already stands.

- **The address is never written.** A record refers to the uplink it follows; there is no
  field for the address it holds (ADR 0002).
- **No IPv4 record follows a `DSLiteTunnel`.** The tunnel's IPv4 is translated by the AFTR,
  so nothing can be published through it, and a declaration asking for such a record is a
  validation error, as a `PortForward` naming the tunnel is
  ([`kinds.md`](../spec/kinds.md#dslitetunnel)).
- **The API token is named by the path of a file holding it**, read on the turn that needs
  it and dropped, and it never appears in `--dry-run` output, a diff, a log line or the
  report of a turn ([ADR 0003](0003-secrets-out-of-configuration.md)). The zone and the
  record names are not secrets and belong in the declaration.
- **The resident process is what updates records.** An address event wakes a turn, and the
  turn writes a record that does not hold what the uplink holds. There is no separate
  poller and no timer, and nothing reads the configuration file (ADR 0016). **Stopping
  regied stops the updates**, which is what the stop lever means: leave everything as it
  is.
- **The pppd hooks do not update DNS.** They are the path that needs no daemon, and they
  do one thing — put an address into a set ([ADR 0015](0015-uplink-addresses-in-sets.md)).
  A write to a remote API needs the network, a token and a backoff, which is a turn's
  work and not a hook's.
- **The address goes in the request, never inferred from it.** The update leaves by
  whatever route the host's own traffic takes, which is not necessarily the uplink the
  record follows, so asking the provider to use the request's source address would put
  the wrong address in the record. Sending the address explicitly makes the route the
  request takes irrelevant.
- **A record with no address to hold is left as it is.** While the uplink holds no global
  address — the session is redialling, the line is down — the turn does not delete the
  record and does not write an empty one; it is waiting, and says what on. This is
  ADR 0016's rule that incomplete is never spelled with a half-written artifact.
- **An unattended turn may update a record.** Writing what the kernel says into a record
  takes down nothing that is up; it is a write, in the tier of writing a file. It is under
  per-target backoff, so a provider that is unreachable or refusing makes that record
  *failing* and holds up nothing else.
- **regied touches only the records it was told to** ([ADR 0009](0009-ownership-boundary.md)).
  Everything else in the zone is somebody else's.

### The questions this left open

These stood open between deciding the direction and building it. They are kept as they
were asked, because the answers below are only legible beside them.

1. **What an IPv6 record holds.** The uplink's own global address, or an address inside a
   delegated prefix for a host behind the router — the case where a published service's
   AAAA record names the server itself, because IPv6 has no NAT. The second is the more
   likely need, and it stretches "an address this host's uplink holds" into "an address
   this host routes", which this record had not decided.
2. **A kind of its own, or a field on the uplink.** ADR 0002's first test says a record is
   not referred to by name, which points at a field. An answer to the first question that
   is not about an uplink's own address points away from it.
3. **How far ownership reaches.** Whether regied creates a record that does not exist or
   only updates one that does; whether a record that leaves the declaration is deleted or
   left alone; and, if it is deleted, what marks it as regied's — Cloudflare records carry
   a comment field that could hold ADR 0009's marker.
4. **How drift is seen.** Reading the record from the provider on every turn keeps the
   loop level-triggered, and costs a request per record per resync against the provider's
   rate limit. Remembering what was last written costs nothing and misses an edit made at
   the provider. A read that fails is *could not ask*, never *absent*
   ([ADR 0004](0004-apply-model.md)'s three-valued probe).
5. **What `--dry-run` shows**, and whether it contacts the provider to show it.
6. **Which record properties are declared at all** — the TTL, Cloudflare's proxy flag — or
   left to whatever the record already carries.

### What was settled when it was built

The six questions above were answered on 2026-09-12, and two more came with them. What
the answers became is the `DNSRecordSet` kind.

#### An IPv4 record, and no IPv6 record

**A record follows an uplink's IPv4 address, and nothing here follows an IPv6 one.**

The first question offered two readings of an AAAA record, and they are not variants of
one feature. An AAAA holding the uplink's own global address is almost never what a
deployment wants: IPv6 has no NAT, so the name of a published service has to name the
server, not the router in front of it. The useful AAAA holds an address inside the
delegated prefix, belonging to a host behind this one — which is not *an address this
host's uplink holds* but *an address this host routes*. That is a different statement
about ownership, and it would have to answer who is entitled to publish a name for
another host's address before it could be built.

So the scope of this record is the IPv4 half, which is also the half the requirement came
from: what a `PortForward` publishes is reachable only through the uplink's IPv4 address,
and that address is the one that changes. The record type is declared rather than
assumed, so that the field exists for a later decision to widen; a declaration writing
`AAAA` is refused with the reason, not accepted as something that quietly does nothing.

#### A kind of its own, holding one zone's records

**`DNSRecordSet` is a resource. A single record is not.**

ADR 0002's first test points away from a kind: nothing refers to a record by name, which
is the mark of something to fold into whatever owns it. Applied honestly, that test does
not say "put it on the uplink" — it says find what does own it. Three things say the
uplink does not.

A record is a target that fails on its own. This record already says a provider that is
unreachable makes *that record* failing under a per-target backoff and holds up nothing
else, which is a lifecycle, and ADR 0002 folds away the things that have none.

A record's own attributes have nothing to do with an uplink. A zone, a name, a type, a
proxy flag, a TTL and the path of an API token describe a thing at a provider. An uplink
resource describes a link on this host, and merging the two would make a field list where
half the entries are about somewhere else.

The multiplicity runs the wrong way for folding. A deployment publishes several names,
across several zones, through one uplink. Folded, that is a list of records inside the
uplink's spec, which is a resource list under a resource — the shape `spec.resources`
exists to avoid.

**What owns a record is its zone**, because the credential does. A token is issued for a
zone, and the reason to hold one token per zone is that it is the smaller credential. So
the resource is a zone's worth of records: the zone, the provider, the token that writes
there, and the records regied keeps in it. That is the same shape as
[`IPAddressSet`](../spec/kinds.md#ipaddressset) — a kind whose body is a list — and it is
ADR 0002's fold applied one level further down: a single record has no name anything
refers to and no lifecycle of its own beyond the set's, so it is a list entry and not a
resource.

The resource is a set of records *in* a zone and not the zone itself. regied writes the
records named in the list and knows nothing about the rest of the zone, which is the
ownership boundary the next answer is about.

**`egressRef` sits on the record, not on the set.** Which uplink a name follows is a
property of that name: a zone may well hold one name published through a session and
another that is not published at all. The zone is where the credential lives, not where
the routing decision is.

#### The declaration names its provider, and there is still no provider abstraction

**Every set says `provider: cloudflare`, and there is one implementation with no
interface admitting a second.**

These are not in tension, because they are decisions about different things. The decision
above — one provider, no abstraction — is about the code, and it stands: building a
seam for a second provider before there is one is the anticipation ADR 0002 refuses.

Naming the provider in the declaration is about the file, and it costs nothing now. A
declaration that does not say which provider it targets means "whichever one regied
happens to implement", which is a fact about the binary rather than about the deployment.
The day a second provider is worth building, every declaration already written says what
it was talking to, so adding the second one adds a value and rewrites no file. That is
the opposite of building the abstraction early: it is keeping the *declaration* open
while the code stays closed.

It also makes a provider-specific field checkable. `proxied` is Cloudflare's; nothing
else has it. It is a plain field on the record rather than something nested in a
per-provider container, because one provider does not need a container and inventing one
now would be the anticipation again. What makes that safe is the `provider` field: a
declaration naming a provider that has no such flag can be refused, by name, instead of
carrying a field that silently does nothing.

#### Update, create, and never delete

**A declared record that the provider does not have is created. A record that leaves the
declaration is left exactly as it is.**

Creating is what makes the declaration the whole statement: a host that has to have its
records made by hand before regied can keep them is a host whose configuration is in two
places.

Not deleting is where this differs from every other thing regied owns, and it is
deliberate. ADR 0009's boundary is "remove only what you installed", and it is enforced
on this host by an ownership marker in a file regied wrote. A DNS zone is not this host:
it outlives the host, it is edited by people and by other tools, and a record regied
created is indistinguishable a year later from one somebody made by hand. Satisfying
ADR 0009 by marking records as regied's — Cloudflare's records carry a comment field that
could hold the marker — would mean writing a marker into a field that is a person's note
about the record, and then deleting other people's records whenever a declaration was
edited or a host was decommissioned with its zone still in use.

**So nothing is deleted, and no marker is needed.** The ownership boundary is met by the
stronger statement: regied writes only the records it was told to write, and removes
none. A record no longer declared stops being followed, which is what a person editing
the declaration asked for; taking the name out of DNS is a separate act, done where the
zone is.

**An existing record's other fields are left alone.** The update carries the address and
the declared properties and nothing else, so a comment a person or an earlier tool wrote
on the record survives it. Writing a whole record over the one that is there would erase
notes regied never had any business holding.

#### Remember what was written, rather than reading the provider every turn

**A record is written when the uplink's address is not what this process last wrote
there, and at no other time.**

The alternative was to read each record from the provider on every turn, which keeps the
loop level-triggered in the way the rest of regied is: the host is compared against the
declaration, and what is found decides. It costs a request per record per resync. The
resident process resyncs every minute by default, so eight records make roughly eleven
thousand requests a day to observe an address that changes a few times a year.

The memory is in the process, not on disk. It is empty when the process starts, and an
empty memory is *not known*, never *the same* — ADR 0004's three-valued probe — so the
first turn after any start writes every record once. That is what recovers the
level-triggered property where it matters: a restart is exactly the event after which
regied cannot vouch for what the provider holds, and it costs one write per record rather
than one read per record per minute. Nothing is persisted, because persisting it would
add a file whose only effect is to skip the write that makes the guarantee true.

Resolving a record at the provider — the identifier that says whether to create or to
update, and where the update goes — is read once per record per process and remembered
with the rest. That is a lookup, not a poll.

**The trade-off is stated rather than hidden.** A record edited at the provider by hand is
not corrected until the address changes or regied restarts. The loop converges on the
declaration, and between those two events it is not looking. A deployment that needs
faster correction than that has a reason to revisit this, and revisiting it is a cost
change and not a design change.

A write that fails leaves the memory as it was, so the next turn tries again. That is the
per-target backoff this record already decided: the record is *failing*, and everything
else in the turn is unaffected.

#### `--dry-run` shows the record, and asks the provider nothing

**A dry run names every record it would write and the address it would write, and makes
no request.**

It can, because what decides whether to write is the memory and the kernel, both of which
are here. It must, for the reason ADR 0006 gives: a dry run is what an operator runs when
they are not sure, and a dry run that authenticates to a remote API to answer is a dry run
with a side effect on somebody's rate limit and audit log. The token is not read either,
which is the difference between a promise not to print it and there being nothing to
print (ADR 0003).

The consequence is that a dry run in a fresh process shows every record as one it would
write, because that process has written none. That is what the turn would do, so it is
what the dry run says.

#### The type, the proxy flag and the TTL are declared

**The record's type, its proxy flag and its TTL are in the declaration, and a record is
written with them.**

Leaving a property to whatever the record already carries is only an option while regied
does not create records. It creates them, and a record being created has no existing
value to defer to, so every property a create needs has to be somewhere — and the only
place a person can put it is the declaration.

The proxy flag earns its place beyond that: a deployment that proxies a published name
through the provider and one that does not are different configurations, and which one is
in effect should be readable in the file rather than discoverable at the provider. The
TTL can say `automatic`, which is the provider deciding — the same thing an absent field
asks for, written down so that the choice is visible in the file.

#### The token is named on the set, so that a zone can have its own

**Each set names the file holding the API token its records are written with.**

A token scoped to one zone is the smaller credential, and a deployment should be able to
move to one without the schema being in the way. The set is a zone's records, so the
token sits beside the zone and stands one to one with it: splitting a shared token into
per-zone tokens is changing one path in each set, and nothing else. A deployment that has
not split its token yet names the same file from every set, which is a state the schema
neither prevents nor pretends is something else. The path is in the declaration and the
content never is (ADR 0003); the file is read on the turn that writes, and dropped.

## Consequences

- `docs/scope.md` is amended: dynamic DNS for this host's own addresses is the one
  exception to "no cloud provider integration", and it points here.
- The schema gains a twelfth kind, `DNSRecordSet`, and the worked example gains two of
  them, so that a token per zone and the proxy flag are both visible in it.
- regied acquires its first outbound connection to a remote service, over HTTPS. The host
  needs the CA certificates to verify it, and they are now among
  [ADR 0011](0011-target-platform.md)'s prerequisites. A host without them does not fail
  to boot or to route: the records go *failing* and say why.
- The apply order gains a phase after the processes, because a record is written from an
  address the kernel holds and nothing on the host depends on the write
  ([ADR 0004](0004-apply-model.md)). It is the only phase whose failure does not stop the
  turn: an unreachable provider leaves that record failing and lets the rest of the turn
  finish.
- What a turn does is no longer a function of the declaration and the host alone. It also
  depends on what this process has already written, which is why a restart writes every
  record once and why the report says which records it wrote.
- It is code regied writes rather than delegates: an HTTP client for one API. Keeping it to
  one provider is what keeps that proportionate.
- A record follows the address only while the resident process is running. A host whose
  daemon is stopped keeps whatever the record last held — which is the stop lever working,
  not failing.
- Resolving the published name from inside is unchanged: a client inside that gets the
  uplink's address is hairpinned (ADR 0015), and `DNSForwarder.staticHosts` is still how to
  answer it with the internal address instead.
