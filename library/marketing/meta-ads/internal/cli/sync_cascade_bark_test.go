// Bark fork addition (not upstream) — exercises syncCascadeResource, the
// account/ad-scoped sync support added for
// https://github.com/mvanhorn/printing-press-library/issues/1434, with a
// fake client so it needs no real credentials or network access.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/printing-press-library/library/marketing/meta-ads/internal/store"
)

// fakeHeadersClient answers GetWithHeaders() from a table of canned
// responses keyed by "path|k=v|k=v..." (params in insertion order matters
// only for the keys the test cares about), recording every call so tests
// can assert exactly which requests were made.
type fakeHeadersClient struct {
	responses map[string]json.RawMessage
	calls     []string
}

func (f *fakeHeadersClient) GetWithHeaders(_ context.Context, path string, params map[string]string, _ map[string]string) (json.RawMessage, error) {
	key := path
	for _, k := range []string{"limit", "after", "fields", "time_increment"} {
		if v, ok := params[k]; ok {
			key += fmt.Sprintf("|%s=%s", k, v)
		}
	}
	f.calls = append(f.calls, key)
	if resp, ok := f.responses[key]; ok {
		return resp, nil
	}
	return json.RawMessage(`[]`), nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestSyncCascadeResource_AccountScoped verifies campaigns sync once per
// already-synced ad account, hitting exactly /{adAccountId}/campaigns for
// each — the fix for #1434, where this call failed instantly for every
// account-scoped resource because the resource was entirely absent from
// the sync spec.
func TestSyncCascadeResource_AccountScoped(t *testing.T) {
	db := newTestStore(t)
	for _, acct := range []string{"act_111", "act_222"} {
		if err := db.UpsertMe(json.RawMessage(fmt.Sprintf(`{"id":%q,"name":"acct"}`, acct))); err != nil {
			t.Fatalf("seed me: %v", err)
		}
	}

	client := &fakeHeadersClient{responses: map[string]json.RawMessage{
		"/act_111/campaigns|limit=100": json.RawMessage(`[{"id":"c1","name":"Campaign One"}]`),
		"/act_222/campaigns|limit=100": json.RawMessage(`[{"id":"c2","name":"Campaign Two"}]`),
	}}

	res := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId", nil, io.Discard)
	if res.Err != nil {
		t.Fatalf("syncCascadeResource returned error: %v", res.Err)
	}
	if res.Count != 2 {
		t.Fatalf("count = %d, want 2", res.Count)
	}

	ids, err := db.ListIDs("campaigns")
	if err != nil {
		t.Fatalf("ListIDs(campaigns): %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("stored campaign ids = %v, want 2 entries", ids)
	}

	wantCalls := map[string]bool{"/act_111/campaigns|limit=100": true, "/act_222/campaigns|limit=100": true}
	if len(client.calls) != 2 {
		t.Fatalf("calls = %v, want exactly 2", client.calls)
	}
	for _, c := range client.calls {
		if !wantCalls[c] {
			t.Fatalf("unexpected call %q", c)
		}
	}
}

// TestSyncCascadeResource_AdScopedRequiresParent verifies insights (an
// ad-scoped resource) warns rather than errors when the `ads` table hasn't
// been synced yet, instead of silently syncing zero rows.
func TestSyncCascadeResource_AdScopedRequiresParent(t *testing.T) {
	db := newTestStore(t)
	client := &fakeHeadersClient{responses: map[string]json.RawMessage{}}

	res := syncCascadeResource(context.Background(), client, db, "insights", "/{adId}/insights", "ads", "adId", map[string]string{"fields": insightsSyncFields}, io.Discard)
	if res.Err != nil {
		t.Fatalf("expected a warning, not an error: %v", res.Err)
	}
	if res.Warn == nil {
		t.Fatalf("expected Warn to be set when parent table is empty")
	}
	if len(client.calls) != 0 {
		t.Fatalf("expected zero API calls with no synced ads, got %v", client.calls)
	}
}

// TestSyncCascadeResource_AdScoped_PassesInsightsFields verifies the
// insights sync path actually requests the analysis-relevant field set
// (fatigue/decay/reconcile need cpm/ctr/frequency/actions/purchase_roas,
// not just the default spend+impressions).
func TestSyncCascadeResource_AdScoped_PassesInsightsFields(t *testing.T) {
	db := newTestStore(t)
	if err := db.UpsertAds(json.RawMessage(`{"id":"ad1","name":"Ad One"}`)); err != nil {
		t.Fatalf("seed ads: %v", err)
	}

	wantKey := "/ad1/insights|limit=100|fields=" + insightsSyncFields + "|time_increment=1"
	client := &fakeHeadersClient{responses: map[string]json.RawMessage{
		wantKey: json.RawMessage(`[{"date_start":"2026-09-01","spend":"12.34"}]`),
	}}

	res := syncCascadeResource(context.Background(), client, db, "insights", "/{adId}/insights", "ads",
		"adId", map[string]string{"fields": insightsSyncFields, "time_increment": "1"}, io.Discard)
	if res.Err != nil {
		t.Fatalf("syncCascadeResource returned error: %v", res.Err)
	}
	if res.Count != 1 {
		t.Fatalf("count = %d, want 1", res.Count)
	}
	if len(client.calls) != 1 || client.calls[0] != wantKey {
		t.Fatalf("calls = %v, want [%q]", client.calls, wantKey)
	}
}

// TestSyncCascadeResource_Pagination verifies the cursor loop advances
// across pages for a single parent (via paginatedGet's Meta-cursor
// auto-detection) instead of stopping after page one.
func TestSyncCascadeResource_Pagination(t *testing.T) {
	db := newTestStore(t)
	if err := db.UpsertMe(json.RawMessage(`{"id":"act_333","name":"acct"}`)); err != nil {
		t.Fatalf("seed me: %v", err)
	}

	client := &fakeHeadersClient{responses: map[string]json.RawMessage{
		"/act_333/adsets|limit=100": json.RawMessage(`{"data":[{"id":"as1"}],"paging":{"cursors":{"after":"CURSOR2"},"next":"https://graph.facebook.test/act_333/adsets?after=CURSOR2"}}`),
	}}
	// paginatedGet drives the second-page request itself; the fake client's
	// second response (matched by the "after" param it receives) needs a
	// terminal empty-cursors envelope so the loop stops after 2 pages.
	client.responses["/act_333/adsets|limit=100|after=CURSOR2"] = json.RawMessage(`{"data":[{"id":"as2"}],"paging":{"cursors":{}}}`)

	res := syncCascadeResource(context.Background(), client, db, "adsets", "/{adAccountId}/adsets", "me", "adAccountId", nil, io.Discard)
	if res.Err != nil {
		t.Fatalf("syncCascadeResource returned error: %v", res.Err)
	}
	if res.Count != 2 {
		t.Fatalf("count = %d, want 2 (across both pages)", res.Count)
	}
	if len(client.calls) != 2 {
		t.Fatalf("calls = %v, want 2 (one per page)", client.calls)
	}
}
