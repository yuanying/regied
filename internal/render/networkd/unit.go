package networkd

import (
	"strconv"
	"strings"
)

// unit is a systemd unit-style file under construction: a header comment and a sequence
// of sections. Sections repeat — a link takes several [Route] sections — so they are a
// list rather than a map, and the order keys are added in is the order they are written.
type unit struct {
	header   string
	sections []*section
}

type section struct {
	name string
	keys []keyValue
}

type keyValue struct{ key, value string }

// section starts a new section. Two sections of the same name are two sections; an empty
// one is left out of the output, so a caller can open one and then find it has nothing
// to say.
func (u *unit) section(name string) *section {
	s := &section{name: name}
	u.sections = append(u.sections, s)
	return s
}

func (s *section) set(key, value string) {
	s.keys = append(s.keys, keyValue{key, value})
}

func (s *section) setInt(key string, value int) {
	s.set(key, strconv.Itoa(value))
}

// setBool writes networkd's spelling of a boolean.
func (s *section) setBool(key string, value bool) {
	if value {
		s.set(key, "yes")
		return
	}
	s.set(key, "no")
}

func (u *unit) String() string {
	var b strings.Builder
	b.WriteString(u.header)
	for _, s := range u.sections {
		if len(s.keys) == 0 {
			continue
		}
		b.WriteString("\n[")
		b.WriteString(s.name)
		b.WriteString("]\n")
		for _, kv := range s.keys {
			b.WriteString(kv.key)
			b.WriteString("=")
			b.WriteString(kv.value)
			b.WriteString("\n")
		}
	}
	return b.String()
}
