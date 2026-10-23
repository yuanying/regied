package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/ddns"
)

// Host is everything the engine is allowed to touch outside itself. Every part of it is
// an interface, which is what lets the whole engine be exercised by unit tests that need
// neither root, nor a network, nor any of nft, networkctl, systemctl and pppd being
// installed (ADR 0004).
type Host struct {
	Files    FileSystem
	Runner   Runner
	Resolver Resolver
	Links    Links
	Sysctl   Sysctl
	Units    Units

	// Delegations is networkd's answer to which prefix each prefix-delegation client
	// holds. Left nil, nothing is read and no delegation is known, which is what
	// `regied render` wants (ADR 0021).
	Delegations DelegationReader

	// Advertisements is networkd's answer to which addresses each link took from a
	// router advertisement, which is how a host behind a router knows the delegation its
	// LAN is advertised a subnet of. Left nil, nothing is read (ADR 0023).
	Advertisements AdvertisementReader

	// DNS is the provider a DNSRecordSet's records are written at. It is an interface
	// for the same reason the rest of this structure is: a turn in a test writes to
	// nothing, and `make test` opens no socket (ADR 0019).
	DNS ddns.Provider

	// Clock and Locker are what a turn adds to the list (ADR 0016): the time a report
	// records, and the lock a turn holds while it runs. Left nil, they are the ones this
	// process is running on.
	Clock    Clock
	Locker   Locker
	Control  Control
	Timer    Timer
	Notifier Notifier
}

// OSHost is the host this process is running on.
func OSHost() Host {
	return Host{
		Files:          OSFileSystem{},
		Runner:         OSRunner{},
		Resolver:       OSResolver{},
		Links:          OSLinks{},
		Delegations:    OSDelegations{},
		Advertisements: OSDelegations{},
		Sysctl:         OSSysctl{},
		Units:          OSUnits{},
		DNS:            ddns.NewCloudflare(nil),
		Clock:          OSClock{},
		Locker:         OSLocker{},
		Control:        OSControl{},
		Timer:          OSTimer{},
		Notifier:       OSNotifier{},
	}
}

// Notifier exposes the daemon's convergence state through the service manager.
type Notifier interface{ Status(string) error }
type noopNotifier struct{}

func (noopNotifier) Status(string) error { return nil }

// OSNotifier sends sd_notify datagrams when systemd supplied NOTIFY_SOCKET.
type OSNotifier struct{}

func (OSNotifier) Status(status string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	if socket[0] == '@' {
		socket = "\x00" + socket[1:]
	}
	address := &net.UnixAddr{Name: socket, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1\nSTATUS=" + status))
	return err
}

// Clock is what a report reads the time from. It is an interface so that a test can say
// when a state was entered.
type Clock interface {
	Now() time.Time
}

// OSClock is this process's clock.
type OSClock struct{}

func (OSClock) Now() time.Time { return time.Now() }

type Timer interface {
	After(time.Duration) <-chan time.Time
}
type OSTimer struct{}

func (OSTimer) After(duration time.Duration) <-chan time.Time { return time.After(duration) }

// Locker takes a lock across processes on a path, and blocks until it has it or the
// context is done. What it returns releases the lock.
type Locker interface {
	Lock(ctx context.Context, path string) (release func() error, err error)
}

// OSLocker locks with flock(2), which the kernel releases when the holder dies, so a turn
// that crashes cannot leave the next one waiting for ever.
type OSLocker struct{}

// Lock polls rather than blocks in flock, because a blocked flock cannot be given up when
// the context is: the poll is what lets a caller stop waiting.
func (OSLocker) Lock(ctx context.Context, path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return func() error {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			file.Close()
			return err
		}
		return file.Close()
	}, nil
}

// FileSystem is the files the engine reads, writes and reclaims.
//
// It is deliberately smaller than os: the engine writes whole files and removes whole
// files, and never appends or truncates in place.
type FileSystem interface {
	// ReadFile returns a file's contents and permission bits. It returns an error
	// matching fs.ErrNotExist when there is no such file.
	ReadFile(path string) ([]byte, fs.FileMode, error)

	// WriteFile replaces a file, creating it if it is not there.
	WriteFile(path string, data []byte, mode fs.FileMode) error

	// MkdirAll creates a directory and its parents.
	MkdirAll(path string, mode fs.FileMode) error

	// Remove deletes a file. Removing a file that is not there is not an error.
	Remove(path string) error

	// List is the names of the regular files directly in a directory, sorted. A
	// directory that is not there is empty rather than an error: reclaiming from a
	// directory nothing has been written to yet is ordinary.
	List(dir string) ([]string, error)
}

// Runner runs one external command.
//
// The engine uses it for two kinds of thing, and the difference matters: `nft --check`
// and `nft list table` change nothing and are run in the staging stage, including during
// a dry-run; everything else is an effect and belongs to the commit stage (ADR 0004).
type Runner interface {
	Run(ctx context.Context, cmd Command) ([]byte, error)
}

