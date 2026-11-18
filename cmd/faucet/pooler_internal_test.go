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
// host:port, that runs each transaction as if on a fresh server connection,
// as a transaction-mode pooler such as Neon's does: before each transaction
// it closes the unnamed prepared statement that the one before left. A
// statement that is parsed in one round trip and bound in the next fails on
// it with "unnamed prepared statement does not exist". It returns the
// proxy's host:port. It speaks only what lib/pq sends with sslmode=disable.
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

// closeUnnamed closes the unnamed prepared statement and ends the
// transaction. Closing one that does not exist is no error.
var closeUnnamed = []byte{'C', 0, 0, 0, 6, 'S', 0, 'S', 0, 0, 0, 4}

func poolOne(client net.Conn, target string) error {
	startup, err := readStartup(client)
	if err != nil {
		return err
	}
	server, err := net.Dial("tcp", target)
	if err != nil {
		return err
	}
	defer server.Close()
	if _, err := server.Write(startup); err != nil {
		return err
	}
	if err := authenticate(client, server); err != nil {
		return err
	}
	for fresh := true; ; {
		typ, msg, err := readMessage(client)
		if err != nil {
			return err
		}
		if typ == 'X' {
			_, err := server.Write(msg)
			return err
		}
		if !fresh {
			if _, err := server.Write(closeUnnamed); err != nil {
				return err
			}
			if err := relayUntilReady(server, io.Discard); err != nil {
				return err
			}
			fresh = true
		}
		if _, err := server.Write(msg); err != nil {
			return err
		}
		if typ == 'S' || typ == 'Q' {
			if err := relayUntilReady(server, client); err != nil {
				return err
			}
			fresh = false
		}
	}
}

// authenticate relays the server's startup answers to the client up to
// ReadyForQuery, and the client's answer to each authentication request
// that wants one: a password, or a SASL message.
func authenticate(client, server net.Conn) error {
	for {
		typ, msg, err := readMessage(server)
		if err != nil {
			return err
		}
		if _, err := client.Write(msg); err != nil {
			return err
		}
		switch {
		case typ == 'Z':
			return nil
		case typ == 'R' && len(msg) >= 9:
			// 3 cleartext, 5 MD5, 10 SASL and 11 SASL continue each wait
			// for the client's next message.
			switch binary.BigEndian.Uint32(msg[5:9]) {
			case 3, 5, 10, 11:
				_, answer, err := readMessage(client)
				if err != nil {
					return err
				}
				if _, err := server.Write(answer); err != nil {
					return err
				}
			}
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
