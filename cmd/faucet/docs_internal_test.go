package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The docs state numbers that come from the constants of cmd/faucet. A
// change to a constant fails here until the docs say the new number.
func TestDocsStateTheNumbersThatTheConstantsGive(t *testing.T) {
	t.Parallel()

	cronSeconds := int(cronTimeout / time.Second)
	for _, p := range []struct {
		file, format string
		args         []any
	}{
		{"README.md", "The server listens on `PORT` (default %s)", []any{defaultPort}},
		{"docs/deploy.md", "per million rows per scan, and stops after %d seconds", []any{cronSeconds}},
		{"docs/redelivery.md", "half of its %d seconds reading the list", []any{cronSeconds}},
		{"docs/redelivery.md", "The cron route stops a run after %d seconds", []any{cronSeconds}},
	} {
		doc, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(p.file)))
		require.NoError(t, err)
		phrase := fmt.Sprintf(p.format, p.args...)
		lead, _, _ := strings.Cut(p.format, "%")
		require.True(t, strings.Contains(string(doc), phrase), "%s does not say %q", p.file, phrase)
		require.Equal(t, strings.Count(string(doc), lead), strings.Count(string(doc), phrase), "%s: every %q must go on as %q", p.file, lead, phrase)
	}
}
