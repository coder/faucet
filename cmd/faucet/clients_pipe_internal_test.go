//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// trust reads no more of -jwks-file than a key set can hold, so a pipe that
// does not end, such as /dev/stdin, gets an answer.
func TestTrustReadsNoMoreOfAKeySetFileThanASetCanHold(t *testing.T) {
	t.Parallel()
	getenv, _ := newClientsDatabase(t)
	_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-no-secret", "-grant", "repo:coder/faucet", "digest-bot")
	require.NoError(t, err)
	fifo := filepath.Join(t.TempDir(), "jwks.json")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	// On Linux, opening a FIFO to read and write does not wait for a reader,
	// and keeps the pipe open after the data.
	pipe, err := os.OpenFile(fifo, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = pipe.Write(bytes.Repeat([]byte(" "), 40<<10))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, _, err := clients(t, getenv, "trust", "digest-bot", "-issuer", clusterIssuer, "-claim", "sub="+clusterAccount, "-jwks-file", fifo)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, pipe.Close())
		require.ErrorContains(t, err, "is over 32768 bytes")
	case <-time.After(10 * time.Second):
		require.NoError(t, pipe.Close())
		<-done
		t.Fatal("trust read the pipe until it ended")
	}
}
