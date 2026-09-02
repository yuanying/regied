# ADR 0014: Give pppd its credentials in a second options file

- Status: Accepted (2026-09-02)

## Context

A PPPoE session needs two values regied is not allowed to write down: the account name
and the password. [ADR 0003](0003-secrets-out-of-configuration.md) says the schema names
the files they are in and nothing regied emits for diagnosis may carry either — not
`--dry-run` output, not a diff, not a log line. `PPPoESession.userIDFile` and
`passwordFile` are what the operator writes; the question this record answers is what
regied does with the values behind them.

pppd offers three places to put a credential, and each of them decides something else
along with it.

**Its own secrets files.** `/etc/ppp/pap-secrets` and `/etc/ppp/chap-secrets` are where
ppp expects a password to live. Both are at fixed paths that ppp itself installs and that
anything else on the host using ppp shares, so rewriting one is exactly the case
[ADR 0009](0009-ownership-boundary.md) rules out: regied would be editing a file it did
not create. And it would not even hold both halves. pppd picks the entry matching the
name given by the `user` option, which is an option like any other — so the account name
would still have to be written into the options file, in the clear, in a file whose whole
purpose is to be printable.

**One options file, as the reference testbed does.** `hack/netns/router/reference.sh`
writes every option and both credentials into a single file under `umask 077`. That works,
and it is the right shape for a hand-written script, but it makes the entire configuration
of the link secret. Nothing about the session could then be shown: not the MTU, not
whether it installs a default route, not why the link is named what it is. The half of
`--dry-run` that matters for a PPPoE session would be blank.

**`passwordfd.so`.** The plugin reads the password from a file descriptor. It covers only
the password — the account name is still an option — and it moves a credential out of what
was rendered and into how the process happens to be started, which is a worse place to
reason about it.

## Decision

**Render two options files per session, and give pppd both.**

- **The peer file** holds every option that follows from the configuration: the plugin and
  the interface to dial over, the pinned link name, MTU and MRU, the default route and its
  metric, persistence and the LCP echoes that make persistence fire. It is ordinary
  configuration, world-readable, printed whole by `--dry-run`, and diffed.
- **The credentials file** holds `user` and `password` and nothing else. It is written
  with mode `0600` in a directory with mode `0700`, and it is reported by path and mode
  rather than by content, as ADR 0003 requires.
- pppd is invoked with both, as two `file` options. Options accumulate across them, so the
  session pppd sees is the two files put together.
- The renderer never opens either credential file. It produces the peer file from the
  configuration alone, and the credentials file's content comes from a separate call that
  takes the two values as an argument. Reading the files, and holding the values for as
  long as writing takes, belongs to the apply engine.

Both halves were checked against the platform ADR 0011 names: ppp 2.5.2 on Debian 13
parses the two files together and reports the options in effect, naming the second file
as the source of `user` and `password`.

## Consequences

- `--dry-run` can show a PPPoE session in full. Everything an operator would look at
  before applying is in the file that is safe to print, and the one file that is not
  reduces to a line saying it would be written and with what mode.
- Rotating a credential produces no diff, because the peer file does not change. It is a
  rewrite of one 0600 file and a restart of the session, as ADR 0003 anticipated.
- The account name is protected as well as the password is. Under any arrangement built on
  `pap-secrets` it would not have been.
- There are two files per session to write, to protect, and to reclaim. Both live under
  regied's own directory, so reclaiming them is the same directory walk as for everything
  else it owns.
- Nothing under `/etc/ppp/` is touched. A host that already runs ppp for something else
  keeps its secrets files and its peers untouched.
