# ADR 0019: Keep DNS records at the uplink's address

- Status: Accepted (2026-09-11). **Not built.**

> **This record decides that following an uplink's address into DNS records is in scope,
> and the direction it takes. Nothing here is built.** The schema in
> [`docs/spec/`](../spec/) and [`config/example.yaml`](../../config/example.yaml) are
> unchanged, because the spec describes what regied accepts, and it accepts none of this
> yet. The field names and the resource shape are settled when it is built, against the
> open questions at the end of this record. Read everything below as what is to be, not
> what is.

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

### Open questions, settled when this is built

1. **What an IPv6 record holds.** The uplink's own global address, or an address inside a
   delegated prefix for a host behind the router — the case where a published service's
   AAAA record names the server itself, because IPv6 has no NAT. The second is the more
   likely need, and it stretches "an address this host's uplink holds" into "an address
   this host routes", which this record has not decided.
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

## Consequences

- `docs/scope.md` is amended: dynamic DNS for this host's own addresses is the one
  exception to "no cloud provider integration", and it points here.
- The spec and the worked example do not change until this is built. What this record
  decides constrains the shape; it does not name the fields.
- regied acquires its first outbound connection to a remote service, over HTTPS. The host
  needs the CA certificates to verify it, which belongs with
  [ADR 0011](0011-target-platform.md)'s prerequisites when this is built.
- It is code regied writes rather than delegates: an HTTP client for one API. Keeping it to
  one provider is what keeps that proportionate.
- A record follows the address only while the resident process is running. A host whose
  daemon is stopped keeps whatever the record last held — which is the stop lever working,
  not failing.
- Resolving the published name from inside is unchanged: a client inside that gets the
  uplink's address is hairpinned (ADR 0015), and `DNSForwarder.staticHosts` is still how to
  answer it with the internal address instead.
