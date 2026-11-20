package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"maps"
	"slices"
	"strings"
)

// giveUpMeanings explains each faucet.GiveUpReason, by value. A meaning
// names a constant as a placeholder (see fill).
var giveUpMeanings = map[string]string{
	"endpoint_gone":              "The endpoint answered `410 Gone`. This ends the delivery, not the subscription.",
	"endpoint_payload_too_large": "The endpoint answered `413`. This ends the delivery, not the subscription.",
	"event_too_large":            "The body would be over {faucet.maxEventBodyBytes:KiB}, so Superblink Events never sent it. A raw subscription gets a gap.",
	"event_malformed":            "Superblink Events could not build a body from the stored event. A raw subscription gets a gap.",
	"attempts_exhausted":         "All {faucet.MaxDeliveryAttempts} attempts failed.",
	"window_expired":             "The next attempt would come more than {faucet.DeliveryWindow} after the event was queued.",
	"event_purged":               "A retention purge deleted the event before the delivery ended, or, for a raw event, a purge dropped its payload. A raw subscription then gets a gap.",
	"repo_mismatch":              "The event is outside the target that the subscription is granted now: another repository, or a repository of another owner.",
	"secret_unreadable":          "The subscriber's sealed secret does not open, as after a change of `FAUCET_WEBHOOK_KEY`. The subscription is deleted too, until its subscriber renews.",
	"subscription_expired":       "The subscription expired before the delivery ended. A raw delivery also ends so when its subscription was unsubscribed, or deleted after a renewal found access revoked.",
	"arguments_undecodable":      "The subscription's stored arguments do not decode, or hold a list longer than an argument may now be, so Superblink Events deleted the subscription with its deliveries.",
}

// callbackReason explains one faucet.CallbackReason. faucet's
// docs_internal_test.go checks lastError and dataReason against the code.
type callbackReason struct {
	// lastError says whether a delivery records the value as the lastError of
	// the subscription's deliveryStatus.
	lastError bool
	// dataReason says whether events/subscribe answers it as the data.reason
	// of a CallbackEndpointError.
	dataReason bool
	meaning    string
}

// callbackReasons explains each faucet.CallbackReason, by value.
var callbackReasons = map[string]callbackReason{
	"connection_refused": {true, true, "The connection failed: the port refused it, the name did not resolve, or the address is not public."},
	"timeout":            {true, true, "No answer within {faucet.callbackRequestTimeout}."},
	"tls_error":          {true, true, "The TLS handshake or the certificate check failed."},
	"http_4xx":           {true, true, "A 4xx answer. For a delivery, also any other answer that is not 2xx, 5xx, `410` or `413`, such as a redirect."},
	"http_5xx":           {true, true, "A 5xx answer. For a delivery, also a response that declares a body over {faucet.maxCallbackResponseBytes:KiB} or has headers over {faucet.maxCallbackHeaderBytes:KiB}. A delivery closes the body unread, so only a declared length counts."},
	"challenge_failed":   {false, true, "The callback URL's origin is not on the allowlist, or the handshake got no 2xx answer of at most {faucet.maxHandshakeAnswer:KiB} that echoes the challenge."},
}

func giveUpTable(s source) ([]string, error) {
	reasons, values, err := reasonsOf(s, "GiveUpReason", slices.Collect(maps.Keys(s.giveUps)), "the give-up registry")
	if err != nil {
		return nil, err
	}
	rows := []string{"| Reason | Meaning |", "| --- | --- |"}
	for _, r := range reasons {
		meaning, err := fill(s.giveUps[r], values)
		if err != nil {
			return nil, fmt.Errorf("give-up reason %s: %w", r, err)
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %s |", r, meaning))
	}
	return rows, nil
}

func callbackTable(s source) ([]string, error) {
	reasons, values, err := reasonsOf(s, "CallbackReason", slices.Collect(maps.Keys(s.callbacks)), "the callback registry")
	if err != nil {
		return nil, err
	}
	rows := []string{"| Value | In lastError | In data.reason | Meaning |", "| --- | --- | --- | --- |"}
	for _, r := range reasons {
		c := s.callbacks[r]
		meaning, err := fill(c.meaning, values)
		if err != nil {
			return nil, fmt.Errorf("callback reason %s: %w", r, err)
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %s | %s |", r, yesNo[c.lastError], yesNo[c.dataReason], meaning))
	}
	return rows, nil
}

var yesNo = map[bool]string{true: "yes", false: "no"}

// reasonsOf returns the values of the constants of package faucet whose type
// is typ, in the order of the code, after it checked them against the
// values that a registry lists. It also returns the constants that the
// registry may name.
func reasonsOf(s source, typ string, listed []string, registry string) ([]string, map[string]goConst, error) {
	core, err := parsePackage(s.fsys, "faucet")
	if err != nil {
		return nil, nil, err
	}
	if made := madeValues(core, typ); len(made) > 0 {
		return nil, nil, fmt.Errorf("package faucet makes a %s that is no declared constant, so %s cannot list it: %s", typ, registry, strings.Join(made, ", "))
	}
	reasons := typedConsts(core, typ)
	if err := compareNames(listed, reasons,
		"package faucet declares the "+typ+" %s, which "+registry+" lacks",
		registry+" lists %s, which no "+typ+" of package faucet is"); err != nil {
		return nil, nil, err
	}
	values, err := constValues(s.fsys)
	return reasons, values, err
}

// typedConsts returns the values of the string constants of pkg that are
// declared with the type typ, in the order of the code.
func typedConsts(pkg *goPackage, typ string) []string {
	consts := readConsts(pkg)
	var values []string
	for _, spec := range valueSpecs(pkg, token.CONST) {
		if id, ok := spec.Type.(*ast.Ident); !ok || id.Name != typ {
			continue
		}
		for _, name := range spec.Names {
			if c, ok := consts[name.Name]; ok && c.value.Kind() == constant.String {
				values = append(values, constant.StringVal(c.value))
			}
		}
	}
	return values
}

// madeValues returns where pkg makes a value of type typ other than by naming
// a constant that declares its type: a conversion typ(...) anywhere, or a
// variable of type typ with a value.
func madeValues(pkg *goPackage, typ string) []string {
	var at []string
	isTyp := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && id.Name == typ
	}
	for _, f := range pkg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if isTyp(n.Fun) && len(n.Args) == 1 {
					at = append(at, pkg.fset.Position(n.Pos()).String())
				}
			case *ast.GenDecl:
				for _, spec := range n.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && n.Tok == token.VAR && isTyp(vs.Type) && len(vs.Values) > 0 {
						at = append(at, pkg.fset.Position(vs.Pos()).String())
					}
				}
			}
			return true
		})
	}
	return at
}
