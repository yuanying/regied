package apply

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/networkd"
	"github.com/yuanying/regied/internal/render/pppd"
)

// Runtime is everything an apply needs that a configuration cannot hold, gathered from
// the host it is running on: the address a provider's AFTR name resolves to, the contents
// of a DUID file, the addresses each uplink is currently holding, and the credentials
// behind a PPPoE session (ADR 0004).
//
// The credentials are the reason this type is not printed. They live here only until the
// files that need them have been rendered, and nothing that reaches an operator's screen
// walks this structure (ADR 0003).
type Runtime struct {
	Networkd networkd.Runtime

	// UplinkAddresses is what each uplink's link is holding, by the uplink's name. It is
	// not rendered — the ruleset names a set per uplink and carries no address
	// (ADR 0015) — it is what the firewall phase puts into those sets.
	UplinkAddresses map[string][]netip.Addr

	// Credentials is the values behind each PPPoESession's userIDFile and passwordFile,
	// by the session's name.
	Credentials map[string]pppd.Credentials

	// Notes is what the host could not answer without that being a failure: a link that
	// is not up, which is ordinary before the line has dialled and is why its uplink set
	// has nothing to be seeded with; an AFTR name that does not resolve yet; a DUID file
	// that cannot be read yet. The last two leave an artifact out of this turn, and the
	// plan says what it waits for (ADR 0016).
	Notes []string
}

// RuntimeError is what CollectRuntime returns when the host cannot answer something a
// turn cannot proceed without. It reports everything it found rather than the first
// thing, because a host being set up for the first time is usually missing several and
// an operator should learn all of them in one run.
//
// Only the credentials are in this class now. A value that is missing is, under a loop,
// only missing now: the artifact depending on it is left out and picked up on a later
// turn (ADR 0016). A credential is the exception, because bringing a line up without
// authentication is not a degraded success and there is no smaller version of a
// credentials file to write (ADR 0003).
type RuntimeError struct{ Problems []string }

func (e *RuntimeError) Error() string {
	return "cannot read what only this host knows:\n  " + strings.Join(e.Problems, "\n  ")
}

// CollectRuntime reads the host for the values the renderers take as arguments.
func CollectRuntime(ctx context.Context, cfg *config.Config, host Host) (*Runtime, error) {
	c := &collector{
		cfg:  cfg,
		host: host,
		runtime: Runtime{
			Networkd: networkd.Runtime{
				AFTRAddresses: make(map[string]netip.Addr),
				DUIDs:         make(map[string]string),
			},
			UplinkAddresses: make(map[string][]netip.Addr),
			Credentials:     make(map[string]pppd.Credentials),
		},
	}

	c.collectDUIDs()
	c.collectCredentials()
	c.resolveAFTRs(ctx)
	c.readLinkAddresses()

	if len(c.problems) > 0 {
		return nil, &RuntimeError{Problems: c.problems}
	}
	return &c.runtime, nil
}

type collector struct {
	cfg      *config.Config
	host     Host
	runtime  Runtime
	problems []string
}

