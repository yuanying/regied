# ADR 0003: Keep secrets out of the configuration

- Status: Accepted (2026-08-30)

## Context

The configuration source we ported from carries a PPPoE user ID and password, password
hashes, and SSH public keys in the same file as the routing and firewall configuration.
That is normal for a vendor router OS, where the configuration file is not something
anybody shares.

regied's configuration is not like that. It is a file people will keep in version
control, paste into an issue when asking why a rule does not match, and hand to a
colleague. Every one of those is a way for a credential to escape, and the escape is
silent — nothing fails, so nobody notices.

The failure is not hypothetical. Preparing this project meant reading a real
configuration file and repeatedly having to remember which lines could not be quoted.

## Decision

**The configuration schema has no field that can hold a secret.**

- Credentials are named by the path of a file that holds them: `passwordFile`,
  `userIDFile`, and so on. There is no `password` field to fall back to, so there is no
  shortcut to take when in a hurry.
- The PPPoE **user ID is treated as a credential**, not as an identifier. It carries the
  provider and the subscriber, and it is half of what is needed to impersonate the line.
- Referenced files live outside the repository. The configuration in `config/` refers to
  paths under `/etc/regied/secrets/`; nothing under that path is part of this project.
- regied reads a referenced file at apply time, keeps the value only as long as it needs
  it to render backend configuration, and never writes it into anything it emits for
  diagnosis: not `--dry-run` output, not a diff, not a log line, not the state API.
- Backend configuration that must contain a secret — a pppd secrets file, for instance —
  is written with restrictive permissions and is not part of what `--dry-run` prints.
  `--dry-run` reports that the file would be written and with what mode, not its content.
- A missing or unreadable secret file is a validation error, not a warning. Bringing an
  uplink up without authentication is not a degraded success.

## Consequences

- A configuration file can be published without review. That is what makes the example
  in `config/` possible at all, and it is what lets somebody paste their real file into a
  bug report.
- Secret distribution becomes someone else's problem, which is correct: it is the same
  problem as getting the configuration file onto the host, and ADR 0009 already places
  distribution outside regied.
- There is a small cost at the console. Bringing up a link means two files, not one, and
  a first-time setup has one more step that can be forgotten. The validation error covers
  the forgetting.
- Rotating a credential does not change the configuration file, so it does not produce a
  diff and does not need an apply — only a restart of the process that reads it.
