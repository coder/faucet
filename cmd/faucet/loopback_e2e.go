//go:build faucet_e2e

package main

// allowInsecureLoopback lets the callback client reach loopback addresses over
// http, so the end-to-end tests can run a receiver on 127.0.0.1. Never ship a
// binary built with this tag. run logs a warning at start.
const allowInsecureLoopback = true
