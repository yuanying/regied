// Package nftables builds the nftables ruleset regied owns out of a validated
// configuration.
//
// It renders and nothing else. No kernel is read, no nft command is run, and no value is
// taken from the running host except through Runtime, which the caller fills in. The
// result is a function of the configuration and that struct alone, so the tests need
// neither privileges nor an uplink, and the same configuration always produces the same
// text. Applying it — replacing the table, rolling back, showing a difference — belongs
// to the apply engine.
//
// Everything here lives in one table, and regied touches nothing outside it. The ruleset
// is never flushed (ADR 0009).
package nftables
