package faucet_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

func TestSignWebhookIsVerifiedByTheStandardWebhooksLibrary(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		secretBytes int
		id          string
		body        []byte
	}{
		"an event":                           {24, "msg_2KWPBgLlAfxdpx2AI54pPJ85f4W", []byte(`{"eventId":"evt_1","name":"ci.run_completed","data":{}}`)},
		"a 32 byte secret":                   {32, "msg_verification_AbC-_123", []byte(`{"type":"verification","challenge":"x"}`)},
		"a 64 byte secret":                   {64, "m", []byte(`{}`)},
		"a body with surrounding whitespace": {32, "msg_ws", []byte(" {\"a\": 1}\r\n\n")},
		"an empty body":                      {32, "msg_empty", nil},
		"a body that is not UTF-8":           {32, "msg_binary", []byte{0xff, 0xfe, 0x00, '.', '.'}},
		"a body with unicode":                {32, "msg_unicode", []byte(`{"title":"héllo ✓ 日本"}`)},
		"a 1 MiB body":                       {32, "msg_big", bytes.Repeat([]byte("a"), 1<<20)},
	} {
		secret := newSecret(t, tc.secretBytes)
		now := time.Now()
		headers, err := faucet.SignWebhook(secret, tc.id, now, tc.body)
		require.NoError(t, err, name)

		verifier, err := standardwebhooks.NewWebhook(secret)
		require.NoError(t, err, name)
		require.NoError(t, verifier.Verify(tc.body, headers), name)

		require.Equal(t, tc.id, headers.Get("webhook-id"), name)
		require.Equal(t, strconv.FormatInt(now.Unix(), 10), headers.Get("webhook-timestamp"), name)
		require.True(t, strings.HasPrefix(headers.Get("webhook-signature"), "v1,"), name)
		require.Len(t, headers, 3, "no header beyond the three of Standard Webhooks")
	}
}

func TestSignWebhookSignaturesFailForAnythingElse(t *testing.T) {
	t.Parallel()

	secret := newSecret(t, 32)
	body := []byte(`{"eventId":"evt_1"}`)
	now := time.Now()
	headers, err := faucet.SignWebhook(secret, "msg_1", now, body)
	require.NoError(t, err)
	verifier, err := standardwebhooks.NewWebhook(secret)
	require.NoError(t, err)
	other, err := standardwebhooks.NewWebhook(newSecret(t, 32))
	require.NoError(t, err)

	require.NoError(t, verifier.Verify(body, headers), "the control: the untouched message verifies")
	require.Error(t, other.Verify(body, headers), "another secret")
	require.Error(t, verifier.Verify([]byte(`{"eventId":"evt_2"}`), headers), "another body")
	require.Error(t, verifier.Verify(append(bytes.Clone(body), ' '), headers), "one byte more")

	tampered := func(name, value string) http.Header {
		h := headers.Clone()
		h.Set(name, value)
		return h
	}
	require.Error(t, verifier.Verify(body, tampered("webhook-id", "msg_2")), "another id")
	require.Error(t, verifier.Verify(body, tampered("webhook-timestamp", headers.Get("webhook-timestamp")+"0")), "another timestamp")
	require.Error(t, verifier.Verify(body, tampered("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))), "a forged signature")
	require.Error(t, verifier.Verify(body, tampered("webhook-signature", strings.Replace(headers.Get("webhook-signature"), "v1,", "v2,", 1))), "another version")

	old, err := faucet.SignWebhook(secret, "msg_1", now.Add(-10*time.Minute), body)
	require.NoError(t, err)
	require.Error(t, verifier.Verify(body, old), "the library refuses a stale timestamp, and faucet signs the time it is given")
	require.NoError(t, verifier.VerifyIgnoringTimestamp(body, old))
}

func TestSignWebhookRefusesBadInputWithoutEchoingThem(t *testing.T) {
	t.Parallel()

	const marker = "Zm9vYmFyLW1hcmtlci0xMjM0"
	good := newSecret(t, 32)
	for name, secret := range map[string]string{
		"empty":              "",
		"no prefix":          strings.TrimPrefix(good, "whsec_"),
		"prefix only":        "whsec_",
		"not base64":         "whsec_" + marker + "!!",
		"URL-safe alphabet":  "whsec_" + strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))),
		"23 bytes":           "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 23)),
		"65 bytes":           "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 65)),
		"unpadded":           "whsec_" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 25)),
		"another prefix":     "whsk_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
		"a long secret text": "whsec_" + strings.Repeat(marker, 20),
	} {
		_, err := faucet.SignWebhook(secret, "msg_1", time.Now(), []byte("{}"))
		require.Error(t, err, name)
		for _, fragment := range []string{marker, strings.TrimPrefix(secret, "whsec_")} {
			if len(fragment) >= 8 {
				require.NotContains(t, err.Error(), fragment, name)
			}
		}
	}

	for name, id := range map[string]string{
		"empty": "", "a dot": "msg.1", "a space": "msg 1", "a newline": "msg_1\r\nX-Evil: 1", "a comma": "msg,1",
		"too long": strings.Repeat("a", 129), "non-ASCII": "msg_é",
	} {
		_, err := faucet.SignWebhook(good, id, time.Now(), []byte("{}"))
		require.Error(t, err, name)
	}
	_, err := faucet.SignWebhook(good, "msg_1", time.Time{}, []byte("{}"))
	require.Error(t, err, "a zero time")
	_, err = faucet.SignWebhook(good, "msg_1", time.Unix(-5, 0), []byte("{}"))
	require.Error(t, err, "a time before 1970")
	_, err = faucet.SignWebhook(good, strings.Repeat("a", 128), time.Now(), []byte("{}"))
	require.NoError(t, err, "128 bytes is the limit")
}
