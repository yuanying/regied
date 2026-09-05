# ADR 0017: A submission goes through the resident process, which is the host's only writer

- Status: Accepted (2026-09-05). **Not built.**

> **Nothing in this record is implemented.** What runs on a host today is ADR 0016 as
> built: `regied apply` validates, stages, writes the record and runs its turn in its own
> process; only a trial travels over the control socket; `regied reconcile` exists, and a
> boot unit runs it. This record decides the shape that replaces that. The code follows in
> a later unit, the same two-step way [ADR 0015](0015-uplink-addresses-in-sets.md) and
> [ADR 0016](0016-converging-on-the-accepted-declaration.md) were carried, after the netns
> acceptance and the operational README are closed and before the first real host.
>
> ADR 0007's HTTP API stays deferred, and nothing here revives it.

## Context

### Two submission paths, and up to three writers

ADR 0016 got the design right and built it in halves that do not quite meet. A plain
`regied apply` reads the file, validates and stages the declaration, writes the record,
takes the turn lock and runs the turn — all in the `apply` process. `regied apply
--confirm` does the first half in the `apply` process and then hands the declaration to
the resident process over the control socket, which runs the turn as a trial and holds
the clock. `regied reconcile` is a third entry point that runs a turn in yet another
process, and the boot unit runs it.

So the host is written by up to three processes — an apply, a reconcile, the daemon —
serialised by a file lock. They run the same engine, so the result is the same whichever
of them runs; the code is not duplicated. What is duplicated is **the position of being
the writer**, and that costs something concrete.

### What it costs

**Runtime state that only the daemon knows cannot survive a turn it did not run.**
[ADR 0013](0013-nftables-ruleset-shape.md) replaces regied's table in one transaction,
which empties every set and map in it. An uplink set costs nothing, because the kernel
holds its answer and every turn re-seeds it (ADR 0015). ADR 0007 and ADR 0016 both closed
with the same warning: an element whose source is not the kernel has no such recovery, and
each replacement of the table opens a window that stays dark until whatever knows the
answer writes it again. The case both records name is the backends behind a load-balanced
address, learned from a cluster's API — something the daemon can hold and an `apply`
process cannot. As long as an `apply` process can replace the table, that window opens on
every submission, and the daemon can only shorten it by noticing afterwards.

With one writer the window does not exist. The turn that replaces the table is the
daemon's turn, and the same turn seeds every element the daemon knows, in the same place
phase 1 seeds the uplink sets today. That direction — regied as the provider behind a
cluster's load-balanced addresses — is one the task keeps open, and it is what prompted
this record. Nothing here adds the feature; what this record does is stop closing the door
to it on every apply.

**Two paths are two things to keep equal.** Today a trial reaches the daemon and a plain
submission does not; the record is written by one process and read by another; "a plain
apply during a trial ends it" is implemented as the daemon noticing on its next
comparison. Each is small. Together they are a protocol between two processes that both
write, and the ordinary way such a thing goes wrong is one side learning a rule the other
did not.

**A submission is tied to the operator's session.** The turn runs in the `apply` process,
which dies with the ssh session that started it. A change that takes the operator's path
away — the case ADR 0016 built the deadline for — can also kill the turn half way, and
the loop then finishes only the half it is allowed to.

### The Kubernetes shape, completed

ADR 0016 drew the correspondence and stopped one row short. In Kubernetes, `kubectl apply`
does not write etcd and does not run a controller. It sends the manifest to the API
server, which validates it, stores it, and answers; the controllers are the server's. The
client is thin because everything it could do in its own process is a second
implementation of something the server must do anyway.

| Kubernetes | regied after this record |
|---|---|
| `kubectl apply` sends a manifest to the API server | `regied apply` sends the declaration's bytes to the resident process |
| The API server validates, stores, answers | The resident process validates, stages, writes the record, runs the turn, answers |
| Controllers live in the control plane and nowhere else | The loop and every turn live in the resident process and nowhere else |
| A manifest that fails admission is never stored | A declaration that fails validation or staging is never recorded |

### What was settled before this record

Three things were decided by the user on 2026-09-05, and this record does not reopen
them.

- The submission path is one path, through the resident process.
- `regied reconcile` and the boot unit `regied-reconcile.service` are removed. The
  resident process is the host's only writer, and its first turn is the boot-time
  convergence.
