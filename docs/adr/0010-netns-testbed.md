# ADR 0010: Build the pseudo-WAN testbed on network namespaces, and keep the device under test replaceable

- Status: Accepted (2026-08-23)

## Context

What we need to know about a router is not whether the configuration parses. It is
whether packets get through: two uplinks picked per source range, a port forwarded
from outside to a host inside, the same forward reachable from inside against the
router's own global address, and everything else from outside dropped. Reading the
configuration file does not answer any of that.

Three constraints shaped the answer.

**The target device is not in hand.** The hardware regied will eventually run on
(an arm64 SBC) is not available yet, and the router that is in service cannot be
used for experiments. We assume a deployment with a single uplink, where taking
the router down is not an option.

**The implementation under test was not settled.** When the testbed was built it was
still open whether regied would be written at all or an existing implementation
adopted. A testbed that depends on one particular implementation is useless for
deciding between them.

**Tooling cannot be installed on the development machine.** Development happens
inside a container that has no `nft`, `dnsmasq`, `pppd`, or `pppoe-server`, and the
rule is to install nothing on the host or in that container.

## Decision

Build a pseudo-WAN out of network namespaces and run the device under test inside it.

### Topology

```
[client] --- [router (device under test)] --- [wan] --- [internet]
                                                |
                                   PPPoE server / DS-Lite AFTR
```

| namespace | role |
|---|---|
| `rg-client` | LAN hosts. Holds three source addresses that straddle the policy-routing ranges |
| `rg-router` | The device under test. Handed a LAN-side and a WAN-side interface, nothing else |
| `rg-wan` | The access network. Runs the PPPoE server and doubles as the DS-Lite AFTR |
| `rg-internet` | Reachability servers. Holds two addresses |

DS-Lite runs an ipip6 tunnel over IPv6 reachability, terminated on the AFTR side,
which then performs NAT44. The B4 side — the router — does not translate. That is
the division of labour DS-Lite actually has.

All addresses come from the ranges reserved for documentation (RFC 5737, RFC 3849).

### Assertions are made from the outside only

All seven checks are made from `rg-client` and `rg-internet`. Nothing looks inside
the device under test. A test that inspects the router's routing tables or its
nftables ruleset only holds for one implementation, so we do not write one.

**Which uplink a packet took is decided by the source address seen from outside.**
The address handed out over PPPoE and the address the AFTR translates to are
different, so a reachability server that echoes one line — the peer it saw — reveals
the path, the translation, and the external port at once. "Did it connect" cannot
tell one uplink from the other.

### The device under test is replaceable

Everything that assembles the router lives in one script, selected by the
`REGIED_NETNS_ROUTER_SETUP` environment variable. The contract is:

| item | contract |
|---|---|
| invocation | `up` to assemble, `down` to tear down. `up` returns once the link is established |
| what it is handed | `rg-router` containing one LAN-side and one WAN-side interface |
| topology values | addresses, ranges, ports and MTUs arrive as environment variables from the shared definitions |
| credentials | the PPPoE user ID and password are passed as file paths, never as values |
| expected result | a state in which the seven checks are observable from outside |

The default device under test is a reference router assembled by hand out of `ip`
and `nft`. It exists to prove that the testbed itself can judge the seven checks.
Any other implementation is put under test by writing one script that satisfies the
contract above; the tests do not change.

Credentials are passed as file paths for the same reason configuration never carries
secrets inline: the boundary with the device under test should not be the one place
where that rule is relaxed.

### Blocked and closed are told apart by time

Observing only that a connection fails cannot distinguish a firewall dropping the
packet from nothing listening. A test that a dead router passes is not a test.

A dropped connection attempt times out; an accepted one to a closed port is refused
at once, because the router answers with a reset. The check reads that difference
from the elapsed time and the failure it gets back.

### Execution is confined to a privileged sibling container

The tooling lives only in a dedicated image, started as a `--privileged` sibling
container. Nothing is installed on the host or in the development container. PPPoE
is a kernel module, so `/lib/modules` is passed in read-only at run time and the
module is loaded from within the container.

The tests sit behind the `netns` build tag so `go test ./...` does not pick them up.
`make test-netns` runs them directly where the tooling exists; `make test-netns-docker`
prepares the container and calls the same target. When a prerequisite is missing the
run is skipped with a reason — except under the container, where it fails instead.
A skipped check must never read as a passing one.

## Consequences

The testbed arrives before the thing it tests. Any candidate implementation can be
measured against the same seven checks, and once real hardware exists, every
configuration change can be put through them before it is deployed.

After the tests were written, each check was run against a deliberately broken
device under test to confirm it actually fails. A check that cannot fail says
nothing when it passes.

The replaceable seam has been exercised: an outside implementation was put under
test by adding one script on the device-under-test side, with no change to the tests.

**Results have differed by environment.** All seven checks passed under Docker
(Debian bookworm, kernel 6.8) while the NAT mapping check failed reproducibly on a
virtual machine (Ubuntu 26.04, kernel 7.0). The cause was not the router but the way
the response was read: socat 1.8 appends an empty datagram to signal EOF, the
responder answers that too, and two identical observations arrive together. The
reader assumed one line per read.

This testbed is the gate in front of real hardware, so a result that depends on the
environment defeats its purpose. **Responses from external commands are read in a way
that gives the same verdict however many pieces they arrive in.** That reading is kept
outside the build tag and covered by unit tests. Verification is done under both
Docker and a virtual machine.

Some things a pseudo-WAN cannot settle.

- **Kernel differences** — verification runs on an amd64 development kernel. The
  target is an arm64 vendor kernel, which is not the same thing
- **Access network behaviour** — the address handed out over PPPoE is fixed here.
  Reconnection, address changes, and differences between AFTR implementations are
  not reproduced
- **How IPv6 is obtained** — what a real link learns through RA and DHCPv6-PD is
  configured statically here. Receiving and delegating a prefix is not covered
- **DHCP and DNS** — dnsmasq's territory is not among the seven checks. It is in the
  image, so it can be added when it is needed

## Alternatives considered

**Nested Docker networks.** Run the router as a container and wire it up with Docker
networking. PPPoE and ipip6 need privileges either way, and it is more indirect than
handling namespaces directly, which makes routing tables and tunnel state harder to
inspect.

**Virtual machines.** Closer to real hardware, but slow to boot and slow to swap the
device under test. Too heavy for something used to make a decision.

**Wait for the hardware.** Anything first exercised on the day of the migration is
discovered on the day of the migration. There is no reason to postpone what can be
settled beforehand.
