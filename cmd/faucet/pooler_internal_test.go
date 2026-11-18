package main

import (
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// startTransactionPooler starts a proxy in front of the Postgres at target,
// host:port, that hands each transaction to the next of two server
// connections, as a transaction-mode pooler such as Neon's does. A
// statement that is parsed in one round trip and bound in the next fails
// on it with "unnamed prepared statement does not exist". It returns the
// proxy's host:port. It speaks only what lib/pq sends with sslmode=disable
// to a server that trusts it.
func startTransactionPooler(t *testing.T, target string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer client.Close()
				_ = poolOne(client, target)
			}()
		}
	}()
	return ln.Addr().String()
}

func poolOne(client net.Conn, target string) error {
	startup, err := readStartup(client)
	if err != nil {
		return err
	}
	var servers [2]net.Conn
	defer func() {
		for _, server := range servers {
			if server != nil {
				_ = server.Close()
			}
		}
	}()
	for i := range servers {
		if servers[i], err = net.Dial("tcp", target); err != nil {
			return err
		}
		if _, err := servers[i].Write(startup); err != nil {
			return err
		}
		out := io.Discard
		if i == 0 {
			out = client
		}
		if err := relayUntilReady(servers[i], out); err != nil {
			return err
		}
	}
	for current := 0; ; {
		typ, msg, err := readMessage(client)
		if err != nil {
			return err
		}
		if _, err := servers[current].Write(msg); err != nil {
			return err
		}
		switch typ {
		case 'X':
			return nil
		case 'S', 'Q':
			if err := relayUntilReady(servers[current], client); err != nil {
				return err
			}
			current = 1 - current
		}
	}
}

func readStartup(r io.Reader) ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint32(size[:]))
	copy(msg, size[:])
	_, err := io.ReadFull(r, msg[4:])
	return msg, err
}

func readMessage(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	msg := make([]byte, 1+binary.BigEndian.Uint32(head[1:]))
	copy(msg, head[:])
	_, err := io.ReadFull(r, msg[5:])
	return head[0], msg, err
}

// relayUntilReady copies the server's messages to w up to and with
// ReadyForQuery.
func relayUntilReady(server io.Reader, w io.Writer) error {
	for {
		typ, msg, err := readMessage(server)
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		if typ == 'Z' {
			return nil
		}
	}
}

// throughPooler returns databaseURL with its host and port replaced by a
// transaction-mode pooler in front of them.
func throughPooler(t *testing.T, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	require.NoError(t, err)
	u.Host = startTransactionPooler(t, u.Host)
	return u.String()
}
