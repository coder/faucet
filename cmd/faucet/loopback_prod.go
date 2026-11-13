//go:build !faucet_e2e

package main

// allowInsecureLoopback is false in every build except the one that the
// end-to-end tests make with -tags faucet_e2e. No variable turns it on, so a
// production binary has no switch that disables the guard of the callback
// client.
const allowInsecureLoopback = false
