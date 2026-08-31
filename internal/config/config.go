package config

import (
	"github.com/yuanying/regied/internal/apis/v1alpha1"
)

// Config is a configuration that has been read and found coherent: the document, an
// index of it, and the values regied derived rather than asked for.
//
// Everything downstream — the renderers, the apply engine, the state API — works from
// this and never re-reads the file.
type Config struct {
	document *v1alpha1.NetworkConfig
	order    []*v1alpha1.Resource
	byKind   map[v1alpha1.ResourceKind][]*v1alpha1.Resource
	index    map[v1alpha1.ResourceKind]map[string]*v1alpha1.Resource
	routing  map[string]PolicyRouting
	warnings Problems
}

// Document is the document as it was written.
func (c *Config) Document() *v1alpha1.NetworkConfig { return c.document }

// Global is the host-wide switches.
func (c *Config) Global() v1alpha1.Global { return c.document.Spec.Global }

// Resources is every resource, in the order the document lists them.
func (c *Config) Resources() []*v1alpha1.Resource { return c.order }

// ByKind is every resource of one kind, in the order the document lists them.
func (c *Config) ByKind(kind v1alpha1.ResourceKind) []*v1alpha1.Resource { return c.byKind[kind] }

// Lookup finds one resource by kind and name. Names are unique within a kind, not across
// kinds, so both are needed.
func (c *Config) Lookup(kind v1alpha1.ResourceKind, name string) *v1alpha1.Resource {
	return c.index[kind][name]
}

// PolicyRouting is the routing table number and firewall mark of one EgressRoutePolicy.
func (c *Config) PolicyRouting(name string) (PolicyRouting, bool) {
	routing, ok := c.routing[name]
	return routing, ok
}

// Warnings is what validation said out loud without refusing the configuration.
func (c *Config) Warnings() Problems { return c.warnings }

// Named is a resource of one kind with its name, which is how a renderer wants them.
type Named[S v1alpha1.ResourceSpec] struct {
	Name     string
	Spec     S
	Resource *v1alpha1.Resource
}

// ResourcesOf is every resource whose spec has the type S, in document order. It is the
// typed way round the fact that spec.resources holds eleven different kinds.
func ResourcesOf[S v1alpha1.ResourceSpec](c *Config) []Named[S] {
	var out []Named[S]
	for _, resource := range c.order {
		if spec, ok := resource.Spec.(S); ok {
			out = append(out, Named[S]{Name: resource.Metadata.Name, Spec: spec, Resource: resource})
		}
	}
	return out
}
