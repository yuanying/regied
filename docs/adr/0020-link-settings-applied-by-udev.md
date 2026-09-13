# ADR 0020: A link's NIC settings go in a .link file, and udev applies them

- Status: Accepted (2026-09-13)

## Context

Some properties of a link belong to the NIC rather than to its network configuration. The
first one to reach the schema is the Wake-on-LAN mode, `Interface.spec.wakeOnLan`
([`docs/spec/kinds.md`](../spec/kinds.md#wakeonlan)). A host whose links regied takes over
from another tool loses it at the next boot unless regied writes it, and nobody notices until
the host is off and does not wake.

systemd takes that setting as `WakeOnLan=` in a `.link` file, and a `.link` file is not
systemd-networkd's. It is read by systemd-udevd, whose `net_setup_link` builtin applies it
to a network device when udev handles an event for that device. `networkctl reload` does
not read it. regied has written no `.link` until now, so three things
[ADR 0004](0004-apply-model.md) and [ADR 0012](0012-networkd-rendering.md) settled for the
networkd files have to be settled again for this one: what the file matches on, how an apply
makes it take effect on a link that is already up, and what taking the file away means.

What follows was checked against the source of systemd 255 (Ubuntu 24.04) and 257
(Debian 13), the versions [ADR 0011](0011-target-platform.md) targets. The two behave the
same on every point below.

- udev applies a `.link` file on the `add`, `bind` and `move` events of a device, and on no
  other. Naming — `Name=`, `NamePolicy=`, the alternative names — is applied on `add` only.
- **The first matching `.link` file, in lexical order across the directories, is applied
  and every later one is ignored**, including the distribution's `99-default.link`, which is
  where the naming policy that gives a NIC its predictable name lives.
- The `[Match]` section of a `.link` file has no `Name=`. What comes closest is
  `OriginalName=`, which matches the name the device has at the event being handled.
- systemd-networkd hears the same event. For a link it already manages it reconfigures only
  if the `.network` file that matches the link is a different one than before.
- udevd checks whether its configuration files changed at most every three seconds, when an
  event arrives.

## Decision

### One `.link` file per Interface that declares a NIC setting

**`50-regied-<resource name>.link`, beside the `.network`, written only when the Interface
declares a setting that belongs in it.** It carries the same prefix, so it is regied's to
rewrite and to reclaim by the rule ADR 0012 gave the directory, and reclaiming it is the same
glob. The number matters more here than for a `.network`: a `.link` behind `99-default.link`
would never be considered at all.

### `[Match]` names the declared interface name, as `OriginalName=`

| Candidate | Why not |
|---|---|
| `MACAddress=`, `PermanentMACAddress=` | The declaration holds no MAC. Adding one would write a fact about the hardware a second time, and it goes stale when the NIC is replaced |
| `Path=`, `Driver=` | Facts about the hardware, which the declaration does not hold either |
| `Name=` | A `.link` file has no such match. `Name=` in `[Link]` is what gives a device a name |
| **`OriginalName=`** | Matches the name the device has when udev handles the event. This is what the declaration can say |

`OriginalName=` is documented as the name the kernel gave the device, and at the `add` event
that is what it is: a NIC appears as, say, `eth0`, and the declared name is the predictable
one it is about to get. So at `add` regied's file does not match, the distribution's default
file does, and the device is renamed. **The rename is itself an event.** The kernel sends
`move`, udev handles it with the device already holding the declared name, regied's file is
now the first match, and its settings are applied. Names are given on `add` only, so the
`move` changes nothing about the name and loses nothing the default file gave.

That is what makes matching by the declared name correct at boot rather than only when an
apply runs, which is the failure this setting exists to prevent: a file that matched on the
apply and not on the next boot would look right until the host is off.

Two consequences are worth writing down.

- **A link whose kernel name is already the declared one is matched at `add`**, which is the
  case on a host where predictable naming is turned off. The file then stands in place of the
  distribution's default for that device: no naming policy runs, which leaves the device with
  the name the declaration already says, and no alternative names are added. udev also logs
  that it matched a kernel-assigned name, which is its warning that such names are not stable
  across reboots.
- The file does not carry the naming policy. Copying the distribution's default into it would
  make regied the author of how every declared NIC is named, which it is not, and it would
  have to follow the distribution's changes to that file.

### An apply asks udev to apply the file to the link that is up

Writing the file is not an effect ([ADR 0004](0004-apply-model.md)), and here the reload
networkd gets does not make it one. What does is the sequence systemd.link(5) documents:

1. `udevadm control --reload`, so that udevd reads the file now. Its own check runs at most
   every three seconds, and an event right after a write could otherwise be handled with the
   old files.
2. `udevadm trigger --settle --action=add /sys/class/net/<ifname>`, which gives the device a
   synthetic `add` event and waits for udev to finish it.

The example in systemd.link(5) takes the link down first. regied does not: that is for
settings a device cannot take while it is up, such as its name and its MAC, and the setting
this record is about is one the NIC takes while up. What the synthetic event does to a link
that is up is, from the source, this and nothing else.

- udev matches regied's file by the current name, which is the declared one, and applies what
  the file says.
- It does not rename the device: the device already holds a name userspace gave it, and
  regied's file carries no naming policy in any case.
- networkd hears the event, finds the same `.network` matching the link, and does not
  reconfigure it.

**So it takes nothing down, and an unattended turn may run it** ([ADR 0016](0016-converging-on-the-accepted-declaration.md)),
under backoff like every other command.

The two commands go in phase 3 of ADR 0004's order, after the networkd reload, and each runs
only when something it is about changed.

| What changed | What runs |
|---|---|
| A `.network` or `.netdev` file | `networkctl reload`, as before. A `.link` file alone is not a reason: networkd does not read it |
| A `.link` file, written or reclaimed | `udevadm control --reload` |
| A `.link` file written, for a link that is on the host | `udevadm trigger` for that link |

**A link that is not on the host is not triggered.** There is no device to give the event to,
and asking fails. The file is in place, udev applies it when the device appears, and the plan
says so as a note rather than as something waited for: the declaration holds as far as the
host can hold it.

### Taking the file away does not turn the setting off

Reclaiming the file changes what udev applies the next time the device appears. It does not
put a NIC setting back, and nothing can: what the NIC held before regied is not known, which
is [ADR 0009](0009-ownership-boundary.md)'s question about state with no marker. The NIC keeps
the mode it has until the device is added again — the next boot, typically — and then it has
whatever the driver or the firmware gives it. This is the same line ADR 0004 draws for a link
that stops being declared. **To turn Wake-on-LAN off, the declaration says `off`.**

So a reclaim reloads udevd and triggers nothing. There is no rollback to add either:
[ADR 0016](0016-converging-on-the-accepted-declaration.md) has none, and going back is a
person applying the previous declaration, which either reclaims the file as above or rewrites
it and triggers.

## Consequences

- regied writes a third kind of file into `/etc/systemd/network/`, and the reclaim glob, the
  ownership marker and the golden rendering cover it without change.
- **What the loop sees of this setting is the file.** Somebody who changes the NIC's mode by
  hand is not taken back by a turn, because the kernel's view of the NIC is not something a
  turn reads. The file is, and a file somebody edited or deleted is put back and applied again.
  This is the line [ADR 0016](0016-converging-on-the-accepted-declaration.md) draws around what
  the loop can see.
- The next NIC setting that reaches the schema goes in the same file, and the reasoning about
  `[Match]` and the trigger holds for it only if the NIC takes it while up. A setting that
  needs the link down is a different record.
- On a host where another tool writes a `.link` file for the same device with a lower number —
  netplan writes its own under `/run/systemd/network/` with `10-` — that file wins and
  regied's is ignored. Taking a link over from such a tool means removing its configuration,
  which a takeover does anyway.
