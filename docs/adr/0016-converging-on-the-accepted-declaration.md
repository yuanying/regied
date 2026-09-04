# ADR 0016: Converging on the declaration the host accepted

- Status: Accepted (2026-09-04). **Decided, not built.**

> **Nothing described here exists on the host.** `regied` has two commands, `render` and
> `apply`. There is no resident process, no loop, no timer, no confirmation, no deadline,
> and no unit that runs anything at boot. The only thing an apply leaves behind is the
> text of the ruleset it installed ([ADR 0004](0004-apply-model.md)), written on success.
> The record of an apply that [ADR 0007](0007-resident-process.md) designed is not built
> either, and this record changes what it holds before anything ever writes one.
>
> **The rollback [ADR 0005](0005-apply-rollback.md) decided is built, and it stays.** This
> record supersedes that one as a decision — nothing rolls back automatically any more;
> the loop keeps working toward what was submitted, and going back is a person applying
> the previous file — but the loop does not exist yet. Taking the undo away first would
> leave a period with no safety net and nothing in its place, on the one part of the
> system that only runs during an incident. **The code is removed by the unit that builds
> the loop**, in the same two-step way [ADR 0015](0015-uplink-addresses-in-sets.md) was
> decided and then built.
>
> **Everything below is therefore a decision about how to build this, not a description of
> anything running.** ADR 0007's HTTP API stays deferred, and nothing here revives it: the
> resident process this record asks for answers no questions over HTTP and serves no
> endpoint.

## Context

### The engine is already a reconciliation loop, one turn at a time

ADR 0004 did not decide to apply a configuration. It decided to make the host match one.
An apply reads the declaration, reads what the host is holding — the bytes and mode of
every file it owns, the value of every kernel switch it writes, whether its table is in
the kernel and what the sets in it hold, what each uplink link is holding — compares, and
moves only what differs. **An apply that changes nothing runs no command at all.** That
is level-triggered reconciliation, and it is what the safety of this system already rests
on: the dry-run's "nothing to do" (ADR 0006), the seeding of the uplink sets against the
kernel rather than against the recorded text (ADR 0015), the boot-time install of a table
the kernel does not have.

So the request that started this record — *keep the host at the declared state, rather
than applying once* — is not a redesign. **An apply is one turn of the loop.** What is
missing is two things: something to turn it, and somewhere to keep the declaration it
turns over.

### What was asked for

Three requests arrived together, and they are three faces of one design.

1. Keep the declared state continuously, the way a Kubernetes controller does, rather
   than only at the moment somebody runs a command.
2. **Stopping the daemon stops the loop.** During an outage an operator edits nftables by
   hand and needs the edit to stay.
3. Rolling back needs no mechanism of its own. The previous declaration is in version
   control, and a person can apply it.

They were prompted by an observation none of them is about: a configuration can be
**valid, renderable, applied without error, and wrong** — an interface declared in a way
that leaves the operator unable to reach the host over the network at all.

### The failure that none of the four safeguards catches

The task this project came from lists four safety properties. Against a declaration that
locks the operator out, every one of them passes.

| Safeguard | Why it does not catch it |
|---|---|
| Idempotent apply | It succeeded. The broken state is held correctly, and re-applying holds it again |
| Rollback | Nothing failed, so nothing rolls back |
| Dry-run | The configuration is valid and the rendering is fine. What is wrong is the meaning |
| A reconciliation loop | The declaration and the host agree. As convergence this is a healthy host |

**The absence of an error is the whole of the problem.** Every mechanism regied has
compares the declaration with the host. None of them can ask whether the declaration is
the one the operator wanted, because that question is not answerable from either side of
the comparison. Only somebody who can still reach the host can answer it, and the answer
that matters is the fact that they reached it.

### Where this is Kubernetes, and where it is not

The correspondence is close enough to build on, and it is worth being exact about.

