# ADR 0025: Solicit the delegation again when the PPPoE session returns from another router

- Status: Accepted (2026-10-23)
- Amends [ADR 0015](0015-uplink-addresses-in-sets.md) and
  [ADR 0019](0019-uplink-address-in-dns-records.md) on what a pppd hook does

## Context

On a line where a PPPoE session and the DHCPv6 prefix-delegation client run over the same
upstream interface, the provider can move the line to another of its routers. The PPPoE
session notices at once: it drops, and pppd redials, reaching the new router. The DHCPv6
client does not notice at all. The upstream interface never lost carrier, so networkd keeps
the lease it holds, for a prefix the new router does not route, until the lease's own
timers bring it to renew or let it expire — hours, on a typical line. Until then every
address derived from the delegation is unreachable, and with
[ADR 0021](0021-values-derived-from-the-delegated-prefix.md) in place every value derived
from it is faithfully wrong.

The session dropping is the only signal the host gets that the upstream may have changed.
It is not proof: a session also drops for reasons that leave the upstream as it was — the
provider's maintenance on the same router, an LCP echo timeout, a redial by hand.

### What networkd 257 offers

Checked in the 257 sources:

| Command | What it does to the DHCPv6 client |
|---|---|
| `networkctl renew` | **Nothing.** The D-Bus method behind it renews the DHCPv4 lease only |
| `networkctl reconfigure` | Stops the DHCPv6 client, drops the dynamic configuration it brought — the delegated prefix and the addresses derived from it on downstream links — and applies the `.network` again, which starts the client and solicits afresh. It does not take the link down, so a PPPoE session on the same interface keeps its carrier |
| `networkctl down` / `up` | Takes the link down, and the PPPoE session with it |

Measured on a lab machine running Debian 13: of the planned operations, a reboot and a
reconfigure of the upstream interface are the ones that send a RELEASE, and with
`[DHCPv6] SendRelease=no` neither does. The same measurement showed that **a reconfigure
withdraws the downstream links' delegated addresses for a moment even with
`SendRelease=no`**: what keeps the delegation is the provider's binding, not the addresses
on this host while the client restarts.

### What a reconfigure costs, and who pays

The withdrawn addresses are not only IPv6's. A router that carries its LAN's IPv4 over a
DS-Lite tunnel sends that tunnel from one of them
([ADR 0022](0022-dslite-local-address-follows-the-delegation.md)), and the tunnel does not
depend on the PPPoE session at all. A PPPoE session dropping on its own is an outage of the
PPPoE uplink only; reconfiguring on every redial would turn it into an outage of the LAN's
IPv6 and of the IPv4 the tunnel carries as well. **The cost does not land inside the
redial's own outage, and it must not be paid on every redial.**

### What the hook can see

pppd's PPPoE plugin (ppp 2.5) puts what it learned in discovery into the environment of the
scripts it runs, alongside pppd's own variables:

| Variable | What it holds |
|---|---|
| `MACREMOTE` | The MAC address of the access concentrator the session was opened with |
| `ACNAME` | The access concentrator's name, when it sent one |
| `REMOTENUMBER` | The same MAC address, as pppd's generic "remote number" |
| `IPREMOTE`, `IPLOCAL` | The IPCP addresses of the two ends |

When a provider moves a line to another router, the access concentrator answering the
discovery is that other router, and its MAC address differs. That is the observation that
told the move apart from a plain redial when it happened.

## Decision

**When a PPPoE session comes up, the ip-up hook regied renders compares the access
concentrator's MAC address with the one the session last came up with, and asks networkd to
reconfigure the upstream interface only when it differs.** The new upstream is answering by
then — the session just came up through it — so the Solicit goes to a router that can reply.

| Case | What the hook does |
|---|---|
| The MAC differs from the one recorded | Records the new one, then runs `networkctl reconfigure` on the interface the session runs over |
| The MAC is the one recorded | Nothing beyond what it already does |
| Nothing is recorded — the first ip-up since boot | Records the MAC and does nothing else. After a boot the DHCPv6 client has just solicited, so there is no old lease to drop |
| `MACREMOTE` is absent or empty | Nothing. A reconfigure costs the LAN an outage; a missed move costs what it cost before this record, and the monitoring that watches for a stale prefix still sees it |

