# ADR 0015: Keep uplink addresses in nftables sets, not in the ruleset

- Status: Accepted (2026-09-04). Built.

## Context

The hairpin half of a port forward has to match on the address the uplink is holding: a
client inside that resolved the service's public name reaches the router's own uplink
address, and only a rule naming that address can readdress the packet
([ADR 0013](0013-nftables-ruleset-shape.md)). That address is the one value the ruleset
depends on which the configuration cannot hold. ADR 0013 passes it in as an argument,
and [ADR 0004](0004-apply-model.md) gives the apply a last phase that re-reads the links
and re-renders the table when the address appeared during the apply.

Building the apply engine showed what that shape costs.

**It does not cover the cold start.** A session the process phase started has not
dialled by the time the last phase reads its link — a `Type=exec` unit returns as soon
as pppd is running, and a PPPoE dial takes seconds. On a host coming up, the ruleset is
installed without the hairpin rules and nothing installs them until the next apply, or
the daemon, which does not exist yet. ADR 0004 rejects waiting for the dial, for good
reason: an apply that blocks on a provider's timing holds a half-configured host. The
result is that the one moment a port forward is most needed — right after a reboot — is
the moment it is not there.

**It makes the ruleset a function of something that is not the configuration.** ADR 0013
says the engine's idempotence rests on the ruleset being a function of the configuration
alone. With the address rendered in, it is not: the same configuration renders differently
before and after the dial, the recorded text changes with the line, and every place that
compares rulesets has to know that.

**The re-render needs a rule about when not to run.** A link read a few milliseconds
after its session was restarted answers that it is not there. Rendering that would take a
working port forward away, so ADR 0004 adds a guard: an uplink that held an address and
answers none stops the re-render. The guard is right, and it exists only because the
address is in the text.

Every one of these is the same fact from a different side. **The address is runtime
state, and the ruleset is treating it as configuration.**

Two other ways to match on the address without rendering it were considered.

- `fib daddr type local` matches any address the router holds, the LAN one included. A
  client reaching the router's LAN address on the forwarded port would be sent to the
  target inside, which is not what a port forward on the uplink declares.
- Having pppd run `regied` from its hook, to re-render with the address. That keeps the
  address in the text and adds a second writer of the ruleset that races the apply.

## Decision

**The ruleset carries no uplink address. Each uplink has a named set per family, and the
hairpin rules match on the set.** The sets are declared in the table for every uplink the
configuration has — a PPPoE session or a DS-Lite tunnel, the kinds an `egressRef` may
name — whether or not anything hairpins through it. A set that is empty matches nothing,
which is exactly what a hairpin rule should do while the line is down.

The rendered text is a function of the configuration alone. It is the same before and
after the dial, the same on a host that has never dialled and on one that redialled ten
times, and it is what the apply records and compares.

**Whoever learns the address puts it in the set.** Three things do, and they do not
depend on one another.

- **The apply**, in the firewall phase, right after the table: it reads what each
  uplink's link is holding and what each set holds, and writes the sets that differ, each
  in one transaction that empties it and refills it. Replacing the table empties its
  sets, so an apply that replaces the table writes every set with something to hold; an
  apply that does not asks the kernel, and writes only what is wrong. An apply that finds
  every set right runs nothing, which is what keeps an apply safe to run from a timer
  ([ADR 0004](0004-apply-model.md)). A set that could not be read is left as it is and
  said to be, never taken for empty. The elements are not part of the recorded text and
  never decide whether the table has to be applied — and the text never decides whether
  a set is written; the dry-run shows the sets it would write, beside the ruleset.
- **pppd**, through a hook regied renders into `/etc/ppp/ip-up.d/` and
  `/etc/ppp/ip-down.d/`: on ip-up it adds the local address pppd was given to the set of
  the link that came up, on ip-down it deletes it. The link is named after the session
  ([ADR 0012](0012-networkd-rendering.md)), so the set's name follows from the interface
  name pppd hands the script, and one script serves every session. The hook succeeds
  whether or not the table is there: a line that comes up before the boot apply has
  nothing to add to yet, and the apply's own seeding covers it moments later.
- **The daemon**, when it exists, from the kernel's address events, for every uplink
  and both families. It is the general mechanism; the two above are what make a host
  correct without it.

Both directories are shared with the distribution's own hooks, so the scripts carry
regied's name prefix and the ownership marker, the way its units do in
`/etc/systemd/system` ([ADR 0009](0009-ownership-boundary.md)). They are reclaimed when
no session is declared.

**The last phase of the apply goes away.** There is nothing to re-render. A rollback
restores the previous table text and, in the same transaction, seeds the sets that text
declares with what the apply read from the links.

**`regied render` no longer takes an uplink address.** Nothing renders from it. The
rendering is complete without the host, which is what
[ADR 0006](0006-dry-run-and-rendering.md) wanted of it.

## Consequences

- **The cold start is covered for IPv4.** The table goes in during the firewall phase
  with empty sets; pppd dials during the process phase and its hook fills the set. A
  PPPoE port forward is an IPv4 port forward — the DS-Lite kind is rejected by
  validation, because the AFTR translates — so this is the case that exists.
- **An IPv6 hairpin on a PPPoE link is not covered by the hook.** pppd's `ipv6-up` hands
  the script a link-local address; a global one arrives through networkd, which has no
  hook. The next apply covers it, whether or not the ruleset changed, because the apply
  writes a set that does not hold what the link holds; a daemon, when there is one,
  covers the interval. This is where things stood before this record, narrowed to one
  family.
- **A redial with a new address is handled by pppd, not by regied.** ip-down removes the
  old element and ip-up adds the new one. The guard ADR 0004 needed against reading a
  link mid-redial, and the note the apply printed when it left the ruleset alone, both
  go, along with the code that carried them.
- **What the apply records is simpler.** The text is a function of the configuration
  again. A table somebody flushed is still noticed by the probe and the elements go back
  in with it; a set somebody emptied or left stale is noticed by the second probe and
  written on its own. The apply is the general repair, with or without a daemon.
- **The hooks are a fourth artefact regied puts on the host**, and the third shared
  directory it puts one in. The ownership rules are the ones already stated.
- **`nft list ruleset` shows the address, the dry-run does not have to.** The dry-run
  shows what elements this apply would add; the running host is where the current
  answer is, which is where ADR 0004 already put "is this running".
- **The netns testbed's reference ruleset changes shape**: its hairpin rule names the
  address, and the generated one names a set. The behaviour it verifies is the same.
- ADR 0004's last phase and its "re-render only ever adds" rule, and ADR 0013's "arrives
  as an argument" section, are amended by this record. Both carry a note.
