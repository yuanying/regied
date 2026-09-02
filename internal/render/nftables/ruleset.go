package nftables

import (
	"fmt"
	"strings"
)

// The one table regied owns. Everything this package emits is inside it, its name says
// whose it is, and nothing outside it is read or written (ADR 0009).
const (
	TableFamily = "inet"
	TableName   = "regied"
)

// Ruleset is the table regied owns: its sets, and its chains.
//
// It is kept as a structure rather than as text so that the apply engine can look at
// what is in it. String is the text to hand to `nft -f`.
type Ruleset struct {
	Family string
	Table  string
	Sets   []Set
	Chains []Chain
}

// Set is one named set inside the table: a zone's interface names, or an IPAddressSet.
type Set struct {
	Name     string
	Type     string   // ifname, ipv4_addr, ipv6_addr
	Flags    []string // interval, for a set that holds prefixes
	Elements []string // written as they appear in the output, quoted where the type needs it
	Comment  string   // what this set came from, emitted as a comment line
}

// Chain is one chain. Base is nil for a chain that is only jumped to. A Comment of
// several lines is emitted as several comment lines.
type Chain struct {
	Name    string
	Base    *BaseChain
	Rules   []Rule
	Comment string
}

// BaseChain is the hook a chain hangs off.
type BaseChain struct {
	Type     string // filter, nat
	Hook     string // prerouting, input, forward, postrouting
	Priority string // filter, mangle, dstnat, srcnat
	Policy   string // accept, drop
}

// Rule is one rule. A Rule with no Text is a comment on its own, which is how something
// that could not be rendered says so where it would have been.
type Rule struct {
	Text    string
	Comment string
}

// The header explains, to whoever reads the file on the host, why it looks like this.
const rulesetHeader = `# regied's nftables ruleset. Generated on every apply; edits here do not survive one.
#
# regied owns this one table and nothing else in the ruleset, so the ruleset is never
# flushed and no rule anybody else installed is touched (ADR 0009). The two statements
# below add the table if it is not already there and then remove it, so that the one
# that follows goes in whether or not a previous apply left anything behind. nft runs
# the whole file as a single transaction, so the table is never half replaced.

`

// String is the text to hand to `nft -f`.
func (r *Ruleset) String() string {
	var b strings.Builder
	b.WriteString(rulesetHeader)
	fmt.Fprintf(&b, "table %s %s\n", r.Family, r.Table)
	fmt.Fprintf(&b, "delete table %s %s\n\n", r.Family, r.Table)
	fmt.Fprintf(&b, "table %s %s {\n", r.Family, r.Table)

	blocks := make([]string, 0, len(r.Sets)+len(r.Chains))
	for _, set := range r.Sets {
		blocks = append(blocks, set.String())
	}
	for _, chain := range r.Chains {
		blocks = append(blocks, chain.String())
	}
	b.WriteString(strings.Join(blocks, "\n"))
	b.WriteString("}\n")
	return b.String()
}

func (s Set) String() string {
	var b strings.Builder
	if s.Comment != "" {
		fmt.Fprintf(&b, "\t# %s\n", s.Comment)
	}
	fmt.Fprintf(&b, "\tset %s {\n\t\ttype %s\n", s.Name, s.Type)
	if len(s.Flags) > 0 {
		fmt.Fprintf(&b, "\t\tflags %s\n", strings.Join(s.Flags, ","))
	}
	if len(s.Elements) > 0 {
		fmt.Fprintf(&b, "\t\telements = { %s }\n", strings.Join(s.Elements, ", "))
	}
	b.WriteString("\t}\n")
	return b.String()
}

func (c Chain) String() string {
	var b strings.Builder
	for _, line := range commentLines(c.Comment) {
		fmt.Fprintf(&b, "\t# %s\n", line)
	}
	fmt.Fprintf(&b, "\tchain %s {\n", c.Name)
	if c.Base != nil {
		fmt.Fprintf(&b, "\t\ttype %s hook %s priority %s; policy %s;\n",
			c.Base.Type, c.Base.Hook, c.Base.Priority, c.Base.Policy)
	}
	for _, rule := range c.Rules {
		for _, line := range commentLines(rule.Comment) {
			fmt.Fprintf(&b, "\t\t# %s\n", line)
		}
		if rule.Text != "" {
			fmt.Fprintf(&b, "\t\t%s\n", rule.Text)
		}
	}
	b.WriteString("\t}\n")
	return b.String()
}

// commentLines is a comment as the lines it is written on, so that an explanation longer
// than one line does not have to be folded by hand where it is written.
func commentLines(comment string) []string {
	if comment == "" {
		return nil
	}
	return strings.Split(comment, "\n")
}
