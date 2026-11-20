package faucet_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
)

// rawNames are the raw event names in events/list order: the GitHub webhook
// events whose payload names a repository, that no typed family stores in
// full, and that GitHub shows to anyone who can read the repository.
var rawNames = []string{
	"github.commit_comment", "github.create", "github.custom_property_values", "github.delete",
	"github.deployment", "github.deployment_protection_rule", "github.deployment_review", "github.deployment_status",
	"github.discussion", "github.discussion_comment", "github.fork", "github.gollum", "github.issue_comment",
	"github.issue_dependencies", "github.issue_relates_to", "github.issues", "github.label", "github.merge_group",
	"github.milestone", "github.package", "github.page_build", "github.project", "github.project_card",
	"github.project_column", "github.public", "github.push", "github.registry_package", "github.release",
	"github.repository", "github.repository_import",
	"github.star", "github.sub_issues", "github.watch", "github.workflow_dispatch",
}

// withRawEvents offers every raw event. events/list calls no RawStore
// method, so a store without methods is enough.
func withRawEvents(d *faucet.Deps) {
	d.RawStore, d.RawEvents = struct{ faucet.RawStore }{}, faucet.RawEventNames()
}

type listedEvent struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Delivery      []string        `json:"delivery"`
	InputSchema   json.RawMessage `json:"inputSchema"`
	PayloadSchema json.RawMessage `json:"payloadSchema"`
}

func listEvents(t *testing.T, h *eventsHarness) []listedEvent {
	t.Helper()
	res, err := h.call(methodList, nil)
	require.NoError(t, err)
	var events []listedEvent
	require.NoError(t, json.Unmarshal(res.fields["events"], &events))
	return events
}

func TestEventsListOffersTheRawNamesAfterTheTypedOnes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		harness  func(t *testing.T, mutate ...func(*faucet.Deps)) *eventsHarness
		delivery []string
	}{
		{name: "PollOnly", harness: newEventsHarness, delivery: []string{"poll"}},
		{
			name: "WithWebhookDelivery",
			harness: func(t *testing.T, mutate ...func(*faucet.Deps)) *eventsHarness {
				return newWebhookHarness(t, mutate...).eventsHarness
			},
			delivery: []string{"poll", "webhook"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			typed := listEvents(t, tt.harness(t))
			events := listEvents(t, tt.harness(t, withRawEvents))
			names := make([]string, len(events))
			for i, e := range events {
				names[i] = e.Name
			}
			require.Equal(t, slices.Concat(allNames, rawNames), names)
			require.Equal(t, typed, events[:len(typed)], "the typed names are listed as they are without raw events")

			for _, e := range events[len(typed):] {
				require.NotEmpty(t, e.Description, e.Name)
				require.Equal(t, tt.delivery, e.Delivery, e.Name)
				for what, schema := range map[string]json.RawMessage{"inputSchema": e.InputSchema, "payloadSchema": e.PayloadSchema} {
					var s jsonschema.Schema
					require.NoError(t, json.Unmarshal(schema, &s), "%s of %s", what, e.Name)
					_, err := s.Resolve(nil)
					require.NoError(t, err, "%s of %s is a valid JSON Schema", what, e.Name)
					require.Equal(t, "object", s.Type, "%s of %s", what, e.Name)
				}
				var input struct {
					Required             []string                   `json:"required"`
					Properties           map[string]json.RawMessage `json:"properties"`
					AdditionalProperties *bool                      `json:"additionalProperties"`
				}
				require.NoError(t, json.Unmarshal(e.InputSchema, &input))
				require.Empty(t, input.Required, e.Name)
				require.ElementsMatch(t, []string{"repo", "owner", "all", "repos", "actions", "fields"}, slices.Collect(maps.Keys(input.Properties)), e.Name)
				require.NotNil(t, input.AdditionalProperties, e.Name)
				require.False(t, *input.AdditionalProperties, e.Name)
			}
		})
	}
}
