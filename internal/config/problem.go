package config

import (
	"strconv"
	"strings"
)

// Severity is whether a problem stops the configuration from being used.
type Severity int

const (
	// SeverityError means the configuration is refused.
	SeverityError Severity = iota
	// SeverityWarning means it is used, with something said out loud first.
	SeverityWarning
)

// Problem is one thing wrong with a configuration, named precisely enough to be found:
// which resource, which field, and — when the document is still at hand — which line.
type Problem struct {
	Severity Severity
	Resource string // "Interface/lan"; empty for a problem with the document itself
	Field    string // "spec.addresses[0].fromDelegatedPrefix.interfaceRef"
	Line     int    // where the resource starts; zero when it is not known
	Message  string
}

func (p Problem) String() string {
	var b strings.Builder
	if p.Severity == SeverityWarning {
		b.WriteString("warning: ")
	}
	if p.Resource != "" {
		b.WriteString(p.Resource)
		if p.Line > 0 {
			b.WriteString(" (line ")
			b.WriteString(strconv.Itoa(p.Line))
			b.WriteString(")")
		}
		b.WriteString(": ")
	}
	if p.Field != "" {
		b.WriteString(p.Field)
		b.WriteString(": ")
	}
	b.WriteString(p.Message)
	return b.String()
}

// Problems is what one run of the validator found, in the order it found it.
type Problems []Problem

func (ps Problems) String() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = "  " + p.String()
	}
	return strings.Join(lines, "\n")
}

// Errors is the problems that refuse the configuration.
func (ps Problems) Errors() Problems { return ps.withSeverity(SeverityError) }

// Warnings is the problems that do not.
func (ps Problems) Warnings() Problems { return ps.withSeverity(SeverityWarning) }

func (ps Problems) withSeverity(s Severity) Problems {
	var out Problems
	for _, p := range ps {
		if p.Severity == s {
			out = append(out, p)
		}
	}
	return out
}

// HasErrors reports whether anything refuses the configuration.
func (ps Problems) HasErrors() bool {
	for _, p := range ps {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ValidationError is what Validate returns when the document is not coherent. It carries
// every problem found in the run, warnings included, so that a caller printing it shows
// the operator everything at once.
type ValidationError struct {
	Path     string // the file the document came from, when it came from one
	Problems Problems
}

func (e *ValidationError) Error() string {
	head := "invalid configuration"
	if e.Path != "" {
		head = "invalid configuration in " + e.Path
	}
	return head + ":\n" + e.Problems.String()
}
