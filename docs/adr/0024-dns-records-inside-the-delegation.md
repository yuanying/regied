# ADR 0024: Keep AAAA records at addresses inside the delegation

- Status: Accepted (2026-10-23)
- Builds on [ADR 0021](0021-values-derived-from-the-delegated-prefix.md)
- Extends [ADR 0019](0019-uplink-address-in-dns-records.md), and amends
  [`docs/scope.md`](../scope.md) when built

## Context

[ADR 0019](0019-uplink-address-in-dns-records.md) put DNS records in scope for one
purpose — a name following the address this host's uplink holds — and refused AAAA with the
reason: the useful IPv6 record does not hold the router's address but an address inside the
delegated prefix belonging to a host behind it, which is *an address this host routes*, and
deciding that meant answering who is entitled to publish a name for another host's address.

The question is now forced by the same event as the rest of this series. A deployment that
publishes services over IPv6 keeps their AAAA records by hand, at the provider; when the
delegation changed, every one of them pointed into a prefix nobody held, and each had to be
found and rewritten. The router knew the new prefix the moment it arrived, and the host
parts are fixed numbers a person chose. Nothing else is needed to write the records.

A dynamic DNS client on each published host could keep its own record. It cannot keep a
record for a container, a load balancer's address or anything else that is not a host
running the client, and it would be one more copy of a token on every host.

## Decision

### What is in scope

**regied may keep DNS records holding an address inside a prefix this host was delegated,
or derived for itself from the advertisement it receives
([ADR 0023](0023-downstream-hosts-derive-their-subnet.md)), and the address this host
takes from that advertisement.** That is the whole of the widening, and it is narrower than
"an address this host routes": a static route to a literal subnet does not qualify.

The line [`docs/scope.md`](../scope.md) draws is about which host is managed, and it stays
where it is.

- **The prefix is this host's to know.** A router was handed the delegation by the
  provider; a downstream host derived its subnets from it, and its declaration is what
  decides which subnet and which host parts it uses. Publishing a name for one of those
  addresses is a statement about this host's own addressing, as an A record is a statement
  about this host's uplink.
- **No other host is managed.** regied does not configure the host or container behind the
  address, does not ask whether anything answers there, and takes nothing from it. Whether
  the address is in use is health, not drift
  ([ADR 0016](0016-converging-on-the-accepted-declaration.md)), and the record says only
  where the name should point.
- **No configuration comes from the provider**, as before.

The amendment to `docs/scope.md`'s exception paragraph replaces "a record names the uplink
it follows and holds an IPv4 address" with: a record either follows an uplink and holds its
IPv4 address, or holds an IPv6 address inside a prefix this host was delegated or derived
for itself, including its own address from an advertisement; regied does not check that
anything answers at that address, and each name is kept by one host. "No other host is
managed" stays as written.

### Which host keeps a name

**A name is kept by the host whose declaration decides its address.**

| The name points at | Kept by |
|---|---|
| The router itself, or something on the router's own segments that does not run regied — a cluster's load-balancer address, a host configured by something else | The router |
| A downstream host running regied, or a container behind its bridge | **That downstream host** |

The subnet ID and the host parts of a downstream host's addresses are written in the
downstream host's declaration ([ADR 0023](0023-downstream-hosts-derive-their-subnet.md)).
A record kept there names them through the same `interfaceRef` the bridge and the address
sets use, so which host a name belongs to and the address it holds are read in one file.
The router could keep the same record — `fromDelegatedPrefix` through its own delegation,
with the subnet ID and the token — and that would put the token in a third place and the
subnet ID in a second, beside the two ADR 0023 already accepts. It is not refused, since
nothing on the router can tell, but it is not the shape to reach for.

The cost is a credential on every host that keeps records: each needs a token that may
write its zone. A token scoped to one zone, which ADR 0019 already prefers, keeps that as
small as it can be.

**One name is written by at most one host.** Two regied instances writing the same name
would each put back what the other wrote, on every change, and neither can see the other.
Validation refuses two entries for the same name and type within one declaration, across
sets, as today; across hosts it cannot, and keeping the names apart is the configuration
repository's responsibility, where both declarations are — the same place ADR 0023 leaves
the subnet allocation to be checked.

### The shape

**A record entry is either an A record following an uplink, or an AAAA record holding a
derived address.** `type` takes `AAAA`, and an AAAA entry carries `address` instead of
`egressRef`. `address` holds exactly one of two forms:

| Field | A | AAAA |
|---|---|---|
| `type` | `A`, the default | `AAAA` |
| `egressRef` | Required | Refused |
| `address.fromDelegatedPrefix` | Refused | One of: `interfaceRef`, `subnetID`, `token` |
| `address.fromRouterAdvertisement` | Refused | One of: `interfaceRef` |
| `names`, `proxied`, `ttl` | As today | As today |

**`fromDelegatedPrefix`** is ADR 0021's form, with `token` required since a record holds an
address. Its `interfaceRef` names an interface running a prefix-delegation client on a
router, or one declaring `ipv6.advertisedDelegation` on a downstream host.