| Kubernetes | regied |
|---|---|
| `kubectl apply` — submission to the API server | `regied apply` — **the only thing that reads the configuration file** |
| Admission: a manifest that does not validate is rejected before it is stored | Validation and rendering, before the first command runs (ADR 0004's staging stage) |
| The spec in etcd, written at submission | The record of the accepted declaration, written at submission |
| A controller's reconciliation loop | The resident process — **it reads no configuration file** |
| A controller that cannot converge retries and reports. It never reverts | The same |
| Going back is `kubectl apply` of the previous manifest | The same: `regied apply` of the previous file |

**A file on disk is not a spec.** It is not one in Kubernetes either: a manifest in an
editor is not in the cluster until it is submitted. Holding to the same discipline makes
the obvious hazard of a continuously reconciling router structurally impossible — a loop
that reads `/etc/regied/config.yaml` would reconfigure the host from a half-finished
edit, on a timer, with nobody present. A loop whose input is the record can only ever
move the host toward something a person submitted.

Two things are different, and both come from the same fact: **this host has one uplink,
no redundancy, and regied is reached over the line it is reconfiguring.**

- A Kubernetes controller can treat every action alike because a pod being restarted
  leaves the rest of the deployment serving. Here, restarting a session is the line going
  down for as long as the provider takes. A loop that treats replacing an nftables table
  and redialling a PPPoE session as the same kind of act will eventually take the line
  away on its own initiative, so the loop is given a line it may not cross on its own.
- Nobody is locked out of a cluster by a bad Deployment, because the API server is not
  behind the workload. Here the operator is behind the configuration, so there has to be
  a way for a change to undo itself when the person who made it can no longer say so. That
  is the one place this design keeps a "previous" declaration, and it keeps it for the
  length of a window and no longer.

## Decision

### The record holds the declaration, and the record is the spec

ADR 0007 gave an apply a second artifact under regied's state directory and listed what
it holds: revision, source, timing, outcome, failure, phases, notes, warnings, uplinks,
runtime values. That record was designed to be *read about*. This record makes it the
thing that is **converged toward**, which changes one thing about it and re-frames the
rest.

**It holds the declaration itself — the document's bytes as they were validated — and not
only the digest of them.** A digest says whether two declarations are the same one. It
cannot be rendered. A loop whose spec is the record has to render every backend from it,
so the record must hold what a renderer can be given. The revision stays beside it, still
the digest of those bytes, still comparable off the host (ADR 0007), and now trivially
derivable from what sits next to it.

Storing the declaration is safe for the reason ADR 0003 arranged: a declaration cannot
contain a secret, only the path of a file that holds one. The PPPoE credentials are read
from the files the declaration names, at the moment they are needed, and dropped
(ADR 0004). **A credential is not in the record and cannot be**, and the loop reads them
the same way an apply does, on the turn that needs them.

**`regied apply` is the submission, and it is the only thing that reads a configuration
file.** Nothing else — not the loop, not the boot unit, not any timer regied ships. This
is ADR 0007's "`serve` takes no configuration file" kept intact and promoted: it was
argued there as a way of keeping the address watcher honest, and it turns out to be the
whole of the protection against a router reconfiguring itself from an unfinished edit.

### The record is written at submission, and it is what was asked for

**A submission is accepted once it validates and stages, and the record is written then —
before the first command runs.** From that moment the declaration is the spec, whether or
not the commands that follow succeed. This is what `kubectl apply` does: the spec is
stored at admission, and what the controllers make of it is a separate matter that the
status reports on.

The record therefore never holds a "last known good" declaration and never holds two.
It holds **what was asked for**, and the host is either at it, on its way to it, or
failing to reach it — which the turn's state says (below). Nothing in the record is a
promise that the declaration ever worked, and nothing needs to be: the previous
declaration is in version control, where ADR 0005 already put it when it decided that
going further back than one apply is an ordinary apply of an ordinary file.

Beside the spec sits the account of what the last turn did — ADR 0007's report, with the
turn's state added to it. It is written by the loop as well as by a submission, and it is
diagnosis, not a second spec: nothing converges toward it.

| Artifact | Written | What it is for |
|---|---|---|
| The accepted declaration | At submission, once validated and staged | The spec. What the loop converges toward |
| The report of the last turn | When what it says changes | Diagnosis. Revision, source, outcome, failure, phases, notes, warnings, the state the turn ended in, and what it is waiting on or failing at |

The report is written when its content would differ, which is ADR 0004's rule for every
file regied writes, and it records **when the state it describes was entered**, not when
the last turn checked it. A report that carried the time of the last turn would be
rewritten every minute for the life of the host to say nothing new; when the last turn
ran is `systemctl status`'s to answer.

The uplink rows ADR 0007 put in the record are no longer a separate field of it. They were
there so the watcher could know which links are uplinks without parsing YAML; with the
declaration in the record, they are derived from it the way everything else is.

**One consequence is worth stating: the ruleset text ADR 0004 records stops being
something the host must remember.** ADR 0015 made the rendered ruleset a function of the
configuration alone, so the text last installed is exactly what the accepted declaration
renders to. It is derivable. Whether the engine goes on keeping it as a cache is an
implementation question this record does not need to settle; what it settles is that
**the accepted declaration is the one thing that has to survive a reboot.**

### A turn, and what causes one

A **turn** is what an apply already does, minus reading a file: collect the runtime
values, render every backend from the spec, compare with the host, move what differs, and
run the commands the differences call for, in ADR 0004's order. There is no second
implementation of convergence, and there is no code path in the loop that an apply does
not also take.

Three things cause a turn.

- **A submission.** `regied apply` validates and stages the declaration in the file,
  writes it to the record, and runs a turn toward it in its own process.
- **A periodic resync.** The floor. It is what makes the loop a repair mechanism rather
  than an event processor, and it is affordable precisely because a turn that finds
  nothing wrong runs no command. The interval is a flag on the resident process, set by
  the unit the package ships, with a default of one minute — short enough that drift is
  taken back before anyone builds on it, long enough that a host doing nothing is not
  rendering four backends every few seconds. It is about how regied is run, which is
  packaging, and not something a declaration says (ADR 0002, and ADR 0007's argument for
  the units being the package's).
- **A kernel address event.** The netlink subscription ADR 0007 gave the watcher. It
  wakes a turn instead of writing a set directly.

That last one dissolves a mechanism rather than adding one. **ADR 0015's third writer of
the uplink sets is not a separate writer any more; it is the loop, woken early.** A turn
already seeds every set from the kernel, so an address event needs to do nothing except
make the turn happen sooner than the resync would. Narrowing such a turn to the firewall
phase is an optimisation inside the loop that changes nothing decided here.

**The pppd hooks ADR 0015 renders are kept, and are deliberately not folded in.** Of that
record's three writers, two — the apply and the daemon — turn out to be the same thing seen
twice, and they collapse into "a turn". The hook does not: it is the fast path that puts an
address in a set the moment pppd learns it, and it works when the loop is stopped, has
crashed, or has never been started. That is the property ADR 0015 was after when it said
the host is correct without a daemon, and this record does not spend it. **Three writers
become two: a turn, and a hook beside it.**

Nothing else is watched. There is no inotify on the files regied owns, no subscription to
systemd's unit states, no watch on `/proc/sys`. Event sources are added where they are
cheap and already needed; the resync is what covers everything, and a Kubernetes informer
has a resync period for the same reason — an event stream is a latency improvement, never
a source of truth.

**Turns do not overlap, across processes as well as within one.** A submission runs its
turn in the `apply` process while the resident process may be about to run one of its own,
so a turn holds a lock under the state directory for its duration, and the other waits.
A tick that arrives while a turn is running is delayed, not run beside it.

### What an unattended turn may do, and what it may not

**The tiers are not about the actions. They are about who asked for the turn.**

A **submitted turn** is one an operator asked for: `regied apply`, and the revert of an
expired confirmation, which the operator asked for in advance when they set the deadline.
It may do everything ADR 0004 describes, including restarting a PPPoE session.

An **unattended turn** is one the resync or an event caused. Nobody is watching, and the
host has one uplink.

| What differs | An unattended turn |
|---|---|
| The nftables table, or a set in it | Replaces it. One transaction, ADR 0013's shape, nothing else on the host disturbed |
| A kernel switch | Writes it, having read it first |
| A file regied owns — networkd, pppd's options, dnsmasq's configuration, a hook, a unit it writes | Writes it |
| A declared unit that is not active | Starts it, under backoff |
| A declared session whose options files it just rewrote, and which is running | **Reports. Does not restart it** |
| A declared session that is running and whose unit is failing to stay up | **Reports. Does not restart it** |
| Something running that the declaration no longer declares | **Reports. Does not stop it, and does not reclaim the unit it runs from** |

**The line is: an unattended turn never takes down something that is up.** It creates,
writes and starts; it does not restart and it does not stop. Starting a session that is
not running cannot cost a line that is already down, and it is the case an operator would
want handled at three in the morning. Restarting one that is up is the act that costs
seconds of the only line there is, and the judgement behind it belongs to a person or to
the deadline a person set.

Two consequences of that line are worth having in writing, because they are the cases
where the loop knowingly stops short.

- **A file rewritten under a running process is drift that the turn only half resolves.**
  If somebody edited pppd's options file, the turn puts regied's content back and the
  running session goes on running with what it read at start. The turn says so. The
  session picks the file up whenever it is next started — by an operator, or by systemd
  after a crash.
- **A unit is not reclaimed while what runs from it is running.** ADR 0004 orders the
  removal of a unit after the stop that resolves through it; an unattended turn does not
  stop, so it does not remove either. It reports, and the next submitted turn does both in
  the right order.

Reloading networkd sits on the safe side of the line and stays there: ADR 0004 already
decided reload and never restart, and `KeepConfiguration=yes` is what makes a reload safe
over a session pppd brought up. Restarting networkd is not something any turn does.

Everything an unattended turn is allowed to do that is not a plain file write is under
backoff, which a later section is about.

### A turn renders what it can, and the rest arrives on a later turn

ADR 0004 gave the staging stage a set of values that exist only at apply time — the address
the AFTR name resolves to, the contents of a DUID file, the addresses each uplink link is
holding, the two PPPoE credentials — and had to decide, one at a time, which of them being
missing is fatal. It is a hard question when there is one attempt. **With a loop it is
mostly not a question at all: a value that is missing now is missing now, and there is
another turn in a minute.**

| Runtime value | Under a single apply | Under the loop |
|---|---|---|
| An uplink's address | ADR 0015 took it out of the ruleset and put it in a set | Seeding a set from the kernel *is* the work of a turn. Already level-triggered, already repeated |
| The address the AFTR name resolves to | Resolved during the apply; a boot where DNS is not up yet is a failed apply | The tunnel is left out, and appears on the turn where the name resolves |
| The DUID | Read during the apply; unreadable stops the rendering, because networkd would otherwise send its own and the delegated prefix would change | The prefix delegation is left out; what is already on the host is left alone, and it is picked up when the file can be read |
| A PPPoE credential | Read, used, dropped | Unchanged. The same rule, on every turn that needs it |

This is ADR 0006's rule for `regied render` — what depends on a value that was not supplied
is omitted exactly as an apply would omit it — turned into the engine's ordinary way of
working. Half of it is already in the engine, and the loop is what makes the other half
safe: leaving something out only settles if something comes back for it.

**What is left out is the whole artifact that depends on the value, and what is already on
the host is left alone.** This is the part that is easy to get wrong, and ADR 0004's DUID
argument is the case that shows why. Writing the prefix delegation without the DUID is not
a smaller version of it; it is a different configuration, one where networkd sends its own
identifier and the prefix changes silently. So a turn that cannot compute an artifact does
not write a degraded one, and does not reclaim the one a previous turn wrote either — the
host keeps the last complete thing it was given while the loop waits for what is missing.
**Incomplete must never be spelled with a half-written file.**

### Every turn ends somewhere safe

Incomplete is allowed. Unsafe is not, and the difference is not a matter of degree: **there
must never be a turn that brings the line up with no firewall in front of it.**

"Safe" here means exposure, not availability. A turn may end with the line down — a
session that did not come up, a tunnel not yet rendered — and that is a host that is
waiting or failing, and says so. What a turn may not end with is a host that forwards
packets with no filter in front of them, or a host holding a configuration it never
declared.

**ADR 0004's phase order holds inside a turn, for the reason it was chosen.** The firewall
goes first because nothing should be able to move a packet before the rules that filter it
exist; forwarding is enabled in the phase after it. What the loop adds is that a turn can
also be stopped part way — by a failure, by a backoff, by an operator stopping the daemon —
and nothing comes along afterwards to undo the half that ran. So the ordering has to be
read more strictly than it was: **every prefix of a turn has to be a safe state, not just
the whole of one.** The order ADR 0004 chose already has that property, and this record
fixes it as a requirement rather than a happy consequence.

**Completeness is what crosses turns. Ordering is not.** A tunnel that could not be rendered
waits for the next turn; enabling forwarding before the table is installed is not something
to defer, because deferring it is exactly the window the ordering exists to close.

This is the same concern as the tiers above, from the other side. The tiers say who is
allowed to take an action that can cost the line. The ordering says that within a turn, the
protective work happens before the work it protects. Both exist because there is one uplink,
and neither substitutes for the other.

### Converged, or waiting on something

**A host that has arrived and a host that is stuck must not look alike.** With turns that
leave things out on purpose, and with nothing reverting a submission that did not finish,
"the last turn ran and changed nothing" no longer means what it used to. So a turn ends in
one of three states, and says which.

| State | What it means |
|---|---|
| Converged | The host holds the declaration entirely. Nothing was left out, nothing is being retried |
| Waiting | Everything that could be done was done, and something is left out for want of a runtime value. **What it is waiting on is named** |
| Failing | Something was attempted and did not work, or something differs that this turn was not allowed to fix. It is under backoff, or it is waiting for a person. **What, and why, is named** |

Naming the thing is the whole point: *waiting on the AFTR name to resolve, and on the DUID
file* is a diagnosis; *not converged* is a mood. This is what a Kubernetes controller puts
in a status condition, and regied has no API, so it goes in the two places it has — the
report of the last turn, and the journal.

The three states are what "changed nothing" has to be read against. An idempotent engine
answers "nothing to do" most of the time and that is the healthy answer (ADR 0006); the same
words from a turn that has been waiting on the same name for an hour mean something else
entirely, and the difference has to be visible without anyone reasoning about it.

**A submission reports the same three states, and its exit status follows them.** An apply
whose turn ended failing exits non-zero and says what failed; one that ended waiting exits
zero and says what it is waiting on, because waiting is the ordinary state of a host that
has just come up and not a defect in the submission.

### Nothing rolls back but a person

**There is no rollback.** Not per step, and not as convergence toward an earlier
declaration. A submission whose turn fails part way leaves the record holding the
declaration that was submitted, the host holding as much of it as got done, and the loop
working toward the rest under backoff. **Going back is a person applying the previous
file**, which is in version control, with `regied apply` — an ordinary submission of an
ordinary declaration, and the thing ADR 0005 already said going back further than one apply
would be.

The reasons are the user's, and they hold up.

- **The person is there.** A submission is a command somebody typed, and the failure lands
  on their console in the same second, in the words ADR 0005 and ADR 0006 already chose for
  it. They know what they just changed. Nothing on the host knows better than they do
  whether the right response is to try again, fix the file, or go back — and a machine
  deciding "go back" on the evidence of one failed command would be deciding it wrong
  whenever the failure was transient.
- **Under a loop, "failed" usually means "not yet".** The section above has turns leaving
  things out to be picked up later; a resolver that is not answering, a unit that took a
  second longer to start. An automatic revert would undo a declaration that was a minute
  from converging, and would do it with the full authority of a submitted turn — restarting
  the session on the way.
- **One mechanism instead of two.** Every step in the engine currently carries an inverse,
  and that set of inverses is the least exercised code in the system, reached only when
  something has already gone wrong. It goes, and nothing has to replace it: the path that
  runs on every host every minute is what finishes the work, and a person is what changes
  the target.
- **A revert is not free.** Converging back to a previous declaration is a turn like any
  other, with the same phases, and it may restart the session that was just restarted. The
  operator who is about to do that by hand can see whether the line is up and choose the
  moment. A mechanism cannot.

**What the loop finishes on its own is bounded by the previous section.** After a failed
submission it writes what is missing, replaces the table, starts what is declared and not
running — and it reports what it is not allowed to do: restart a session that is running on
old options, stop what is no longer declared. Those wait for a person, and the way to ask for
them is to run the same `regied apply` again. An apply is idempotent (ADR 0004), so
re-running it does the steps that remain and nothing else; **the retry for the half of a
submission the loop may not do is the submission itself.**

Three things this does **not** change, all of them inherited from ADR 0005.

- **Applying the previous file does not un-happen events.** A session that was restarted is
  a new session. Leases dnsmasq handed out under the new configuration stay handed out.
  Connections the new ruleset dropped stay dropped. An address networkd removed was removed.
- **A failure part way is reported as what it is.** ADR 0005's disposition survives with the
  undo taken out of it: stop at the step that failed, describe what the host is now running,
  and exit non-zero. On a host with one uplink an accurate description beats a loop of
  reloads.
- **A previous declaration cannot restore what regied never owned.** If a declaration set a
  kernel switch and the next declaration stops mentioning it, applying the earlier file does
  not put back the value the host had before regied was installed. **That is ADR 0009's
  ownership question — what reclaim means for state that has no marker — and it predates
  this record rather than being created by it.**

The first submission on a host has nothing to go back to and needs nothing: the declaration
that takes everything away is the empty one, and applying it is ADR 0009's reclaim. That is
the concept that already exists, and the loop does not do it by itself.

### When a turn cannot converge

**The disposition is Kubernetes's, for the loop and for a submission alike: retry, back
off, report.** Being unable to reach the declaration that was asked for is a condition to
be reported and retried, not a reason to jump somewhere else, and there is nowhere else
in the record to jump to.

Backoff, which is a requirement and not a refinement:

- **Per target, not global.** A dnsmasq that will not start must not stop the table from
  being repaired.
- **Driven by attempts that did not resolve the drift, not by the exit status of the
  command.** A unit that starts and dies immediately reports success to the thing that
  started it. Flapping is exactly that case, and counting exit statuses would miss it.
- **Exponential with a ceiling; reset when the drift clears.** The ceiling matters more
  than the growth: a host whose provider is down for an hour must not spend the hour
  issuing commands.
- **A submitted turn is not rate-limited, and it resets the backoff.** The operator asked,
  and they are present.

The cheap half of a turn — render, read the host, compare — runs regardless. Backoff
suppresses the command, not the comparison, so the report of what is wrong stays current
while the retrying is slowed.

### Applying with a deadline

This is the answer to lockout, and it is the mechanism the operator already knows from
EdgeOS and VyOS (`commit-confirm`) and from JunOS (`commit confirmed`). It is also **the
one exception to the record being written at submission**, and the one place a previous
declaration is kept — for the length of a window, and no longer.

**`regied apply --confirm <duration>` applies, and then waits to be told it worked.**

| Step | What happens |
|---|---|
| The submission | Validated and staged as any submission is. **The record is not written.** The declaration goes to the resident process as a **trial**, and the submitted turn runs toward it |
| Whatever that turn achieved | The clock starts. A turn that ended waiting, or failing, starts it all the same — seeing that is what the operator is there for, and the clock is what covers them if they are not |
| While the clock runs | The loop's spec is the trial. Turns converge toward it, as usual. A second `--confirm` replaces the trial and leaves the record alone |
| `regied confirm` | The trial is written to the record and becomes the spec. The clock stops |
| `regied cancel` | The trial is dropped, and a turn converges toward the record — the previous declaration — at once |
| The clock expires | The same as cancel, without anybody having to be there |

**Which of the two is transient, and why it is the trial.** The record goes on holding the
previous declaration and the resident process holds the trial in memory, not the other way
round, because every way of losing the process has to resolve toward the previous one. An
operator who is locked out will power-cycle the router; if the trial were in the record and
the previous declaration in memory, the reboot would bring the lockout back with nothing
left to undo it. With the previous declaration in the record, a daemon that dies mid-trial
comes back, finds the record, and converges toward it — expiry arriving early, in the safe
direction. **A trial that survived a reboot would be a trial nobody is timing.**

**A trial may be confirmed whatever state its turn is in, and it is never refused on the
grounds of what the turn achieved.** The deadline tests one thing — whether the operator can
still reach the host — and the confirmation is the answer to that and to nothing else.
Requiring convergence first would make the mechanism fire on cases it is not for: a
declaration waiting on an AFTR name that upstream DNS is slow to answer would be
unconfirmable, and would be reverted at expiry with the operator standing right there.

What the daemon does instead is **say what is being confirmed**: the revision, and the state
of the last turn, so that confirming something that is still waiting on two artifacts, or
failing on one, is done knowingly rather than by accident. An operator who wants to see it
converge first simply does not confirm yet. **The deadline is a budget for their own
checking, and how they spend it is theirs.**

**The revert is a submitted turn**, with all of ADR 0004 available to it including a
session restart, because the operator asked for it when they set the deadline. This is
what makes the mechanism worth anything: the change that locked them out is very often
exactly the change that has to be undone in full. It is also the only automatic revert in
this design, and the reason it is allowed here and nowhere else is the reason the previous
section gave for refusing it elsewhere: **the person is not there.** Everything the
previous section trusts to the operator — judging whether the failure is transient, choosing
the moment — is exactly what a locked-out operator cannot do.

**An apply with `--confirm` is refused when the resident process is not running.** The
deadline is the whole of the promise, and there is nobody to hold the clock. Accepting it
silently would be a lie told at the precise moment somebody is relying on it. The refusal
says what to do instead: start regied, or apply without `--confirm` and accept that
nothing will undo it. The alternative — the `apply` process forking something to hold the
clock — was considered and is rejected: what has to outlive the operator's ssh session is
by definition a daemon, and ADR 0004 already decided that regied does not supervise
processes of its own.

The two verbs reach the daemon over a control socket in regied's own directory under
`/run`, with a filesystem mode, exactly as ADR 0007 described for its socket and for the
same reasons — a socket cannot be reached from the network, which matters when the
firewall that would protect a port is installed by the thing behind the port. Two other
transports were weighed. A signal cannot carry which trial is meant or answer with what
was confirmed. A file the daemon polls would make confirmation wait for a tick, and a file
is the one transport that a scheduled job writes as easily as a person does. The socket
answers synchronously and is written to by a command a person runs.

**This is not the HTTP API and does not revive it.** The socket carries two verbs and
nothing else. There is no apply endpoint, no dry-run endpoint and no status endpoint;
ADR 0007's read-only API stays deferred and unbuilt. ADR 0007 declined write endpoints on
two grounds, and both are satisfied here: the sharp act it refused to expose was `apply`,
which is still not exposed, and its second ground — that an endpoint adds no capability
to somebody who can already run `regied` on the host — is exactly why confirming over the
socket is acceptable.

Details that follow from the shape:

- **`--confirm` is opt-in, not the default.** A boot unit and any unattended apply have
  nobody to confirm them, and a default that expires unconfirmed on a host nobody is
  watching would revert every boot.
- **A plain apply during a trial ends it**, by writing its own declaration to the record
  at submission. There is then no previous declaration anywhere and no clock. That is the
  operator choosing to have no net, the same choice as not passing `--confirm` in the first
  place, and the command says so.
- **`regied confirm` names no revision.** At most one trial exists; the daemon answers
  with the revision it confirmed.

**The condition this rests on, which regied cannot enforce.** The confirmation has to
travel the path that a lockout destroys. An operator who arranges for the confirmation to
arrive by some other route — a scheduled command that confirms after sleeping, a script on
a serial console — has disarmed the mechanism, because the thing being tested is not the
configuration but whether the operator can still reach the host. Somebody working from a
console that the change cannot affect is not at risk of this failure and does not need
this mechanism; what must not happen is confirming from such a path while believing the
network path was tested.

### Stopping is one operation, and it breaks nothing

**`systemctl stop regied` stops the convergence and does nothing else.** No file is
reclaimed, no table is removed, no session is stopped, no switch is put back. The host
goes on running exactly what is on it. **What stops is the converging, not the
configuration.**

This is the requirement that during an outage an operator can edit nftables by hand and
have the edit stay. A loop would take it back within a resync interval; stopping the
daemon is how they say *leave it alone for now*. Starting it again resumes convergence,
which will take the hand edit back — and that is the correct behaviour, made explicit and
put under the operator's control.

**Therefore the loop lives only in the resident process, and the package ships no timer
that runs `regied apply`.** The value of the stop lever is that there is one of them. Two
mechanisms would mean two things to stop, and the one people forget is the one that runs
at the worst moment. A timer would also be the wrong mechanism on its own terms: it would
read the configuration file, which is the hazard the first section of this decision closes.

ADR 0004's statement that an apply is safe to run from a timer is still true and still
worth having — it is a property of idempotence, not a plan — and an operator who wants
such a timer can have one, knowing it reads the file.

Stopping the daemon during a trial is the operator choosing to hold the clock themselves:
the trial dies with the process, and the next thing to run a turn — the daemon started
again, or the boot unit — converges toward the record, which is the previous declaration.
That is the safe direction, and an operator who stops the daemon mid-trial should expect
it.

The stop lever belongs in the README's outage procedure: stop before touching things by
hand, start when finished. That is the unit that writes the operational documentation, not
this one.

### At boot, and nothing reads the file

ADR 0007 decided two units and argued that the boot-time apply stands on its own: the
ruleset is kernel state, so something must install it after every reboot, and that
something is an apply rather than a file. All of that survives. **What changes is what it
reads.**

| Unit | What it runs | Input |
|---|---|---|
| The boot unit | One turn, then exits | The record |
| The resident process | The loop | The record |

A boot-time apply that read `/etc/regied/config.yaml` would be the same hazard the loop
avoids, deferred to the next reboot: an edit left unfinished in the file becomes the
host's configuration the next time the power goes out. Reading the record instead closes
it. **After this record, no automatic mechanism in regied reads a configuration file.**

`regied reconcile` is the name of that one turn: converge toward the record and exit,
non-zero if it ended failing. It is the boot unit's command and it is what a person runs to
ask "put the host back where it should be" without submitting anything. It is a third
verb, and it earns its place by being the only way to ask for a turn that reads no file.

The boot unit stays a separate unit even though the resident process would run the same
turn on its way up, for two reasons. The stop lever is also a disable lever — an operator
who has turned the daemon off during an incident and then reboots still needs a firewall
after the reboot — and a unit that runs once and exits gives the boot a definite point at
which the host is configured, which a daemon that keeps running does not.

The ordering ADR 0007 decided is unchanged: both units after `systemd-networkd`, neither
before `network-online.target`, the resident process after the boot unit and not requiring
it, the guard against a distribution `nftables.service` flushing the ruleset. The reason
the resident process must not require the boot unit gets stronger here: when the boot turn
fails is exactly when the loop's retrying is wanted.

**A host with no record does nothing, and says so.** Not converge to the empty
declaration. Reclaiming everything regied owns — taking the firewall off a running router
— is not a reasonable response to a missing file. A turn that finds no record knows nothing
about how the host got the way it is, and the one declaration that takes everything away is
one a person submits on purpose.

**A record that no longer validates does nothing either, and says so.** The case is a
regied upgrade whose validation grew stricter than the declaration that was accepted under
the old one. Converging toward a partly-understood declaration would be worse than not
converging, and converging toward empty would be worse still. The turn reports and the
host keeps running what it is running, which leaves the operator a working router and a
message telling them to submit a declaration the new version accepts.

### What the loop can see, and what it cannot

**The loop sees differences between the declaration and what regied can read.** Files,
kernel switches, its table and the sets in it, whether a unit it declares is enabled and
active. That is the whole list.

**Health is not drift.** pppd running but never authenticating, a session up with no
route out, a DNS forwarder answering nothing but failures: in every one of those the
declaration says "there is a session" and there is one. There is no difference to find.
Reporting them requires a different observation, with a policy attached — how long is too
long, what counts as reachable — and an action attached, which on a router with one uplink
is redialling: the one act this record just forbade the loop to take on its own.

So the line is drawn here: **a declared unit that is not active is drift, and a declared
unit that is active is at the spec, whatever it is doing.** That line needs no new
observation — systemd already answers it — and it needs no policy.

This does not leave a dropped line unattended. A session that goes down comes back through
pppd's own `persist` and its LCP echoes (ADR 0014) and systemd's `Restart=` (ADR 0004);
supervision is systemd's and always was, which is ADR 0008 one layer up. What is not built
is a reachability watchdog that redials when traffic stops flowing. That is a real thing
to want, it is a different record, and it has to answer the questions above before it can
be written (ADR 0002: the interfaces that are hard to withdraw are the ones added before
anybody needed them).

A turn that cannot render *part* of the declaration leaves that part out and waits, as
decided above. A turn that cannot render *anything* — the accepted declaration no longer
parses or no longer validates — changes nothing and reports, which is ADR 0004's staging
stage doing what it already does: a failure before the first command costs nothing.

### Where a failing convergence shows up

There is no HTTP API. The surfaces are these.

- **The journal, under the resident process's unit.** `systemctl status` and `journalctl`
  are where an operator looks, and the loop is a systemd service precisely so that they
  work.
- **Each distinct drift is logged when it appears and when it clears**, with escalations of
  its backoff, and **not on every turn.** A loop that logs at every tick is a loop nobody
  reads, and this one ticks every minute forever. **A change of state — converged, waiting,
  failing — is logged whenever it changes**, with what is being waited on or what failed,
  by the same rule.
- **The report of the last turn**, rewritten when what it says changes, so a daemon restart
  does not lose what went wrong.
- **The console and exit status of `regied apply`**, for the turn a submission ran.
- **`regied reconcile` exits non-zero** when its turn ended failing, which is how the boot
  unit's failure reaches systemd.

**The resident process does not exit because it cannot converge.** A daemon that dies on
drift takes away the thing reporting the drift, and hands systemd a restart loop on a
router. It stays up, keeps comparing, keeps retrying under backoff, and keeps saying what
is wrong.

## Alternatives considered

**A timer that runs `regied apply`.** The cheapest possible loop, and it was the shape
this design started from. Rejected twice over: it reads the configuration file, so an
unfinished edit lands on a schedule with nobody present; and it puts the convergence
somewhere other than the daemon, which makes stopping it two operations when the
requirement is that it be one.

**Letting the loop read the configuration file.** Simpler — no record to keep, no
submission step. It gives up the property that the whole design rests on: that the host
can only be moved toward something a person submitted. It also makes the spec change
without anyone deciding it changed, which is the failure mode people fear about
continuous reconciliation, and they are right to.

**A record written only when a turn converged, so that it is always the last declaration
that worked, and a failed submission converges back to it automatically.** This was the
shape of this design for most of its drafting, and it is the one the user's third
observation removed. It looks like safety and it is two things at once: a machine judging
a declaration bad on the evidence of one failed command, at exactly the moment a loop has
made "failed" mean "not yet"; and a second declaration that has to be held somewhere
whenever a submission is in flight, which puts the desired-versus-last-known-good pair back
in through the side door. What it protects against — the person who submitted being unable
to go back — is not a thing that happens, except in the one case where they cannot reach
the host, and that case has its own mechanism.

**Two persistent records, the desired declaration and the last known good one.** Rejected
for the same reason with one more: on every path something has to decide which of the two
is authoritative, and getting that wrong in one place is a revert to a declaration that
never worked.

**Keeping the per-step undo alongside the loop.** Two mechanisms for one job, where the
less-used one is the one that runs during an incident. The undo is kept until the loop
exists, and then removed; that is sequencing, not a design.

**Making `--confirm` the default.** It would put the safety net where people forget to
ask for it. Rejected because the boot turn and every unattended apply have nobody to
confirm them, and a deadline that expires on an unwatched host reverts a change that was
fine.

**Holding the trial in the record and the previous declaration in memory** — the other
way round from what was decided. It would keep "the record is written at submission" true
without exception. Rejected because a reboot mid-trial, which is what a locked-out operator
does to the router, would then bring the trial back with nothing left to undo it.

**A confirmation delivered out of band** — from a serial console, from a second link.
Not rejected, because an operator in that position was never at risk of the failure this
mechanism is for. Named because confirming from such a path while believing the network
path was tested is how the mechanism is defeated without anybody noticing.

**Detecting lockout directly** — probing reachability, or reasoning about the ruleset to
find out whether the operator's own path survives. Rejected on principle rather than on
cost: any mechanism that compares the declaration with the host is blind to a declaration
that is wrong, and a probe would need to know which path the operator is on, which regied
cannot know and should not guess. The deadline works because it uses the operator's own
reachability as the test, without having to model it.

**Keeping ADR 0004's disposition that a missing runtime value fails the apply.** It is the
right answer when there is one attempt and no second chance: the alternative was writing a
prefix delegation without its DUID, which silently changes the prefix. With a loop the
answer changes, but only because the omission is at the level of the whole artifact and
because something comes back for it. Failing the turn instead would mean a boot where the
resolver is not up yet ends with a failed boot turn rather than a tunnel that appears a
minute later.

**Folding ADR 0015's pppd hooks into the loop**, now that the loop can do what they do.
Rejected: they are the path that works when the loop is not running, which is the property
that record was bought with. A mechanism that is only correct while the daemon is alive is
the thing both records have been avoiding.

**Watching every artifact for events** — inotify on the files, subscriptions to systemd.
Rejected as latency work with a correctness cost: an event stream that is believed becomes
a source of truth, and event streams miss things. The resync covers everything, and the
one subscription that is kept is the one ADR 0015 already needed.

## Consequences

- **A submission that fails part way stays failed until a person acts.** This is the cost
  of having no automatic revert, and it is real: a declaration that restarts the session
  with options pppd rejects leaves the line down until the operator applies the previous
  file or fixes this one. The loop will keep starting the session under backoff, and it will
  keep saying why it is not staying up, but it will not change the target. What makes the
  cost acceptable is that a submission is a command a person types: they are on the console
  when it fails, the previous file is in version control, and `regied apply` of it is one
  command. The case where they are *not* there to act is lockout, and that case has the
  deadline.
- **The least-tested path in the system goes away and nothing has to replace it.** Recovery
  stops being a separate mechanism with its own inverses. What finishes a submission is the
  loop that runs on every host every minute; what changes the target is a person.
- **The loop can only move the host toward something a person submitted.** No sequence of
  edits to a file, however broken or however incomplete, reaches the host without a
  submission. This is a structural property, not a rule to remember.
- **After this record, nothing automatic in regied reads a configuration file** — not the
  loop, not the boot unit, and no timer regied ships.
- **The accepted declaration becomes the one piece of state that has to survive.** The
  ruleset text ADR 0004 recorded is derivable from it, and a host that loses the record
  does not reconfigure itself: it reports and keeps running, which is the safe direction
  and one worth testing for directly.
- **A stopped daemon is a supported state, not a degraded one.** The host keeps running
  what is on it, hand edits stay, and starting the daemon resumes convergence and takes
  them back. Everything an operator does during an outage is bracketed by one stop and one
  start.
- **Lockout protection requires the resident process, and says so at the moment it is
  asked for.** A host with the daemon stopped can still apply; it just cannot apply with a
  deadline, and it is told why rather than being allowed to believe otherwise.
- **A daemon that dies during a trial reverts on the way back up.** The trial is transient
  by design and the previous declaration is the durable one, so every way of losing the
  daemon — a crash, a stop, a power cycle — resolves toward the declaration that was in
  effect before the trial.
- **The staging stage gets simpler rather than larger.** ADR 0004 had to rule on each
  runtime value separately, because a missing one had to be either fatal or ignorable
  forever. Under the loop the general answer is "leave the artifact out and wait", and the
  rulings that remain are the ones that are about correctness rather than about timing.
- **"Nothing to do" acquires a second reading, and the states are what keep them apart.**
  A converged host and a host that has been waiting on the same name since it booted both
  change nothing on every turn. Anything that reports on convergence — the journal line, the
  report, a future readiness answer — has to carry the state and not just the diff.
- **The loop is unit-testable the way the engine is**, because it is the engine: the same
  interfaces stand in for the filesystem, the command runner, the resolver and the link
  reader (ADR 0004), and what is new is a clock and a lock, which join them. `make test`
  stays pure.
- **There is exactly one answer to "what is this host supposed to be"**, it is a file a
  person can read, and it says what was asked for rather than what once worked. Whether the
  host is there is a different question, and the report answers it.
- **ADR 0007's HTTP API is still deferred and is now less pressing**, because the two
  questions the API was going to answer — what is in effect, and what went wrong — have a
  journal and a report behind them. What an API would add is remote access to them, which
  is the same thing it always was and still nobody has asked for.
- **This record does not close off the direction ADR 0007 named.** Runtime state that
  arrives from somewhere other than the kernel — the backends behind a load-balanced
  address, to name the case that prompted that paragraph — would be written by this loop,
  in the same place, leaving the rendered text a function of the configuration alone. The
  warning attached to it there is unchanged and applies here word for word: the uplink sets
  are safe because the kernel holds their answer, and an element whose source is not the
  kernel has no such recovery when the table is replaced.

## What this supersedes

- **[ADR 0005](0005-apply-rollback.md) is superseded.** Rollback as an independent
  mechanism with an inverse per step goes, and nothing automatic takes its place: the loop
  finishes what it may, and a person changes the target. What ADR 0005 says about what
  cannot be reversed at all — a restarted session, a lease already handed out, a connection
  already dropped — survives intact and is restated above, because it is a property of the
  host and not of the mechanism; and its last section, that going back is an ordinary apply
  of an older file from version control, stops being the exception and becomes the whole
  answer. **The code stays until the loop is built.**
- **The daemon half of [ADR 0007](0007-resident-process.md) is superseded.** "The resident
  process never applies" is reversed: converging is the reason it exists. "It takes no
  configuration file" is not merely kept but promoted to the centre of the design. The
  record it designed gains the accepted declaration, is written at submission, and carries
  the turn's state. Its HTTP API, its endpoints and its socket-as-API-transport stay
  deferred, and the control socket here carries two verbs and nothing else.
- **[ADR 0004](0004-apply-model.md) is amended** in three places: the boot-time apply reads
  the record rather than the file; the timer it names as a thing an apply is safe to run
  from is not what runs the loop; and its per-value rulings on what a missing runtime value
  does become the general rule that the artifact depending on it is left out and picked up
  on a later turn. Its ordering, its phases, its two stages, its idempotence rule and its
  supervision decision are all unchanged — this record is built entirely on top of them,
  and the rollback it points at is ADR 0005's, superseded above.
- **[ADR 0015](0015-uplink-addresses-in-sets.md) is amended** in one place: of its three
  writers of the uplink sets, the apply and the daemon are one thing — a turn — and the pppd
  hook stays beside it as the path that needs no daemon.
