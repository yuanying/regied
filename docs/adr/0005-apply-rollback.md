# ADR 0005: What a failed apply rolls back, and what it cannot

- Status: Accepted (2026-09-03)

## Context

The host regied is written for has **one uplink and no redundancy**, and regied runs on
it. An apply that stops halfway is not a tidy failure: it is a router that is half
configured, reached over the line it just half reconfigured.

[ADR 0004](0004-apply-model.md) already removes most of the ways that happens. Everything
knowable is checked before the first command runs, and the commit stage is six phases in
a fixed order. What is left is the case this record is about: a command in the commit
stage fails, and some phases have run and some have not.

The obvious shape — remember each action and undo it in reverse — does not survive
contact with what is being applied. Some of what apply does is a write that can be
reversed byte for byte. Some of it is an event that has already happened in the kernel,
and there is no operation that un-happens it.

## Decision

**Roll back by restoring the previous declaration and re-running it, not by undoing
individual actions.**

Before the commit stage begins, apply captures the previous generation of everything it
is about to change:

- the bytes and mode of every file it owns, as they are on disk;
- the text of its own nftables table as it last applied it, and whether the table is in
  the kernel at all;
- the current value of every kernel switch it is about to write.

If a command in the commit stage fails, apply puts those back and re-runs the reload and
restart commands of the phases it had already got through — the same commands in the same
order, over the previous configuration.

### What that promises

**Fully reversible.** The nftables table: one transaction, replaced by the previous text,
or deleted if there was none and the kernel said, before the apply, that it held no table.
A probe that could not be asked is not that answer, and is read as "present" here: a
failed question never licenses the deletion. The files regied owns, including the ones it
had reclaimed. The kernel switches, because each was read before it was written.

**With one exception, and it is the one that decides what "reversible" means here.** If
regied has no record of the ruleset it installed last time and the table *is* in the
kernel, there is nothing to put back. Deleting it is not the reverse of installing it: it
would take the firewall off a host that was running one, to recover from a missing note.
The table this apply installed is left in place and the rollback says so. The host is then
running a firewall it did not ask for in a rollback it did ask for, which is a mixture —
and a mixture that filters is better than a host that forwards with nothing in front of
it. This state is reachable because a failure to write that record is reported rather than
rolled back, which the section below is about.

**A rollback follows the same ordering rules the apply does.** Putting a file's content
back is always safe, because the file goes on existing and nothing that resolves through
it breaks. Taking a file away is not, and a unit this apply created is taken away only
after the stops that resolve through it have run, and systemd is told once it is gone —
the same steps ADR 0004 states for the forward direction, built by the same code rather
than written out twice.

**Reversible in effect, not in state: networkd.** Restoring the files and reloading
returns networkd to the previous declaration. It does not return the kernel to the instant
before. An address networkd removed was removed, and a link it took down and brought back
is a new link to everything that was watching it.

**Not reversible: a PPPoE session that was restarted.** Rollback restores the previous
peer file and restarts the session with it, and what comes back is a *new* session. It may
hold a different address, and the seconds it takes to dial are seconds the line is down.
There is no operation that un-drops a session.

This is the sharpest edge in the whole apply model, and two things in ADR 0004 exist
because of it. The session restart is the **last** phase, so a failure anywhere else never
reaches it. And a session is restarted **only when its own files changed**, so an apply
that touches the firewall or a DHCP reservation leaves the line alone entirely.

**Not reversible: what the new configuration did while it was live.** A lease dnsmasq
handed out under the new configuration stays handed out. A connection the new ruleset
dropped stays dropped.

### A failure before the first command costs nothing

That is the whole point of the staging stage. A configuration that will not render, a
credential that cannot be read, an AFTR name that will not resolve over IPv6, a ruleset
`nft --check` refuses — every one of those ends the apply with the host running exactly
what it was running, and rollback is the restoration of a handful of files that nothing
has read yet.

### Rollback is best-effort, and it says so rather than trying harder

If restoring fails too, apply does not retry and does not fall back to some third state.
It finishes the undo steps it can still run — abandoning the rest would leave more of a
mixture than finishing does — and then **stops**, reporting the original failure, every
rollback failure, and what it believes the host is now running: which files it restored,
which it did not, and which steps it could not undo.

The step that failed is undone along with the ones before it. A command that returned an
error may still have taken effect before it did, and the cost of undoing something that
never happened is smaller than the cost of leaving something that did.

On a host with one uplink, a loop that keeps re-running reloads and restarts is worse than
a stop with an accurate description. The operator is the recovery path, and what they need
is to be told the truth about where it stopped.

The same reasoning applies to the exit code and the log: a rolled-back apply is a failure,
not a partial success, and it exits non-zero.

### After the commit stage succeeded, a failure is reported, not rolled back

Once the last command has run, the host is running the new configuration. What is left is
regied's own bookkeeping — writing down the ruleset it installed, and rendering the table
once more in case a session dialled while the apply was running. Both can fail, and
neither is a reason to undo anything.

Rolling back at that point would take a working configuration off a host to recover from
not being able to write a note about it. Reporting a failed apply is nearly as bad in a
different way: the operator reads "failed", believes the change did not land, and acts on
that. So a failure here is reported as what it is — the configuration is applied, and this
one thing around it is not — and the exit status stays zero.

The rule in the previous section is unchanged and this is its boundary: **a rolled-back
apply is a failure and exits non-zero.** An apply that was not rolled back is not.

### Rollback covers one apply, and nothing wider

There is no "revert to the last known good configuration" and no generation history.
Rollback restores the state the host was in at the start of *this* apply and stops. Going
further back means applying an older configuration file, which is an ordinary apply of an
ordinary file, and the file is in version control where it belongs.

This is also why apply keeps almost no state: for a file, the previous generation *is* the
file on disk, so there is nothing to store. The one exception is the nftables ruleset,
which is kernel state and has no file, and which ADR 0004 already records for its own
reasons.

## Consequences

- The safety requirement in the task — "a failed apply rolls back, so it does not stop
  halfway and leave the line down" — is met for every layer except a session restart, and
  for that one the ordering reduces the exposure to the case where the session's own
  configuration changed.
- The engine has to hold the previous bytes of every file it writes for the duration of an
  apply. That is a bounded amount of text and it is the price of not having to model an
  inverse for every action.
- A rollback is exercised in the unit tests the same way an apply is, by failing a command
  in the fake runner at each phase in turn and asserting what the host is left holding.
- Reclaimed files are part of the previous generation, so a rollback puts back a file an
  apply had deleted. Reclaim is therefore never a truncation or an in-place removal that
  loses the content before the apply has committed.
- **The window an operator has to worry about is one phase wide.** Between a command
  failing and the rollback finishing, the host is running a mixture. Making that window
  smaller is a matter of having fewer commands, which is what the idempotence rule in
  ADR 0004 is for: an apply that changes nothing runs nothing, so most applies have no
  window at all.
