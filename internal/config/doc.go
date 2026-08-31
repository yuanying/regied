// Package config turns one configuration file into a validated model.
//
// It has three jobs, and they are in three files. Parse reads the document and rejects
// anything malformed, including a field nobody declared. Validate says whether the
// document is coherent: references resolve, names are unique, required fields are
// present, and the pairs where exactly one half may be written have exactly one half.
// Derive allocates the routing table numbers and firewall marks the operator does not
// write.
//
// What comes out is a Config: the document, an index of it, and the derived numbers.
// The renderers build on that and never re-read the file.
//
// Both phases report everything they found rather than the first thing. A configuration
// with six mistakes in it should take one run to see all six, not six runs.
package config