// Units observes whether a declared process is active. Activity is drift; process
// health beyond systemd's active state is deliberately outside reconciliation.
type Units interface {
	Active(context.Context, string) (bool, error)
}

type OSUnits struct{}

func (OSUnits) Active(ctx context.Context, unit string) (bool, error) {
	err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return false, err
}

// Command is one external command. Stdin is what is fed to it, which is how a ruleset
// reaches nft without a temporary file anybody could read.
type Command struct {
	Name  string
	Args  []string
	Stdin string
}

// String is the command as it would be typed, for the dry-run output and for a log line.
// Stdin is not part of it: it is the ruleset, which is printed on its own.
func (c Command) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Resolver looks up a host name. Only one thing in the schema is a name rather than an
// address — a provider's AFTR — and it is behind this interface so that rendering a
// configuration in a test never asks a real resolver.
type Resolver interface {
	LookupHost(ctx context.Context, name string) ([]netip.Addr, error)
}

// ErrLinkNotFound is what Links returns for a link that is not on the host. It is the
// ordinary answer for an uplink that has not dialled yet, not a failure.
var ErrLinkNotFound = errors.New("no such link")

// Links reads the addresses a link is holding. It is the one piece of kernel state the
// engine reads, and it is read because the hairpin half of a port forward has to match on
// the address clients inside resolved: the ruleset names a set per uplink, and this is
// what the apply seeds that set with (ADR 0015).
type Links interface {
	Addresses(ifname string) ([]netip.Addr, error)
}

// DelegationReader reads the prefixes a link's DHCPv6 client holds. The answer comes from
// the party holding the lease, as it describes the link, not from an address networkd
// derived from a file regied wrote (ADR 0009, ADR 0021). A link whose client holds
// nothing answers none, which is not an error.
type DelegationReader interface {
	DelegatedPrefixes(ctx context.Context, ifname string) ([]netip.Prefix, error)
}

// AdvertisementReader reads the addresses networkd configured on a link from router
// advertisements, as networkd describes the link. On a host behind a router the address
// carrying the declared token is what the delegation is read from (ADR 0023). A link
// that has taken none answers none, which is not an error.
type AdvertisementReader interface {
	AdvertisedAddresses(ctx context.Context, ifname string) ([]config.AdvertisedAddress, error)
}

// --- the implementations that talk to this host ----------------------------------

// OSDelegations asks this host's networkd, through the description `networkctl status
// --json` prints for a link. It is the same object networkd's D-Bus Describe call
// returns; the command is used so that regied needs no D-Bus client.
type OSDelegations struct{}

func (OSDelegations) DelegatedPrefixes(ctx context.Context, ifname string) ([]netip.Prefix, error) {
	out, err := OSRunner{}.Run(ctx, Command{Name: "networkctl", Args: []string{"status", "--json=short", "--", ifname}})
	if err != nil {
		return nil, err
	}
	return parseDelegatedPrefixes(out)
}

// AdvertisedAddresses reads the same description for the addresses networkd took from
// router advertisements.
func (OSDelegations) AdvertisedAddresses(ctx context.Context, ifname string) ([]config.AdvertisedAddress, error) {
	out, err := OSRunner{}.Run(ctx, Command{Name: "networkctl", Args: []string{"status", "--json=short", "--", ifname}})
	if err != nil {
		return nil, err
	}
	return parseAdvertisedAddresses(out)
}

// linkDescription is the part of networkd's description of a link that says what its
// DHCPv6 client was delegated. systemd 257 writes each prefix as its sixteen bytes.
type linkDescription struct {
	DHCPv6Client *struct {
		Prefixes []struct {
			RawPrefix    []int `json:"Prefix"`
			PrefixLength int   `json:"PrefixLength"`
		} `json:"Prefixes"`
	} `json:"DHCPv6Client"`
}

// parseDelegatedPrefixes reads the delegated prefixes out of a link's description. A
// description with no DHCPv6 client, or with a client holding nothing, has none.
func parseDelegatedPrefixes(data []byte) ([]netip.Prefix, error) {
	var description linkDescription
	if err := json.Unmarshal(data, &description); err != nil {
		return nil, fmt.Errorf("cannot read networkd's description of the link: %w", err)
	}
	if description.DHCPv6Client == nil {
		return nil, nil
	}
	var out []netip.Prefix
	for _, delegated := range description.DHCPv6Client.Prefixes {
		if len(delegated.RawPrefix) != 16 {
			return nil, fmt.Errorf("networkd describes a delegated prefix of %d bytes, want 16", len(delegated.RawPrefix))
		}
		var raw [16]byte
		for i, b := range delegated.RawPrefix {
			if b < 0 || b > 255 {
				return nil, fmt.Errorf("networkd describes a delegated prefix with a byte of %d", b)
			}
			raw[i] = byte(b)
		}
		prefix, err := netip.AddrFrom16(raw).Prefix(delegated.PrefixLength)
		if err != nil || delegated.PrefixLength < 0 {
			return nil, fmt.Errorf("networkd describes a delegated prefix of length %d", delegated.PrefixLength)
		}
		out = append(out, prefix)
	}
	return out, nil
}