- The HTTP API stays deferred. The socket is a submission path, not a read API.

Whether the client half becomes its own binary, and what it would be called, is
deliberately not decided here.

## Decision

### `regied apply` is a client, and the resident process is the only writer

**`regied apply` reads the configuration file and sends its bytes to the resident process
over the control socket. It writes nothing on the host and runs no turn.** The resident
process validates the declaration, stages it, writes the record, runs a submitted turn
toward it, and answers with what the turn did and where it left the host. The client
prints that answer and exits with the status the state calls for.

Everything ADR 0016 says about a submission stays true, and now stays true in one place.

- **The record is written at submission, once the declaration validates and stages and
  before the first command runs.** The process that writes it is now the process that
  reads it.
- **`regied apply` is the only thing that reads a configuration file.** The daemon reads
  the record and the bytes a client sends it. It has no configuration-file flag, and never
  will.
- **The record holds what was asked for.** A submission whose commands fail leaves the new
  declaration in the record and the host at the safe prefix it reached, and the loop works
  toward the rest under ADR 0016's rules.
- **A trial writes no record**, and the daemon holds it in memory for the length of the
  deadline.

The validation the daemon performs is the one `apply` performs today: the document, the
schema, and the files the declaration names. A credentials file that cannot be read fails
the submission before anything runs, as it does now. The client does not validate; it may
parse the file to fail fast, but the daemon's answer is the answer, and a declaration the
daemon refuses is refused.

**Five verbs.** `render` and `apply --dry-run` read no daemon and stay that way: they are
readers, and [ADR 0006](0006-dry-run-and-rendering.md)'s split between what reads the host
and what reads nothing is untouched. `apply` needs the daemon. `serve` is the daemon.
`confirm` and `cancel` are the two messages ADR 0016 gave the socket.

| Verb | Reads | Needs the daemon |
|---|---|---|
| `render` | the file | no |
| `apply --dry-run` | the file, and this host read-only | no |
| `apply` | the file; the daemon reads this host | **yes** |
| `serve` | the record and this host | is the daemon |
| `confirm`, `cancel` | the socket | yes |

**`reconcile` is gone.** It was the one way to ask for a turn that reads no file; every
turn now reads no file, and the daemon runs one every minute. What a person ran
`reconcile` for — put the host back where it should be without submitting anything — is
what the loop does unasked, up to the line ADR 0016 draws for an unattended turn. The
other half, past that line, is a submission, and the way to ask for it is unchanged:
`regied apply` of the file. Re-running the same apply is idempotent and does the remaining
steps and nothing else ([ADR 0004](0004-apply-model.md), ADR 0016).

**When the file and the record differ, and it is the record the operator wants re-run
with a person's authority,** the answer is still `apply`: the recorded declaration is a
file whose bytes are an ordinary declaration, and pointing `apply` at it is an ordinary
submission of the same revision. No socket message is added for it. If operating the host
shows this is asked for often enough to deserve a verb, one can be added — a submission
carrying no bytes, meaning the record — without changing anything else here, because the
socket is versioned by its verbs. The reason not to add it now is
[ADR 0002](0002-configuration-schema.md)'s: an interface added before anyone needs it is
the one that is hard to withdraw.

### The socket carries four messages, and it is the submission path

To the three messages ADR 0016 gave the socket — trial, confirm, cancel — this record adds
**submit**. Four, and nothing else.

| Message | Carries | The daemon | The record |
|---|---|---|---|
| submit | the declaration's bytes, and where they were read from | validates, stages, writes the record, runs a submitted turn, answers | written before the first command |
| trial | the same, and a deadline | validates, stages, holds the declaration as the trial, starts the clock, runs a submitted turn, answers | untouched |
| confirm | nothing | writes the trial to the record, stops the clock, answers | written |
| cancel | nothing | drops the trial, runs a submitted turn toward the record, answers | untouched |

**submit and trial are two messages on the wire, and one path inside the daemon.** The
framing this record was given — a trial is the same path with a deadline attached — is
exactly how the daemon treats them: one sequence, validate and stage, then either write
the record or hold the trial, then the same turn. The alternative, one message with an
optional deadline, was weighed and rejected on one ground: the difference between the two
is whether the host is left with a way back, and **that must never be decided by the
absence of a field.** Under a single message, a trial request that arrived without its
deadline would become a permanent submission with no net, at the moment the operator was
relying on one. As two messages, a trial without a deadline is malformed and is refused.

