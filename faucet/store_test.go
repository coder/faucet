package faucet_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
)

func TestEventValidateRejectsNUL(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]string{
		"value":        `{"msg":"a\u0000b"}`,
		"key":          `{"a\u0000":1}`,
		"nested array": `{"list":[{"x":"\u0000"}]}`,
	} {
		ev := faucet.Event{Source: "github", DeliveryID: "d", Type: "t", RepoID: 1, Data: json.RawMessage(data)}
		require.ErrorContains(t, ev.Validate(), "NUL", name)
	}

	ok := faucet.Event{Source: "github", DeliveryID: "d", Type: "t", RepoID: 1, Data: json.RawMessage(`{"msg":"a\\u0000b"}`)}
	require.NoError(t, ok.Validate(), "an escaped backslash is not a NUL")
}
