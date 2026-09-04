# ADR 0006: Showing what would change before anything changes

- Status: Accepted (2026-09-03)

## Context

The task this project came from asks for a dry-run in the same breath as idempotence and
rollback, and for the same reason: there is one uplink, and the person applying a change
is reached over it. It also sets a completion condition — the nftables rules, the dnsmasq
configuration and the routing a configuration produces have to be inspectable without
applying them.

There are two different questions hiding in that, and answering both with one command
would answer neither well.

- *What does this configuration mean?* — asked while writing the file, on a laptop, about
  a host that may not exist yet. [ADR 0012](0012-networkd-rendering.md) went out of its
  way to keep this answerable by making every renderer a pure function.
- *What would change on this host, now?* — asked with a finger over the enter key, and
  the answer depends on what is already on the host and on what the line is currently
  holding.

[ADR 0003](0003-secrets-out-of-configuration.md) constrains both: nothing regied emits
for diagnosis may carry a credential, and that explicitly includes dry-run output and a
diff. A dry-run whose whole job is to print what would be written is the place where that
rule is easiest to break by accident.

## Decision

### Two commands, split by whether the host is read

**`regied render` reads no host state.** It renders a configuration and prints the result.
The values that exist only at apply time are supplied on the command line or left out, and
what depends on a value that was left out is omitted exactly as it would be omitted at
apply time. It runs anywhere, for any host, and it is what answers the first question.

**`regied apply --dry-run` reads this host.** It collects the runtime values here,
computes the plan an apply would compute, compares it with what is on the host, and prints
the difference. It runs no command that changes anything and writes no file.

### A dry-run is the apply path, stopped at the commit stage

It is not a second implementation of what apply would do. [ADR 0004](0004-apply-model.md)
splits apply into a staging stage that changes nothing and a commit stage that runs the
commands; **a dry-run is the staging stage with the files kept in memory instead of
written, and the commit stage printed instead of run.** The plan it shows is the plan
apply would execute, and the checks it makes are the checks apply makes, so there is no
class of failure that a dry-run passes and an apply then hits at the same point.

The one command a dry-run may run is `nft --check`, which parses the ruleset and validates
it against the kernel without installing anything. It is worth the exception because a
ruleset nft refuses is the single most likely reason an apply fails at all. Where nft is
not available — a dry-run on a laptop — the check is skipped and the output says it was
skipped, rather than implying the ruleset was validated.

### What a dry-run prints

- The renderers' warnings first, before any diff. A declaration that could not be rendered
  as written ([ADR 0012](0012-networkd-rendering.md) has two) is more important than any
  line of the diff below it, and it is the reason dry-run exists at all.
- Every file that would be added or removed, and a unified diff of every file that would
  change. Files that would not change are counted, not listed.
- The nftables table, as a diff against the ruleset the last apply installed, or in full
  when there is none to compare with.
- Each kernel switch that would change, with the value now and the value after.
- The commands the commit stage would run, in order, and which phase each belongs to.
- A closing line saying whether anything would change at all. **"Nothing to do" has to be
  unmistakable**, because on an idempotent engine it is the answer most of the time and an
  operator reading past it is how a change gets applied twice.

Exit status says whether the dry-run succeeded, not whether it found a difference. A
difference is the expected outcome, and overloading the exit code would make a failed
staging check indistinguishable from ordinary work to do.

### A secret is one line: path, mode, and whether it changed

A credentials file never appears as content and never appears as a diff. It appears as its
path, the mode it would be written with, and whether its content would change — which
reveals nothing and is exactly what an operator needs, because it is what decides whether
the session will be restarted.

The guarantee is structural rather than a rule to remember: the value is read, used, and
dropped inside the staging stage, and it is never put into the plan the printer walks
([ADR 0004](0004-apply-model.md)). There is no code path from printing a plan to printing
a credential.

The DUID is the exception ADR 0003 already argued for, and it is printed in full. The
question a prefix delegation raises is why the prefix changed, and the DUID in effect is
what answers it.

### A dry-run does not need the line to be up

*Amended by [ADR 0015](0015-uplink-addresses-in-sets.md), which is built: the ruleset is
no longer one of the things that depends on a runtime value. Both dry-runs now show the
same ruleset — the hairpin rules match on the uplink's set whether or not the line is up —
and what differs is the line beside it saying which addresses this apply would put into
that set. A host whose line is not up is told so as a note and shows no elements. The
point of the section is unchanged: nothing has to be waited for. **What the second
paragraph below describes is what a dry-run showed before that record.***

Missing runtime values are reported as missing and what depends on them is left out,
exactly as apply would leave it out.

A dry-run on a host whose session has not dialled therefore shows a ruleset without the
hairpin rules, and one taken after the line came up shows them. That difference is not
noise: it is the same difference ADR 0004's last phase re-applies, made visible.

`regied render`, which reads no host at all, is complete on its own for the same reason:
the ruleset needs no runtime value, so nothing in it is left out for want of one.

## Consequences

- The task's completion condition is met by running `regied apply --dry-run` against
  `config/example.yaml`: the nftables ruleset, the dnsmasq configuration, and the routing
  the policies produce are all in the output.
- The diff is regied's own, computed between text it rendered and text it read. There is
  no dependency on an external diff program, and none is needed at apply time either. Two
  things it cannot show are said in words instead: a change that is only a trailing
  newline, which is still a rewrite and still restarts whatever reads the file, and a
  change that is only a file's mode.
- The same honesty the skipped `nft --check` gets is owed to the probe beside it. A
  machine with no nft cannot say whether regied's table is in the kernel either, and
  answering "it is not" would make every preview taken away from the host report that the
  whole ruleset goes in.
- `regied render` gives the netns testbed a way to inspect a rendering without a host, and
  gives a reviewer a way to see the effect of a schema change in a pull request.
- Printing "nothing to do" correctly requires the idempotence rule in ADR 0004 to hold at
  every artefact, so the two decisions check each other: a dry-run that keeps reporting a
  change nobody made is how a broken comparison is discovered.
- The output is written for a person at a console during an outage, which rules out
  making it the machine-readable interface as well. Anything that wants the plan
  structured should ask the state API for it.
