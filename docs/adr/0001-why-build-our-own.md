# ADR 0001: Build our own

- Status: Accepted (2026-08-23)

## Context

We want to move a router off a vendor OS that has stopped receiving updates and onto an
arm64 SBC. Doing that means replacing EdgeOS's own configuration language
(`config.boot`, a little over 700 lines) with a declarative model.

The target deployment looks like this: **there is exactly one uplink, and when it goes
down both daily life and work stop with it.** There is no redundancy, and there is one
machine.

Two existing implementations could have been adopted instead.

## Options considered

### Adopt imksoo/routerd

We measured it on the synthetic-WAN testbed (→ ADR 0010) at `v20260822.2333`. It can
express most of the seven areas we need to port, and **all seven verification items
passed**. The functionality is there.

We did not adopt it because of the following properties, measured on that release.
All of them are the kind of thing an upstream report would fix; none of them is a
statement about the quality of the implementation as a whole.

- **A dropped PPPoE session is never re-established.** The generated Linux `pppd`
  configuration carries neither `persist` nor `maxfail` nor `holdoff`; there is no
  reconnect loop, and nothing anywhere sends the daemon a connect command. We took the
  link down and watched for seven minutes without a recovery. With a single uplink, and
  with that uplink being PPPoE, this is the heaviest of the three.
- **Two configurations pass validation and then break silently.** Setting
  `PPPoESession.spec.ifname` makes the generated firewall rules reference a device name
  that differs from the one actually created, which takes out the entire PPPoE path.
  Using `NAT44Rule.egressPolicyRef` produces no masquerade rule at all, which removes
  NAT. `validate` accepts both.
- **The published schema is not a faithful map of the implementation.** Finding out what
  can actually be written means reading the source.

Every one of these can be worked around from outside the daemon, and we demonstrated
that. So adopting it is technically viable. But adopting it also means taking on the QA
of 280,000 lines.

### Adopt VyOS

VyOS is the direct descendant of the same lineage as EdgeOS (Vyatta), and it has
everything required. We did not adopt it because we do not like how its configuration
model is managed.

## Decision

Build our own. But **not because something is missing.** The goal is to keep whatever we
entrust our single uplink to small enough that one person can read all of it.

## Consequences

- Because keeping the size down is the whole point, **we do not replace implementations
  that already exist** (→ ADR 0008).
- We do not break state that other software owns (→ ADR 0009).
- We keep borrowing the resource-kind naming and schema idioms from imksoo/routerd as a
  design reference.
- The synthetic-WAN testbed built during the evaluation stays, since it is useful either
  way (→ ADR 0010).