**`fromRouterAdvertisement`** is the address the named interface takes from the
advertisement it receives, with the token that interface's own `fromRouterAdvertisement`
address declares. It is how a downstream host publishes its own LAN address. The token is
not written again: the interface already holds it, and the record refers to the interface.
Writing the same address as `fromDelegatedPrefix` would need the LAN's subnet ID, which is
the router's choice and appears nowhere in the downstream host's declaration — writing it
there would be one more copy of the router's plan. The named interface must have a
`fromRouterAdvertisement` address; one that does not is a validation error.

`address` is a container rather than either form directly on the entry, so that the field
that says *what the record holds* reads the same whichever form it carries. An AAAA with a
literal address is refused: a literal record is one regied has nothing to keep up to date,
and the provider's own console is where it belongs. An AAAA following an uplink's own IPv6
address stays refused for ADR 0019's reason.

A name with both an A and an AAAA entry is two records and is fine.

### What carries over unchanged from ADR 0019

Ownership, the memory of what was written, the dry run, the token and the backoff are
ADR 0019's, word for word.

| Question | Answer |
|---|---|
| A record not at the provider | Created |
| A record leaving the declaration | Left at the provider; never deleted, no marker |
| When a record is written | When the resolved address is not what this process last wrote |
| `--dry-run` | Names each record and the address, asks the provider nothing |
| The token | Named by a file, read on the turn that writes, never shown |
| A failing write | That record is *failing*, under its own backoff; the turn finishes |

### What is new

**While there is no delegation, the record is left as it is** — on a downstream host, while
no advertised prefix has been read. It is an artifact not on this host
([ADR 0021](0021-values-derived-from-the-delegated-prefix.md)); the turn waits and names
the interface. A record pointing at the previous prefix for a minute is better than no
record.

**A prefix change rewrites every AAAA record at once.** That is what was asked for, and it
has three consequences.

- **TTLs expire together.** Resolvers that cached the old answer keep it for the TTL. The
  default stays `automatic`, which is short at the provider; a long TTL on an AAAA record is
  a choice to be unreachable for that long after a change, and the spec says so.
- **The writes come in a burst.** One write per record, in one turn. For the tens of records
  a home or small-office deployment has, that is far inside the provider's rate limit, and
  per-record backoff already handles a refusal: the rest are written, the refused one
  retries.
- **The host's own path out is not necessarily up.** If the change came with an outage of
  the path the host's own traffic takes — a tunnel that is about to be rebuilt
  ([ADR 0022](0022-dslite-local-address-follows-the-delegation.md)) — the first attempts fail
  and back off. ADR 0019 already sends the address in the request, so which path the request
  takes does not change what is written, only when it succeeds.

## Alternatives considered

**A dynamic DNS client on each published host.** Covers only hosts that can run it, puts a
token on each, and cannot keep a record for an address no host owns outright — a load
balancer's virtual address. Rejected.

**Folding the AAAA into the `IPAddressSet` or `Interface` that holds the address.** A record's
zone, token, proxy flag and TTL are about the provider, which is why ADR 0019 gave records a
kind of their own; nothing about the family changes that.

**Deleting AAAA records that leave the declaration.** The reason ADR 0019 gives for never
deleting applies more strongly here: a zone's AAAA records are often written by hand beside
the ones regied keeps, and regied would have no way to tell them apart.

**Keeping every AAAA on the router**, including the names of downstream hosts and their
containers. It was the first draft of this record: the router holds the delegation and the
fewest credentials. Rejected because the router would then hold the downstream host's
subnet ID and tokens once more, so that renumbering a container means editing a host that
does not run it; the credential a downstream host needs is the smaller cost.

**Checking that the address answers before publishing it.** That is a health probe with a
policy, the thing ADR 0016 keeps out of the loop; and it would make the record depend on a
host regied does not manage.

## Consequences

- **The hand-kept AAAA records of a deployment move into the declarations of the hosts whose
  addresses they hold** — the router for itself and for what does not run regied, each
  downstream host for itself and its containers — and follow a prefix change with no edit.
- **A host that keeps records holds a token for the zone**, downstream hosts included.
- **Keeping one name on one host is the configuration repository's to check.**
- **`docs/scope.md`'s exception grows by one clause**, and its line about hosts is unchanged.
- **`DNSRecordSet.records[].type` gains its second value**, through the field ADR 0019 kept
  for it.
- A record's address is shown in `--dry-run` beside the prefix it was derived from.

## Implementation units

Two units of [ADR 0021](0021-values-derived-from-the-delegated-prefix.md)'s list.

- **Unit 4, records on the router**: `type: AAAA` and `address.fromDelegatedPrefix` through
  an interface running a prefix-delegation client, in the spec and the validation, the DNS
  writer taking resolved addresses, and the scope amendment — in one pull request. It follows
  unit 1, which resolves the prefix, and needs nothing from units 2 and 3.
- **Unit 6, records on a downstream host**: `fromDelegatedPrefix` through an interface
  declaring `ipv6.advertisedDelegation`, and `address.fromRouterAdvertisement`. It follows
  unit 5, which gives the downstream host its prefix.