// linkAddresses is the part of networkd's description of a link that lists its
// addresses. networkd names where each came from; an address from a router advertisement
// is "NDisc". The preferred lifetime is networkd's timestamp in microseconds, and an
// address that is preferred for ever has none.
type linkAddresses struct {
	Addresses []struct {
		Family                int     `json:"Family"`
		RawAddress            []int   `json:"Address"`
		ConfigSource          string  `json:"ConfigSource"`
		PreferredLifetimeUSec *uint64 `json:"PreferredLifetimeUSec"`
	} `json:"Addresses"`
}

// parseAdvertisedAddresses reads the IPv6 addresses networkd took from router
// advertisements out of a link's description, in the order networkd lists them.
func parseAdvertisedAddresses(data []byte) ([]config.AdvertisedAddress, error) {
	var description linkAddresses
	if err := json.Unmarshal(data, &description); err != nil {
		return nil, fmt.Errorf("cannot read networkd's description of the link: %w", err)
	}
	var out []config.AdvertisedAddress
	for _, described := range description.Addresses {
		if described.Family != syscall.AF_INET6 || described.ConfigSource != "NDisc" {
			continue
		}
		if len(described.RawAddress) != 16 {
			return nil, fmt.Errorf("networkd describes an IPv6 address of %d bytes, want 16", len(described.RawAddress))
		}
		var raw [16]byte
		for i, b := range described.RawAddress {
			if b < 0 || b > 255 {
				return nil, fmt.Errorf("networkd describes an address with a byte of %d", b)
			}
			raw[i] = byte(b)
		}
		preferred := uint64(math.MaxUint64)
		if described.PreferredLifetimeUSec != nil {
			preferred = *described.PreferredLifetimeUSec
		}
		out = append(out, config.AdvertisedAddress{Address: netip.AddrFrom16(raw), PreferredUntil: preferred})
	}
	return out, nil
}

// OSFileSystem is the filesystem this process is running on.
type OSFileSystem struct{}

func (OSFileSystem) ReadFile(path string) ([]byte, fs.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return data, info.Mode().Perm(), nil
}

// WriteFile writes through a temporary file in the same directory and renames it over
// the target, so that a reader never sees a half-written file and a crash never leaves
// one. The mode is set before the rename, so a credentials file is never briefly
// readable.
func (OSFileSystem) WriteFile(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)

	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (OSFileSystem) MkdirAll(path string, mode fs.FileMode) error {
	return os.MkdirAll(path, mode)
}

func (OSFileSystem) Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (OSFileSystem) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// Sysctl is the kernel switches spec.global asks for. They have no renderer, because
// they are not configuration handed to another implementation: they are writes to
// /proc/sys, which makes them the apply engine's (ADR 0004).
//
// Reading before writing is not an optimisation. It is what makes a switch reversible,
// and it is why a failed apply can put the previous values back (ADR 0005).
type Sysctl interface {
	// Get is the current value of one key, written in the dotted form sysctl uses.
	Get(key string) (string, error)
	Set(key, value string) error
}

// OSSysctl reads and writes this kernel's switches.
//
// The writes are done in place rather than through a temporary file and a rename, which
// is what the rest of the engine does: /proc/sys supports neither.
type OSSysctl struct{}

func sysctlPath(key string) string {
	return "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
}

func (OSSysctl) Get(key string) (string, error) {
	data, err := os.ReadFile(sysctlPath(key))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (OSSysctl) Set(key, value string) error {
	return os.WriteFile(sysctlPath(key), []byte(value+"\n"), 0o644)
}

// OSRunner runs commands on this host.
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, cmd Command) ([]byte, error) {
	command := exec.CommandContext(ctx, cmd.Name, cmd.Args...)
	if cmd.Stdin != "" {
		command.Stdin = strings.NewReader(cmd.Stdin)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return out, fmt.Errorf("%s: %w", cmd, ErrCommandNotFound)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return out, fmt.Errorf("%s: %w", cmd, err)
		}
		return out, fmt.Errorf("%s: %w: %s", cmd, err, message)
	}
	return out, nil
}

// OSResolver asks this host's resolver.
//
// The lookup is not restricted to IPv6 here. Which family an answer has to be in is a
// decision about the tunnel being configured, not about resolution, and it is made where
// the answer is used so that the reason can be reported (ADR 0004).
type OSResolver struct{}

func (OSResolver) LookupHost(ctx context.Context, name string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
}

// OSLinks reads this host's links.
type OSLinks struct{}

func (OSLinks) Addresses(ifname string) ([]netip.Addr, error) {
	link, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrLinkNotFound, ifname)
	}
	configured, err := link.Addrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(configured))
	for _, address := range configured {
		prefix, ok := address.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(prefix.IP)
		if !ok {
			continue
		}
		out = append(out, addr.Unmap())
	}
	return out, nil
}