**The line ADR 0007 and ADR 0016 drew is re-drawn here, in these words: the socket is the
path a declaration takes to the host. It is not a way to ask the host questions.** Both
earlier records said the socket carries no status, no apply and no dry-run. Adding submit
makes "no apply" wrong as a sentence, so the line has to be stated as what the socket is
for, and the list follows from it.

What the socket does not carry, and this record does not leave open:

- **No question about the host that nothing just asked it to change.** No status, no
  report, no readiness, no health. The report of the last turn is a file, and the journal
  is the journal (ADR 0016). A reply to a submission describes the turn that submission
  caused; nothing describes a turn somebody else caused.
- **No preview.** No dry-run and no render. Both read no daemon, and ADR 0006 deliberately
  kept the dry-run's output for a person at a console rather than making it a machine
  interface.
- **No reload, no resource operation, no CRUD.** The declaration is the unit of
  submission; there is no smaller one.
- **No leases, no conntrack, no link state, no logs.** Those are the read API's, and it is
  deferred.

**The reply carries the same account the console gets today.** This is what keeps the
README's failure procedure true — it says the console tells the operator which step
failed, and it still will. A reply names the revision; the state (converged, waiting,
failing); the outcome; what the turn changed, by phase; what it is waiting on; what
failed and where — phase, step and cause; the renderers' warnings; and the notes. That is
the content of the report of the last turn, which is already a structured file. It is not
the plan of a dry-run, which ADR 0006 keeps unstructured, so the two decisions do not
collide.

### Who may reach the socket is who may reconfigure the host

ADR 0007 designed its socket for a read-only API and gave it to root and one group;
ADR 0016 kept that mode for confirm and cancel, on the ground that reaching the socket adds
no capability to somebody who can already run `regied` on the host. With submit on it
that ground is gone: connecting to the socket is now exactly the capability of changing
the host's network, and it must be given only to whoever has that capability already.
**The socket is root's. The package sets no group on it.** An operator who opens it to a
group has granted that group root over the host's network and should do so knowing it;
regied does not stop them, and does not do it for them.

### The result comes back synchronously

**A submission waits for its turn and returns its result.** It does not return once the
daemon has accepted the declaration and leave the operator to read the report.

- **The person is there.** ADR 0016's whole argument against automatic rollback is that a
  submission is a command somebody typed, with the failure landing on their console in
  the same second. A submit-and-poll design would spend that.
- **A turn is bounded.** ADR 0004 refuses to wait for a dial or a resolver, and a turn is
  the commands it runs and no more, so waiting for it is waiting for something that ends.
- **The README's procedure reads the console.** Which step failed, and what the host is
  running now, is printed by the command the operator ran. This record keeps that as a
  requirement, not a habit.

**The turn does not belong to the connection.** If the client goes away mid-turn — the
operator interrupts it, the ssh session drops, the change itself cuts the operator's path
— the daemon finishes the turn, writes the report, and logs the result. The client, when
it still can, says that the connection was lost and that the outcome is in the report and
the journal. What the client cannot know when its connection drops is whether the record
was written; the report is what answers that.

This is a property the current design does not have, and the deadline mechanism benefits
from it directly: a change that takes away the operator's path no longer takes half the
turn away with it.

### One writer, one lock, and the same line between turns

**Turns no longer have to be serialised across processes, because one process runs them.**
The daemon runs turns one at a time, as it does today: a submission that arrives while a
resync turn is running waits for it, a tick that arrives while a submission is running is
delayed, and a trial's expiry waits its turn.

**The lock under the state directory stays, and changes meaning.** It was held by
whichever turn was running; it becomes **the daemon's, held for the life of the process.**
A second `regied serve` finds it held and refuses to start, saying so, rather than taking
the socket over from the first. Nothing else takes the lock: a dry-run reads the host
without it, as today, and there is no other writer to exclude. A lock that asserts "there
is one writer" is worth keeping precisely because the design now depends on that being
true.