- **The MAC, not the name.** `ACNAME` is whatever the provider configured, and a provider
  may give the same name to several of its routers or send none; the MAC identifies the box
  the session terminates on. A provider replacing hardware in place changes the MAC and gets
  one needless reconfigure, which is the cheap direction to be wrong in. `IPREMOTE` is not
  used either: it identifies the far end of the IPCP link, not the router, and may change
  from one session to the next on the same router.
- **The record lives under `/run`**, in regied's own directory, one file per session named
  after the session and written whole. `/run` is emptied at boot, which is exactly when the
  comparison has to start again: the DHCPv6 client starts fresh with the host. It survives a
  pppd that is restarted on its own and the resident process being stopped, which are the
  cases where networkd's lease outlives the session. It is not configuration and not part
  of the accepted declaration ([ADR 0016](0016-converging-on-the-accepted-declaration.md));
  it is the hook's memory.
- **Only where the session and the client share an interface.** `PPPoESession.interfaceRef`
  and the interface's `dhcpv6.prefixDelegation` together say that the two share an
  upstream, and no field is added. Where they do not, the hook records nothing and does
  nothing extra.
- **In the hook, not in the loop.** The session coming back from another router is an
  event, and what to do about it does not depend on comparing anything with the
  declaration; a level-triggered turn has nothing to compare. The hook is also the path that
  works with the daemon stopped, which is the property a prefix change most needs, because
  nobody is watching when it happens.
- **On ip-up, not ip-down.** At ip-down the line is gone and the next router is not known.
- **Its failure is ignored by the session.** A reconfigure that cannot run must not stop the
  session coming up. The next turn sees the prefix networkd holds, whatever it is.
- **`SendRelease=no` is expected beside it.** A reconfigure that sends a RELEASE gives up the
  binding at the router that just started answering; without one, a router that kept the
  binding hands the same prefix back.

[ADR 0015](0015-uplink-addresses-in-sets.md) and [ADR 0019](0019-uplink-address-in-dns-records.md)
said the hooks do one thing, putting an address in a set, and that anything needing the
network, a token or a backoff is a turn's work. This keeps the second half and widens the
first: **a hook may run a local command that needs no network, no credential and no
backoff, and keep a small record of its own under `/run`.** A request to networkd over its
bus qualifies; a DNS write still does not.

### To confirm before it is built

On the testbed, once it can delegate prefixes (unit 7 of
[ADR 0021](0021-values-derived-from-the-delegated-prefix.md)): that the plugin sets
`MACREMOTE` in the ip-up environment of the pppd the platform ships; that a PPPoE session on
the same interface survives the reconfigure; that a redial to the same concentrator leaves
the derived addresses alone; and that one to a different concentrator, whose DHCPv6 server
did not keep the binding, brings a new prefix that the next turn follows.

## Alternatives considered

**Reconfiguring on every ip-up.** The first draft of this record. Rejected: it turns every
PPPoE redial into an outage of the LAN's IPv6 and of the DS-Lite tunnel, which do not depend
on the session, and most redials do not change the upstream.

**Comparing the delegated prefix instead of the concentrator.** The hook cannot learn the
prefix the new router would delegate without soliciting, which is the act being decided.

**Doing it outside regied**, as a script the deployment drops into `/etc/ppp/ip-up.d/`. It
works, and it has to know which interface runs the delegation client — which the declaration
already says. Rendering it keeps that fact in one place, in a directory regied already
writes hooks into under its own name.

**Doing it in the resident process**, on the PPPoE link's address event. Rejected: it is
edge-triggered work in a loop that is deliberately level-triggered, it withdraws addresses
that are up from an unattended turn, and it does not happen with the daemon stopped.

**`networkctl renew`.** It does nothing to DHCPv6 on 257.

**Waiting for the lease.** The status quo: correct within the lease's valid lifetime, which
is the outage this record is about.

## Consequences

- **A provider moving the line to another router is followed within seconds of the session
  returning**, instead of within the lease's lifetime.
- **A redial to the same router costs nothing beyond the redial.** The LAN's IPv6 and the
  DS-Lite tunnel are untouched.
- **A move is paid for with a few seconds of the derived addresses**, the tunnel's local
  address among them.
- **The pppd hooks run one more kind of command and keep one small file**, and the rule for
  what a hook may do is written down rather than implied.

## Implementation units

Unit 8 of [ADR 0021](0021-values-derived-from-the-delegated-prefix.md), after unit 7 has
given the testbed a delegating DHCPv6 server and the checks above pass. It is independent
of units 2 to 6: it makes the prefix arrive sooner, and they make the host follow it.
