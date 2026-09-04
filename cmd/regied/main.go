// Command regied puts a declared network configuration on one Linux host.
//
// It has two commands and the difference between them is whether the host is read.
// `render` answers what a configuration means, anywhere, about any host. `apply` reads
// this host and puts the configuration on it, and `apply --dry-run` shows what that
// would change without doing any of it (ADR 0006).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yuanying/regied/internal/config"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// commands is what regied can be asked to do. The HTTP API is not here yet.
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"render": renderCommand,
	"apply":  applyCommand,
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	}
	command, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "regied: there is no %q command\n\n", args[0])
		usage(stderr)
		return 2
	}
	return command(args[1:], stdout, stderr)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `regied manages one host's network configuration from a declaration.

Usage:
  regied render [flags]   Render a configuration and print it. Reads nothing.
  regied apply  [flags]   Put a configuration on this host.

Run a command with -h for its flags.
`)
}

// DefaultConfigPath is where regied looks for the declaration when nothing says
// otherwise. It is under regied's own directory, alongside the generated configuration
// it owns and the secrets directory the declaration refers to.
const DefaultConfigPath = "/etc/regied/config.yaml"

// newFlagSet builds a flag set that prints to the writer the caller chose rather than to
// stderr, so that a test can read what it said.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

// reportError prints a problem the way an operator reading a console wants it: the
// message on its own lines, with nothing wrapped around it.
func reportError(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "regied: %v\n", err)
	return 1
}

// pairs is a repeatable flag written as name=value. It is how `render` is given the
// values that exist only at apply time without reading a host for them.
type pairs map[string]string

func (p pairs) String() string {
	var out []string
	for name, value := range p {
		out = append(out, name+"="+value)
	}
	return strings.Join(out, ",")
}

func (p pairs) Set(argument string) error {
	name, value, err := splitPair(argument)
	if err != nil {
		return err
	}
	p[name] = value
	return nil
}

// multiPairs is the same flag for a name that may be given more than once. An uplink
// holds an address in each family, and the engine takes all of them, so the flag that
// stands in for reading the link has to be able to say so.
type multiPairs map[string][]string

func (p multiPairs) String() string {
	var out []string
	for name, values := range p {
		for _, value := range values {
			out = append(out, name+"="+value)
		}
	}
	return strings.Join(out, ",")
}

func (p multiPairs) Set(argument string) error {
	name, value, err := splitPair(argument)
	if err != nil {
		return err
	}
	p[name] = append(p[name], value)
	return nil
}

func splitPair(argument string) (name, value string, err error) {
	name, value, ok := strings.Cut(argument, "=")
	if !ok || name == "" || value == "" {
		return "", "", errors.New("expected name=value")
	}
	return name, value, nil
}

// reportConfigWarnings prints what validation said out loud without refusing the
// configuration. Both commands print them: a warning is about the configuration, so it
// does not become less true for not being applied (ADR 0006).
func reportConfigWarnings(stderr io.Writer, cfg *config.Config) {
	for _, warning := range cfg.Warnings() {
		fmt.Fprintf(stderr, "regied: %s\n", warning)
	}
}
