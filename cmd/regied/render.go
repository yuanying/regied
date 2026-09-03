package main

import (
	"fmt"
	"io"
	"net/netip"

	"github.com/yuanying/regied/internal/apply"
	"github.com/yuanying/regied/internal/config"
)

// renderCommand prints what a configuration means, without reading the host.
//
// The values that exist only at apply time are given on the command line, and what
// depends on one that was not given is left out exactly as an apply would leave it out.
// Nothing here opens a credential file: the file that holds one is reported by path and
// mode, which is all it could be reported by anyway (ADR 0003).
func renderCommand(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("render", stderr)
	path := flags.String("config", DefaultConfigPath, "the configuration to render")
	aftr := pairs{}
	duid := pairs{}
	uplink := pairs{}
	flags.Var(aftr, "aftr", "a DS-Lite tunnel's resolved AFTR address, as name=address (repeatable)")
	flags.Var(duid, "duid", "the contents of a DUID file, as path=value (repeatable)")
	flags.Var(uplink, "uplink-address", "an address an uplink is holding, as name=address (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*path, config.WithSecretFiles(namedFilesOnly{}))
	if err != nil {
		return reportError(stderr, err)
	}

	runtime, err := runtimeFromFlags(aftr, duid, uplink)
	if err != nil {
		return reportError(stderr, err)
	}

	plan, err := apply.New(apply.Host{}, apply.Options{}).Render(cfg, runtime)
	if err != nil {
		return reportError(stderr, err)
	}
	apply.Report(stdout, plan)
	return 0
}

// runtimeFromFlags builds the apply-time values out of what was written on the command
// line. Credentials are deliberately not among them.
func runtimeFromFlags(aftr, duid, uplink pairs) (*apply.Runtime, error) {
	runtime := &apply.Runtime{}
	runtime.Networkd.AFTRAddresses = map[string]netip.Addr{}
	runtime.Networkd.DUIDs = map[string]string{}
	runtime.NFTables.UplinkAddresses = map[string][]netip.Addr{}

	for name, value := range aftr {
		address, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("-aftr %s=%s: %w", name, value, err)
		}
		runtime.Networkd.AFTRAddresses[name] = address
	}
	for path, value := range duid {
		runtime.Networkd.DUIDs[path] = value
	}
	for name, value := range uplink {
		address, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("-uplink-address %s=%s: %w", name, value, err)
		}
		runtime.NFTables.UplinkAddresses[name] = append(runtime.NFTables.UplinkAddresses[name], address)
	}
	return runtime, nil
}

// namedFilesOnly answers validation's question about the files a configuration names
// without looking at any.
//
// Rendering is about the configuration, not about the host it would be applied to, and a
// declaration should be printable on a machine that holds none of its secrets. An apply
// checks them for real, and refuses a configuration whose credentials are missing.
type namedFilesOnly struct{}

func (namedFilesOnly) CheckSecretFile(string) error { return nil }
