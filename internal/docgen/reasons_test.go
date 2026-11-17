package main

import (
	"strings"
	"testing"
)

func TestReasonTables(t *testing.T) {
	t.Parallel()
	code := map[string]string{
		"cmd/faucet/main.go": "package main\n",
		"faucet/reasons.go": "package faucet\n\nimport \"time\"\n\ntype GiveUpReason string\n\ntype CallbackReason string\n\n" +
			"const (\n\tGiveUpGone GiveUpReason = \"endpoint_gone\"\n\tGiveUpTooLarge GiveUpReason = \"event_too_large\"\n\tmaxBody = 256 << 10\n\twindow = 15 * time.Minute\n\tuntyped = \"not_a_reason\"\n)\n\n" +
			"const CallbackTimeout CallbackReason = \"timeout\"\n",
	}
	doc := "<!-- BEGIN generated:give-up-reasons -->\n| Reason | Meaning |\n| --- | --- |\n" +
		"| `endpoint_gone` | Gone. |\n| `event_too_large` | Over 256 KiB, within 15 minutes. |\n<!-- END generated:give-up-reasons -->\n\n" +
		"<!-- BEGIN generated:callback-reasons -->\n| Value | In lastError | In data.reason | Meaning |\n| --- | --- | --- | --- |\n" +
		"| `timeout` | yes | no | No answer. |\n<!-- END generated:callback-reasons -->\n"
	giveUps := map[string]string{"endpoint_gone": "Gone.", "event_too_large": "Over {faucet.maxBody:KiB}, within {faucet.window}."}
	callbacks := map[string]callbackReason{"timeout": {lastError: true, meaning: "No answer."}}
	with := func(m map[string]string, key, value string) map[string]string {
		out := map[string]string{key: value}
		for k, v := range m {
			if k != key {
				out[k] = v
			}
		}
		return out
	}
	cases := []struct {
		name      string
		giveUps   map[string]string
		callbacks map[string]callbackReason
		// wantErr are the names that the error must mention.
		wantErr []string
	}{
		{name: "the registries match the code", giveUps: giveUps, callbacks: callbacks},
		{name: "a constant without a row", giveUps: map[string]string{"endpoint_gone": "Gone."}, callbacks: callbacks, wantErr: []string{"event_too_large", "lacks"}},
		{name: "a row without a constant", giveUps: with(giveUps, "gone_away", "x"), callbacks: callbacks, wantErr: []string{"gone_away", "no GiveUpReason"}},
		{name: "a callback row without a constant", giveUps: giveUps, callbacks: map[string]callbackReason{"timeout": {}, "refused": {}}, wantErr: []string{"refused"}},
		{name: "a callback constant without a row", giveUps: giveUps, callbacks: map[string]callbackReason{}, wantErr: []string{"timeout"}},
		{name: "a size that is no whole KiB", giveUps: with(giveUps, "endpoint_gone", "{faucet.window:KiB}"), callbacks: callbacks, wantErr: []string{"{faucet.window:KiB}", "KiB"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"docs/webhooks.md": doc}
			for name, src := range code {
				files[name] = src
			}
			s := source{
				fsys: mapFS(files), tables: map[string]table{"give-up-reasons": giveUpTable, "callback-reasons": callbackTable},
				giveUps: tc.giveUps, callbacks: tc.callbacks,
			}
			updates, err := s.regenerate()
			if len(tc.wantErr) == 0 {
				if err != nil || len(updates) > 0 {
					t.Fatalf("got %d updates and error %v, want the tables to be current", len(updates), err)
				}
				return
			}
			if err == nil {
				t.Fatal("got no error")
			}
			for _, name := range tc.wantErr {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %s", err, name)
				}
			}
		})
	}
}
