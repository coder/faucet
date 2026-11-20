package faucet_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The repository of octokit/webhooks' recorded release payloads.
const (
	releaseRepo   = "Codertocat/Hello-World"
	releaseRepoID = 186853002
)

// publishedRelease is octokit/webhooks' recorded delivery of a release that
// was published, payload-examples/api.github.com/release/published.payload.json.
func publishedRelease(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "release_published.json"))
	require.NoError(t, err)
	return b
}

// draftRelease is the recorded release as GitHub sends it while the release
// is a draft: with action, release.draft true, no published_at, and notes
// that only people with push access may read. edit changes it further.
func draftRelease(t *testing.T, action string, edit func(release map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(publishedRelease(t), &doc))
	release := doc["release"].(map[string]any)
	doc["action"] = action
	release["draft"] = true
	release["published_at"] = nil
	release["body"] = "Unpublished notes"
	if edit != nil {
		edit(release)
	}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return b
}

// draftReleases are deliveries of a release that is not published, of every
// action that GitHub sends for a draft.
func draftReleases(t *testing.T) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		"created":           draftRelease(t, "created", nil),
		"edited":            draftRelease(t, "edited", nil),
		"deleted":           draftRelease(t, "deleted", nil),
		"unpublished":       draftRelease(t, "unpublished", nil),
		"without draft":     draftRelease(t, "edited", func(r map[string]any) { delete(r, "draft") }),
		"draft not boolean": draftRelease(t, "edited", func(r map[string]any) { r["draft"] = "false" }),
		"Draft false after": bytes.Replace(draftRelease(t, "edited", nil), []byte(`"draft":true`), []byte(`"draft":true,"Draft":false`), 1),
	}
}

func TestNoPollOfARawReleaseGetsADraft(t *testing.T) {
	t.Parallel()

	h := newRawPollHarness(t)
	h.allow(releaseRepo, releaseRepoID)
	whole := args("repo", releaseRepo)
	fields := args("repo", releaseRepo, "fields", []string{"release.body", "release.tag_name"})
	wholeLease := h.pollRaw("github.release", whole, "")
	fieldsLease := h.pollRaw("github.release", fields, "")

	answers := map[string]string{}
	for name, body := range draftReleases(t) {
		answers[name] = h.deliver("release", "draft "+name, body)
	}
	require.Empty(t, h.pollRaw("github.release", whole, wholeLease.Cursor).Events, "no draft reaches a lease")
	require.Empty(t, h.pollRaw("github.release", fields, fieldsLease.Cursor).Events, "no draft reaches a lease with fields")
	for name, answer := range answers {
		require.Equal(t, "ignored", answer, name)
	}

	published := publishedRelease(t)
	require.Equal(t, "stored", h.deliver("release", "published", published))
	data := h.pollData(pollParams("github.release", whole, raw(wholeLease.Cursor)))
	require.Len(t, data, 1)
	require.JSONEq(t, string(published), string(data[0]), "a published release is the payload")
	data = h.pollData(pollParams("github.release", fields, raw(fieldsLease.Cursor)))
	require.Len(t, data, 1)
	require.JSONEq(t, `{"release":{"body":"","tag_name":"0.0.1"}}`, string(data[0]))
}

func TestNoSubscriptionToRawReleasesGetsADraft(t *testing.T) {
	t.Parallel()

	d := newRawDeliverHarness(t)
	d.allow(releaseRepo, releaseRepoID)
	whole := hookEndpoint{path: "/whole", secret: newSecret(t, 32), args: args("repo", releaseRepo)}
	fields := hookEndpoint{path: "/fields", secret: newSecret(t, 32), args: args("repo", releaseRepo, "fields", []string{"release.body", "release.tag_name"})}
	d.subscribeRaw("github.release", whole)
	d.subscribeRaw("github.release", fields)
	d.advance(time.Second)

	answers := map[string]string{}
	for name, body := range draftReleases(t) {
		answers[name] = d.deliver("release", "draft "+name, body)
	}
	require.Zero(t, d.deliverDue().Claimed, "no draft reaches a subscription, with fields or without")
	for name, answer := range answers {
		require.Equal(t, "ignored", answer, name)
	}

	published := publishedRelease(t)
	d.deliverRaw("release", published)
	require.Equal(t, 2, d.deliverDue().Delivered)
	var want map[string]any
	require.NoError(t, json.Unmarshal(published, &want))
	for path, data := range map[string]map[string]any{
		"/whole":  want,
		"/fields": {"release": map[string]any{"body": "", "tag_name": "0.0.1"}},
	} {
		requests := d.rcv.requests(path)
		require.Len(t, requests, 1, path)
		require.Equal(t, data, requests[0].body(t).Data, fmt.Sprintf("%s gets the published release", path))
	}
}
