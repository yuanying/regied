// Package networkd builds the systemd-networkd configuration a validated regied
// configuration asks for: the .network and .netdev files that carry links, addresses,
// MTUs, bridges, static routes, prefix delegation, router advertisement, the DS-Lite
// tunnel, and the routing half of policy routing (ADR 0008).
//
// It builds their contents and nothing else. Writing the files out, reloading networkd,
// reclaiming what an earlier apply left behind, and showing a diff belong to the apply
// engine. Rendering is a pure function of the configuration and of the few values that
// exist only at apply time, which arrive in a Runtime: nothing here reads a file,
// resolves a name, or looks at the running kernel.
package networkd