**The tiers ADR 0016 drew — a submitted turn may restart and stop, an unattended turn may
not take down what is up — are unchanged, and so is the line: it is about who asked for
the turn.** Now that both kinds run in one process, the distinction is a property of the
turn and not of the process running it, which is already how the engine represents it.

| What caused the turn | Kind |
|---|---|
| A submit message | submitted |
| A trial message | submitted |
| A cancel message, or a trial's expiry | submitted — the operator asked for it when they set the deadline |
| A confirm message | none: confirming writes the record and runs no turn |
| The periodic resync | unattended |
| A kernel address event | unattended |
| **The daemon starting** | **unattended** |

The last row is the one this record has to get right, because it is where the removed
boot unit's authority would otherwise have gone.

### At boot, the daemon's first unattended turn is the convergence

`regied-reconcile.service` is removed. `regied.service` is the one unit, and the first
turn the daemon runs on its way up is what installs the ruleset and everything else after
a reboot.

**Why the first turn is enough at boot.** After a reboot nothing regied declares is up:
the table is not in the kernel, the switches are at their defaults, no session has
dialled, dnsmasq is not running. ADR 0016's line for an unattended turn — never take down
something that is up — binds nothing on such a host, because nothing is up. Everything a
boot has to do is on the allowed side of the line: install the table, write the switches,
write the files, start the units. The boot unit ran a submitted turn; on a host that has
just booted, a submitted turn and an unattended turn do the same things.

**Why the first turn after a restart is unattended too, and must stay so.** `systemctl
restart regied` is a thing operators do — after an upgrade, after editing the unit, out of
habit. If the daemon distinguished a boot from a restart and gave its first turn a
submitted turn's authority, every restart would be entitled to restart a running PPPoE
session whose options file an earlier, half-finished submission had rewritten — and the
line would go down on a `systemctl restart` nobody thought of as an apply. The daemon
cannot reliably tell a boot from a restart, and it does not try: every start is the same,
and the first turn is unattended. Where a running host needs the other half, a person asks
for it with `apply`, as ADR 0016 already says.

**One thing a boot no longer does.** A submission that failed part way and then met a
reboot — before it reclaimed a unit a stopped session ran from, say — used to be finished
by the boot turn, which had a submitted turn's authority. Now it waits for a person, like
every other half of a submission the loop is not allowed to do, and the report says so.
This is the disposition ADR 0016 gives the same case without the reboot, and it is
preferred over giving a restart the power to take the line down.

**What is lost, and what stands in for it.** A oneshot unit that fails is red in
`systemctl status regied-reconcile`, and that was a real signal. It is replaced by two
things that already exist and one that is new.

- The journal under `regied.service` says when the state changed and to what, with what
  failed, and it says it at the first turn like any other (ADR 0016).
- The report of the last turn is on disk and says the same.
- **The daemon puts its state in the status line systemd shows for it**, so that
  `systemctl status regied` says converged, waiting or failing, and what it is waiting on
  or failing at, without anyone opening the journal. Whether the daemon also reports
  readiness to systemd after its first turn — which would give the boot a definite point
  at which the host was configured, the other thing ADR 0016 valued the separate unit for
  — is left to the unit that builds this, with the note that no unit regied ships depends
  on it.

**Stop, never disable.** ADR 0016 gave the separate boot unit a second justification: the
stop lever on the daemon is also a disable lever, and an operator who disabled the daemon
during an incident and then rebooted still needed a firewall. With one unit, disabling it
is choosing to have no convergence at boot either — and on such a host the session units
regied wrote still dial at boot, because they are systemd's to start (ADR 0004), with no
table in front of them. This is the host an operator got today by disabling both units,
and this record does not make it more likely; but the outage procedure has to say *stop,
never disable*, and the README owns that sentence.

The ordering of `regied.service` is what ADR 0007 decided for both units and ADR 0016
kept: after `systemd-networkd.service`, after a distribution `nftables.service` if one is
enabled, never before `network-online.target`. It is shipped by the package and not
rendered by regied, for ADR 0007's reasons.

### Without the daemon, `apply` is refused

**`regied apply` on a host where the resident process is not running is refused before
anything is read from the host, and says what to do: start `regied.service`.** It does not
fall back to running the turn itself.

