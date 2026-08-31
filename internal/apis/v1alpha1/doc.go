// Package v1alpha1 holds the Go types for regied's configuration document, the schema
// described in docs/spec/. It carries the types and the decoding of individual values,
// and nothing else: whether a document is coherent — whether references resolve, whether
// names are unique, whether a pair of mutually exclusive fields has exactly one half —
// belongs to internal/config.
//
// The split matters for diagnosis. A malformed value can be reported with the line it
// sits on, because the decoder still has the document; a broken reference can only be
// reported with the resource and field it was written in, because by then the document
// is a graph. Each layer says what it can.
package v1alpha1