func (c *collector) failf(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

func (c *collector) notef(format string, args ...any) {
	c.runtime.Notes = append(c.runtime.Notes, fmt.Sprintf(format, args...))
}

// readSecret reads one file named by the configuration.
//
// What was read is never put in the problem it reports, only the path and the reason. A
// failure to read a credential file is exactly the moment when what was read so far
// could end up in a log (ADR 0003).
func (c *collector) readSecret(kind, name, field, path string) (string, bool) {
	data, _, err := c.host.Files.ReadFile(path)
	if err != nil {
		c.failf("%s/%s: %s %s cannot be read: %v", kind, name, field, path, err)
		return "", false
	}
	if len(data) == 0 {
		c.failf("%s/%s: %s %s is empty", kind, name, field, path)
		return "", false
	}
	return string(data), true
}

func (c *collector) collectDUIDs() {
	for _, iface := range config.ResourcesOf[*v1alpha1.InterfaceSpec](c.cfg) {
		if iface.Spec.DHCPv6 == nil || iface.Spec.DHCPv6.PrefixDelegation == nil {
			continue
		}
		path := iface.Spec.DHCPv6.PrefixDelegation.DUIDFile
		if path == "" {
			continue
		}
		if _, done := c.runtime.Networkd.DUIDs[path]; done {
			continue
		}
		// A DUID that cannot be read is not handed to the renderer, which then leaves
		// the link's file out whole rather than write a prefix delegation under which
		// networkd sends an identifier of its own and the delegated prefix changes
		// (ADR 0004, ADR 0012). The file is read again next turn (ADR 0016).
		data, _, err := c.host.Files.ReadFile(path)
		switch {
		case err != nil:
			c.notef("Interface/%s: the DUID file %s cannot be read: %v", iface.Name, path, err)
		case len(data) == 0:
			c.notef("Interface/%s: the DUID file %s is empty", iface.Name, path)
		default:
			c.runtime.Networkd.DUIDs[path] = string(data)
		}
	}
}

func (c *collector) collectCredentials() {
	for _, session := range config.ResourcesOf[*v1alpha1.PPPoESessionSpec](c.cfg) {
		userID, gotUser := c.readSecret("PPPoESession", session.Name, "the user ID file", session.Spec.UserIDFile)
		password, gotPassword := c.readSecret("PPPoESession", session.Name, "the password file", session.Spec.PasswordFile)
		if !gotUser || !gotPassword {
			continue
		}
		c.runtime.Credentials[session.Name] = pppd.Credentials{UserID: userID, Password: password}
	}
}

// resolveAFTRs looks up the AFTR name of every tunnel that named one.
//
// The answer has to be an IPv6 address. The tunnel being configured is what carries this
// host's IPv4, so an IPv4 answer would describe a road that does not exist yet
// (ADR 0004). A name that does not resolve, or resolves to no IPv6 address, is not
// handed over, and the renderer leaves the tunnel out: on a host that has just booted the
// resolver is not up yet, and the next turn asks again (ADR 0016).
func (c *collector) resolveAFTRs(ctx context.Context) {
	for _, tunnel := range config.ResourcesOf[*v1alpha1.DSLiteTunnelSpec](c.cfg) {
		name := tunnel.Spec.AFTRHost
		if name == "" {
			continue
		}
		answers, err := c.host.Resolver.LookupHost(ctx, name)
		if err != nil {
			c.notef("DSLiteTunnel/%s: the AFTR %s cannot be resolved: %v", tunnel.Name, name, err)
			continue
		}
		var chosen netip.Addr
		for _, answer := range answers {
			if answer.Is6() && !answer.Is4In6() && answer.IsGlobalUnicast() {
				chosen = answer
				break
			}
		}
		if !chosen.IsValid() {
			c.notef("DSLiteTunnel/%s: the AFTR %s resolves to no IPv6 address, and this tunnel is what would carry IPv4", tunnel.Name, name)
			continue
		}
		c.runtime.Networkd.AFTRAddresses[tunnel.Name] = chosen
	}
}

// readLinkAddresses reads what each uplink's link is holding.
//
// A link that is not there is not an error: before the line has dialled there is no PPPoE
// link, and its set stays empty. An empty set matches nothing, which is what a hairpin
// rule should do while the line is down (ADR 0015).
func (c *collector) readLinkAddresses() {
	addresses, missing := readUplinkAddresses(c.cfg, c.host)
	c.runtime.UplinkAddresses = addresses
	if len(missing) > 0 {
		c.notef("no address was read from %s; the uplink set stays empty, so anything matching on it matches nothing until the line is up", strings.Join(missing, ", "))
	}
}

// readUplinkAddresses reads what each uplink is holding, and names the ones that answered
// nothing. Only an uplink's address is anything the ruleset refers to — the hairpin half
// of a port forward matches on the uplink's set (ADR 0015) — so only the uplinks are
// asked, and a LAN link is not reported as missing an address it never needed.
func readUplinkAddresses(cfg *config.Config, host Host) (map[string][]netip.Addr, []string) {
	addresses := make(map[string][]netip.Addr)
	var missing []string
	for _, link := range uplinkResources(cfg) {
		held, err := host.Links.Addresses(link.ifname)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s)", link.name, link.ifname))
			continue
		}
		var usable []netip.Addr
		for _, address := range held {
			if isReachableAddress(address) {
				usable = append(usable, address)
			}
		}
		if len(usable) == 0 {
			missing = append(missing, fmt.Sprintf("%s (%s)", link.name, link.ifname))
			continue
		}
		slices.SortFunc(usable, func(a, b netip.Addr) int { return a.Compare(b) })
		addresses[link.name] = usable
	}
	return addresses, missing
}

// isReachableAddress is whether an address is one something outside could have resolved.
// A link-local or loopback address is not, so it is not one a hairpin rule could match.
func isReachableAddress(address netip.Addr) bool {
	return address.IsGlobalUnicast() && !address.IsLinkLocalUnicast()
}

// link is one link resource with the kernel name it puts on the host.
type link struct {
	name   string
	ifname string
}

// uplinkResources is every resource that leads outward, with its kernel name. A PPPoE
// session and a DS-Lite tunnel are named after the resource (ADR 0012).
func uplinkResources(cfg *config.Config) []link {
	var out []link
	for _, session := range config.ResourcesOf[*v1alpha1.PPPoESessionSpec](cfg) {
		out = append(out, link{name: session.Name, ifname: session.Name})
	}
	for _, tunnel := range config.ResourcesOf[*v1alpha1.DSLiteTunnelSpec](cfg) {
		out = append(out, link{name: tunnel.Name, ifname: tunnel.Name})
	}
	return out
}