The fallback is the tempting thing, and it is exactly the second writer coming back
through a side door — exercised on precisely the hosts with the least supervision, and
carrying the whole apparatus this record removes. A daemon that is not running is not an
emergency: the host keeps running what is on it (ADR 0016's stop lever), and the one
command that starts the daemon is the command the refusal names.

**On a new host the daemon starts first, and the first `apply` configures it.** A daemon
with no record does nothing and says so (ADR 0016), and it listens; it is safe to start
before anything has been submitted. Installation becomes: install, enable and start
`regied.service`, then `regied apply`. Today it is apply, then enable both units. That
change is handed to the README.

What "not running" looks like from the client is no socket at the control path, or a
socket nothing is listening on. Both are refused with the same message. A socket that
something *is* listening on but that does not answer is a different failure, and the next
section has it.

### Where each failure leaves the record and the host

ADR 0016's invariant is that every way of losing the process resolves toward the previous
declaration, and that a trial never survives the daemon. Both hold, and the table is the
argument.

| Failure | The record | The host | What the operator sees |
|---|---|---|---|
| No socket, or nothing listening | untouched | untouched | `apply` refuses and says to start `regied.service` |
| The daemon accepts the connection and never answers | unknown from the client; the report says | whatever the turn did | the client waits; when interrupted it says where to look |
| The daemon dies during validation or staging, before the record | the previous declaration | the files staging wrote, if any; the next turn writes the previous declaration's back, a file write being on the allowed side of the line | the connection drops; the client says the outcome is in the report |
| The daemon dies after the record, during the commands | the new declaration | the safe prefix the turn reached; the daemon comes back and converges unattended, reporting what needs a person | the same |
| The daemon dies during a trial | the previous declaration | converged toward it on the way back up (ADR 0016) | the same |
| The client dies mid-turn | as the turn left it | the turn finishes | nothing, until they read the report |

Two rows deserve their reasons.

**The daemon dying between staging and the record** is the one case where files of a
declaration that was never accepted are on the host. It resolves correctly for a reason
ADR 0016 already gave: rewriting a file is on the allowed side of the unattended line, so
the daemon's first turn after restart writes the previous declaration's files back, and
nothing that reads them was restarted in between. The previous declaration wins, which is
the direction the invariant requires.

**The client dying mid-turn** is the row that improves. Today the `apply` process is the
turn; losing it — the ssh session dropping, the change cutting the operator's path —
stops the turn where it was. After this record the turn is the daemon's, and it finishes.
The submission the operator can no longer see is completed, recorded, and reported.

The daemon is restarted by systemd on failure, as its unit already says, and its first
turn after any start is unattended, as decided above.

### The netns acceptance keeps its shape

The acceptance for the router-in-a-VM context ([ADR 0010](0010-netns-testbed.md)) runs
`apply` against the host and starts `serve` afterwards, to check that the loop takes over.
After this record the order is the other way round, and it is the only thing about the
acceptance that changes: **`serve` is started first, with its control path, and `apply` is
run with the same control path once the daemon is listening.** `apply` still blocks until
the turn is done and still prints the same states with the same exit status, so the check
the acceptance makes right after it — that the management path still answers, and
removing the table if it does not — depends on nothing this record moves. The seven
checks that follow are made from outside the router and do not know how it was
configured. What the acceptance has to tolerate, and today does not, is the daemon logging
that no declaration has been accepted between its start and the first `apply`; that line
is the right answer from a daemon on a host that has not been configured yet (ADR 0016).
The reorder is handed to the implementation unit together with the code, so that the
acceptance and the binary change in the same pull request.

The unit tests keep their shape too. The daemon's handling of a submit message is the
same engine behind the same interfaces, and the client is a function of a socket and a
file, both of which are already stood in for.

### What this record does not decide

- **Whether the client is a separate binary**, and what it is called. The client needs
  the socket and the file, and none of the engine; splitting it is packaging, and nothing
  here has to change to do it.
- **The wire encoding**, beyond the four messages and what a reply carries.
- **The load-balancer elements** whose window this record closes. No kind, no field and no
  element is promised. What is promised is that when they exist, they are written in the
  same turn that replaces the table, by the only process that can.

## Alternatives considered

**Keep two paths, and have `apply` tell the daemon afterwards.** The daemon re-seeds what
it knows when told, and the window becomes the notification's latency. Rejected: the
window is still there, the protocol between two writers is still there, and it has one
more message in it.

**Keep `reconcile` as a fallback for a host with no daemon.** This was the shape agreed
before the user decided to remove it, and the argument against it is theirs: the fallback
is the same binary, so a host on which the daemon cannot run is a host on which
`reconcile` cannot be relied on either; and a fallback that is a second writer is the
thing this record exists to remove. What `reconcile` was for at boot, the daemon's first
turn does; what it was for by hand, the loop does unasked and `apply` does on request.

**Give the daemon's first turn a submitted turn's authority**, so that a boot finishes a
half-done submission. Rejected because the daemon cannot tell a boot from a restart, and a
restart with that authority redials the line.

**One message with an optional deadline** instead of submit and trial. Rejected above:
whether the host keeps a way back must not depend on a field being present.

**Submit and return, and let the operator read the report.** Rejected above: the person is
at the console, the turn is bounded, and the README's procedure reads the console.

**Run the turn in the `apply` process when the daemon is absent.** Rejected above: the
second writer through a side door, on the least-supervised hosts.

**Let the daemon read the configuration file on a signal**, which would make `apply` a
`kill -HUP`. Rejected on ADR 0016's ground that nothing which runs on its own may read the
file; and a signal cannot carry which file, cannot answer, and is sent by a script as
easily as by a person.

**Drop the lock now that there is one writer.** Rejected: a lock that asserts the invariant
the design rests on costs nothing, and turns a duplicated daemon from a race into a
refusal.

## Consequences

- **The host has one writer.** Every replacement of the table happens inside a turn the
  daemon runs, and everything the daemon knows can be written back in that same turn. The
  window ADR 0007 and ADR 0016 warned about closes for elements that do not exist yet,
  which is when it is cheapest to close.
- **`regied apply` needs `regied.service` running**, and says so when it is not. On a new
  host the daemon starts first. This is the cost of one writer, and it is paid once, at
  install.
- **A submission survives its operator's session.** The turn finishes whether or not the
  client is still connected. A change that locks the operator out no longer also leaves
  the host at whatever step the disconnect happened to stop it at.
- **Six commands become five verbs, and two units become one.** `reconcile` and
  `regied-reconcile.service` go. What they did is done by the daemon's first turn and by
  `apply`.
- **A restart of the daemon is safe by construction**, because its first turn is
  unattended: it can bring things up and cannot take the line down. This is what makes it
  acceptable to have no separate boot unit.
- **The state is visible in `systemctl status regied`**, which is what stands in for the
  red boot unit.
- **The socket's mode is an authorisation to reconfigure the host**, and the package
  grants it to root alone.
- **The engine's shape does not change.** Staging, the record, the turn, the tiers, the
  report, backoff and the deadline are ADR 0016's, moved into one process. The client is
  new, and it is small.
- **ADR 0007's HTTP API is still deferred, and further from this socket than before.** The
  socket now carries the one write ADR 0007 refused to put behind an endpoint — and it
  carries it under a filesystem permission equivalent to root, to a process on the same
  host, which is the condition under which ADR 0007 said the refusal did not apply. A read
  API, when somebody wants one, is a separate decision about exposure and authentication,
  as ADR 0007 said, and it does not go on this socket.

## What this supersedes

- **ADR 0016 is superseded in its submission path, and nowhere else.** The turn a
  submission runs is the daemon's, not the `apply` process's; the record is written by the
  daemon; `regied reconcile` and the boot unit are removed, and the daemon's first turn is
  the boot-time convergence; turns are serialised within one process rather than across
  processes, and the lock becomes the daemon's. Its record, its tiers, its three states,
  its refusal of rollback, its deadline, its stop lever and its rules for what a turn may
  leave out are unchanged, and are built on here.
- **ADR 0007 is amended in what the socket carries.** "No status, no apply, no dry-run"
  becomes: the socket is the submission path and not a read API — four messages, and no
  question about the host. Its socket-under-`/run` decision and its reasons stand; its
  mode is narrowed to root.
- **ADR 0006 is unchanged.** `render` and `apply --dry-run` read no daemon.
- **ADR 0004 is unchanged.** The two stages, the order and the supervision decision are
  what every turn does; only which process runs a submitted turn changes.
