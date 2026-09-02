package networkd

import (
	"cmp"
	"fmt"
	"io/fs"
	"net/netip"
	"slices"
	"strings"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// Dir is where systemd-networkd reads its configuration and where these files go.
// networkd searches /etc ahead of /run and /usr/lib, so a file here wins over anything a
// distribution or another renderer left in those (ADR 0008).
const Dir = "/etc/systemd/network"

// FilePrefix is on the name of every file regied puts in Dir, and it is the ownership
// marker (ADR 0009): what carries it is regied's to rewrite and to reclaim, and what
// does not is somebody else's.
//
// The number is what decides which .network file takes a link, because networkd sorts
// the candidates by file name and the first match wins. 50 leaves both directions open:
// it is ahead of the 80- files systemd and the distribution ship, so the links regied
// declares are configured the way regied says, and behind the 10- range that hand-written
// overrides and other renderers conventionally use, so an operator can still put a file
// in front of one of ours without editing it.
const FilePrefix = "50-regied-"

// FileMode is the permission every file gets. Nothing in this backend holds a
// credential: the DUID is the one thing here that came out of a file named by the
// configuration, and it is deliberately not treated as a secret (ADR 0003).
const FileMode fs.FileMode = 0o644

// File is one file, ready to be written by the apply engine.
type File struct {
	Name    string
	Mode    fs.FileMode
	Content string
}

// Path is where the file goes.
func (f File) Path() string { return Dir + "/" + f.Name }

// Output is a whole rendering: every file, sorted by name, and what the renderer had to
// say about the configuration on the way.
//
// A warning is for a declaration that could not be rendered as written and was not
// rendered as written. It is not an error — the rest of the configuration is sound and
// applying it is better than refusing it — but it must reach the operator, which is why
// it is carried here rather than logged.
type Output struct {
	Files    []File
	Warnings []string
}

// Runtime carries the values a rendering needs that exist only at apply time. The
// renderer resolves no names and reads no files; the apply engine fills this in.
type Runtime struct {
	// AFTRAddresses is what each DSLiteTunnel's aftrHost resolved to, by resource name.
	// A tunnel written with aftrAddress needs no entry.
	AFTRAddresses map[string]netip.Addr

	// DUIDs is what each duidFile holds, by the path the configuration named. The DUID
	// is not a credential and is shown in dry-run output and in diagnostics (ADR 0003).
	DUIDs map[string]string
}

// Error is what Render returns for a configuration it cannot turn into files. It reports
// everything it found rather than the first thing, in the same way validation does.
type Error struct {
	Messages []string
}

func (e *Error) Error() string {
	return "cannot render the systemd-networkd configuration:\n  " + strings.Join(e.Messages, "\n  ")
}

// Render builds the systemd-networkd configuration the document asks for.
func Render(cfg *config.Config, rt Runtime) (*Output, error) {
	r := &renderer{cfg: cfg, rt: rt}
	r.index()

	for _, iface := range config.ResourcesOf[*v1alpha1.InterfaceSpec](cfg) {
		r.renderInterface(iface)
	}
	r.renderBridgeMembers()
	for _, tunnel := range config.ResourcesOf[*v1alpha1.DSLiteTunnelSpec](cfg) {
		r.renderDSLiteTunnel(tunnel)
	}
	for _, session := range config.ResourcesOf[*v1alpha1.PPPoESessionSpec](cfg) {
		r.renderPPPoESession(session)
	}

	slices.SortFunc(r.files, func(a, b File) int { return cmp.Compare(a.Name, b.Name) })
	r.checkNameCollisions()

	if len(r.errors) > 0 {
		return nil, &Error{Messages: r.errors}
	}
	return &Output{Files: r.files, Warnings: r.warnings}, nil
}

type renderer struct {
	cfg *config.Config
	rt  Runtime

	files    []File
	warnings []string
	errors   []string

	// interfaces is every Interface by resource name, for the references that name one.
	interfaces map[string]config.Named[*v1alpha1.InterfaceSpec]

	// enslavedBy maps a kernel interface name to the bridge that claims it. Members are
	// kernel names, not resource names, and a member need not be a resource at all.
	enslavedBy map[string]config.Named[*v1alpha1.InterfaceSpec]

	// policies is every EgressRoutePolicy that leaves by an uplink, by the uplink's
	// resource name, in the order the policies are evaluated in.
	policies map[string][]policy

	// tunnelOn is the DS-Lite tunnels stacked on an interface, by the interface's
	// resource name.
	tunnelOn map[string][]string
}

// policy is one EgressRoutePolicy with the two numbers regied derived for it.
type policy struct {
	name    string
	spec    *v1alpha1.EgressRoutePolicySpec
	routing config.PolicyRouting
}

func (r *renderer) index() {
	r.interfaces = make(map[string]config.Named[*v1alpha1.InterfaceSpec])
	r.enslavedBy = make(map[string]config.Named[*v1alpha1.InterfaceSpec])
	r.policies = make(map[string][]policy)
	r.tunnelOn = make(map[string][]string)

	for _, iface := range config.ResourcesOf[*v1alpha1.InterfaceSpec](r.cfg) {
		r.interfaces[iface.Name] = iface
		if iface.Spec.Bridge == nil {
			continue
		}
		for _, member := range iface.Spec.Bridge.Members {
			r.enslavedBy[member] = iface
		}
	}

	for _, tunnel := range config.ResourcesOf[*v1alpha1.DSLiteTunnelSpec](r.cfg) {
		r.tunnelOn[tunnel.Spec.UnderlayRef] = append(r.tunnelOn[tunnel.Spec.UnderlayRef], tunnel.Name)
	}

	for _, p := range config.ResourcesOf[*v1alpha1.EgressRoutePolicySpec](r.cfg) {
		routing, ok := r.cfg.PolicyRouting(p.Name)
		if !ok {
			r.errorf("EgressRoutePolicy/%s: no routing table and mark were derived", p.Name)
			continue
		}
		r.policies[p.Spec.EgressRef] = append(r.policies[p.Spec.EgressRef], policy{p.Name, p.Spec, routing})
	}
	// The order a policy's rule and route are written in is the order the policies are
	// evaluated in, which is also the order their numbers were allocated in.
	for egress := range r.policies {
		slices.SortStableFunc(r.policies[egress], func(a, b policy) int {
			if order := cmp.Compare(a.spec.FamilyOrDefault(), b.spec.FamilyOrDefault()); order != 0 {
				return order
			}
			if order := cmp.Compare(priorityOf(a.spec), priorityOf(b.spec)); order != 0 {
				return order
			}
			return cmp.Compare(a.name, b.name)
		})
	}
}

func priorityOf(spec *v1alpha1.EgressRoutePolicySpec) int {
	if spec.Priority == nil {
		return 1 << 30
	}
	return *spec.Priority
}

// add puts one rendered file into the output.
func (r *renderer) add(name string, u *unit) {
	r.files = append(r.files, File{Name: name, Mode: FileMode, Content: u.String()})
}

func (r *renderer) errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *renderer) warnf(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

// checkNameCollisions catches two resources that would render into the same file. File
// names are built from resource names and kernel interface names, and nothing in the
// schema keeps those from colliding across kinds.
func (r *renderer) checkNameCollisions() {
	for i := 1; i < len(r.files); i++ {
		if r.files[i].Name == r.files[i-1].Name {
			r.errorf("two resources render into %s", r.files[i].Path())
		}
	}
}

// newUnit starts a file, saying what it was generated from so that a file found on a
// host can be traced back to the resource that asked for it.
func newUnit(kind v1alpha1.ResourceKind, name string) *unit {
	return &unit{header: fmt.Sprintf("# Generated by regied from %s/%s. Do not edit.\n", kind, name)}
}

// fileName is the name of the file a resource renders into.
func fileName(name, extension string) string {
	return FilePrefix + name + extension
}
