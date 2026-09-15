// Bark fork addition (not upstream) — exercises syncCascadeResource, the
// account/ad-scoped sync support added for
// https://github.com/mvanhorn/printing-press-library/issues/1434, with a
// fake client so it needs no real credentials or network access.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/printing-press-library/library/marketing/meta-ads/internal/client"
	"github.com/mvanhorn/printing-press-library/library/marketing/meta-ads/internal/store"
)

// fakeHeadersClient answers GetWithHeaders() from a table of canned
// responses keyed by "path|k=v|k=v..." (params in insertion order matters
// only for the keys the test cares about), recording every call so tests
// can assert exactly which requests were made. deniedPaths simulates a
// per-parent 403 (access-warning) instead of returning a canned response.
// delay, when set, sleeps before responding — used to make the
// cascadeSyncBudget cutoff deterministically testable without a real
// 25-second wait.
type fakeHeadersClient struct {
	mu        sync.Mutex
	responses map[string]json.RawMessage
	denied    map[string]bool
	delay     time.Duration
	calls     []string
}

func (f *fakeHeadersClient) GetWithHeaders(_ context.Context, path string, params map[string]string, _ map[string]string) (json.RawMessage, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	key := path
	for _, k := range []string{"limit", "after", "fields", "time_increment"} {
		if v, ok := params[k]; ok {
			key += fmt.Sprintf("|%s=%s", k, v)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if f.denied[path] {
		return nil, &client.APIError{Method: "GET", Path: path, StatusCode: 403, Body: "denied"}
	}
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

	res := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId", nil, 1, io.Discard)
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

// TestSyncCascadeResource_AccountScopedFields verifies campaigns sync
// requests the explicit accountScopedSyncFields list (RunE's wiring, not a
// nil fields param) — regression test for the case a real Bark ad account
// with an ads_read-only token actually hit: omitting `fields` makes Meta
// fall back to its default field set for the node, which can include
// fields gated behind more than ads_read and 403 the whole request even
// though the identical endpoint with an explicit narrow field list (what
// the live-read tools always send) succeeds.
func TestSyncCascadeResource_AccountScopedFields(t *testing.T) {
	db := newTestStore(t)
	if err := db.UpsertMe(json.RawMessage(`{"id":"act_111","name":"acct"}`)); err != nil {
		t.Fatalf("seed me: %v", err)
	}

	wantFields, ok := accountScopedSyncFields["campaigns"]
	if !ok {
		t.Fatalf("accountScopedSyncFields has no entry for campaigns")
	}
	wantKey := "/act_111/campaigns|limit=100|fields=" + wantFields
	client := &fakeHeadersClient{responses: map[string]json.RawMessage{
		wantKey: json.RawMessage(`[{"id":"c1","name":"Campaign One"}]`),
	}}

	res := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId",
		map[string]string{"fields": wantFields}, 1, io.Discard)
	if res.Err != nil {
		t.Fatalf("syncCascadeResource returned error: %v", res.Err)
	}
	if len(client.calls) != 1 || client.calls[0] != wantKey {
		t.Fatalf("calls = %v, want [%q]", client.calls, wantKey)
	}
}

// TestAccountScopedSyncFields_NoFinancialFields is a cheap guard against
// reintroducing the exact class of bug this was added to fix: none of
// these field lists should request budget/financial fields, or fields
// touching audience membership/targeting/data-source, which are the kind
// of field Meta gates behind more than ads_read (ads_management, for
// customaudiences).
func TestAccountScopedSyncFields_NoFinancialFields(t *testing.T) {
	suspect := []string{
		"budget", "daily_budget", "lifetime_budget", "budget_remaining", "spend_cap",
		"rule", "data_source", "subscription_info", "lookalike_spec",
	}
	for resource, fields := range accountScopedSyncFields {
		for _, s := range suspect {
			if strings.Contains(fields, s) {
				t.Errorf("accountScopedSyncFields[%q] = %q contains %q — likely to require more than the base ads_read/ads_management grant", resource, fields, s)
			}
		}
	}
	for _, resource := range []string{"campaigns", "adsets", "ads", "customaudiences"} {
		if _, ok := accountScopedSyncFields[resource]; !ok {
			t.Errorf("accountScopedSyncFields is missing an entry for %q", resource)
		}
	}
}

// TestSyncCascadeResource_AdScopedRequiresParent verifies insights (an
// ad-scoped resource) warns rather than errors when the `ads` table hasn't
// been synced yet, instead of silently syncing zero rows.
func TestSyncCascadeResource_AdScopedRequiresParent(t *testing.T) {
	db := newTestStore(t)
	client := &fakeHeadersClient{responses: map[string]json.RawMessage{}}

	res := syncCascadeResource(context.Background(), client, db, "insights", "/{adId}/insights", "ads", "adId", map[string]string{"fields": insightsSyncFields}, 1, io.Discard)
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
		"adId", map[string]string{"fields": insightsSyncFields, "time_increment": "1"}, 1, io.Discard)
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

	res := syncCascadeResource(context.Background(), client, db, "adsets", "/{adAccountId}/adsets", "me", "adAccountId", nil, 1, io.Discard)
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

// TestSyncCascadeResource_PartialDenialReportsWarning verifies that when one
// account succeeds and another is denied, the result is a Warn carrying the
// partial count — not a plain success that silently hides the denied
// account. Regression test for review finding "Partial Sync Reports
// Success" on Bark-com/printing-press-library#3.
func TestSyncCascadeResource_PartialDenialReportsWarning(t *testing.T) {
	db := newTestStore(t)
	for _, acct := range []string{"act_ok", "act_denied"} {
		if err := db.UpsertMe(json.RawMessage(fmt.Sprintf(`{"id":%q,"name":"acct"}`, acct))); err != nil {
			t.Fatalf("seed me: %v", err)
		}
	}

	client := &fakeHeadersClient{
		responses: map[string]json.RawMessage{
			"/act_ok/campaigns|limit=100": json.RawMessage(`[{"id":"c1"}]`),
		},
		denied: map[string]bool{"/act_denied/campaigns": true},
	}

	res := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId", nil, 1, io.Discard)
	if res.Err != nil {
		t.Fatalf("expected a warning (partial data), not a hard error: %v", res.Err)
	}
	if res.Warn == nil {
		t.Fatalf("expected Warn to be set when one of two accounts was denied — got a silent success")
	}
	if res.Count != 1 {
		t.Fatalf("count = %d, want 1 (the account that did succeed)", res.Count)
	}
}

// TestSyncCascadeResource_AllDeniedReportsWarningNotSuccess verifies that
// when every parent is denied, the result is a Warn, not a zero-count
// success. Regression test for review finding "Denied Sync Reports Success"
// on Bark-com/printing-press-library#3.
func TestSyncCascadeResource_AllDeniedReportsWarningNotSuccess(t *testing.T) {
	db := newTestStore(t)
	if err := db.UpsertMe(json.RawMessage(`{"id":"act_denied","name":"acct"}`)); err != nil {
		t.Fatalf("seed me: %v", err)
	}

	client := &fakeHeadersClient{denied: map[string]bool{"/act_denied/campaigns": true}}

	res := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId", nil, 1, io.Discard)
	if res.Err != nil {
		t.Fatalf("expected a warning, not a hard error: %v", res.Err)
	}
	if res.Warn == nil {
		t.Fatalf("expected Warn to be set when every account was denied — got a silent zero-count success")
	}
	if res.Count != 0 {
		t.Fatalf("count = %d, want 0", res.Count)
	}
	if !strings.Contains(res.Warn.Error(), "1/1") {
		t.Fatalf("Warn = %q, want it to mention 1/1 parents failed", res.Warn.Error())
	}
}

// withCascadeSyncBudget temporarily lowers cascadeSyncBudget for a test and
// restores it afterward, so the time-budget cutoff is testable without a
// real 25-second wait.
func withCascadeSyncBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := cascadeSyncBudget
	cascadeSyncBudget = d
	t.Cleanup(func() { cascadeSyncBudget = old })
}

// TestSyncCascadeResource_TimeBudgetStopsEarlyAndReportsRemaining verifies
// the core usability fix: an account with more parents than fit in the
// time budget stops cleanly instead of hanging past an MCP client's
// timeout, reports a clean partial success (not a Warn — hitting the
// budget with zero actual failures is expected, successful behavior for a
// large account, not a problem), and surfaces remaining/processed counts
// plus a time_budget_reached event for visibility.
func TestSyncCascadeResource_TimeBudgetStopsEarlyAndReportsRemaining(t *testing.T) {
	withCascadeSyncBudget(t, 15*time.Millisecond)
	db := newTestStore(t)
	for _, acct := range []string{"act_1", "act_2", "act_3"} {
		if err := db.UpsertMe(json.RawMessage(fmt.Sprintf(`{"id":%q,"name":"acct"}`, acct))); err != nil {
			t.Fatalf("seed me: %v", err)
		}
	}

	client := &fakeHeadersClient{
		delay: 20 * time.Millisecond, // longer than the budget: only 1 call fits
		responses: map[string]json.RawMessage{
			"/act_1/campaigns|limit=100": json.RawMessage(`[{"id":"c1"}]`),
			"/act_2/campaigns|limit=100": json.RawMessage(`[{"id":"c2"}]`),
			"/act_3/campaigns|limit=100": json.RawMessage(`[{"id":"c3"}]`),
		},
	}

	var buf bytes.Buffer
	// concurrency=1 so the dispatcher's per-item deadline check is
	// deterministic: item 1 dispatches immediately (budget not yet spent),
	// takes 20ms: by the time item 2 is considered, 20ms > 15ms budget has
	// elapsed, so the dispatcher stops before sending it.
	res := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId", nil, 1, &buf)
	if res.Err != nil {
		t.Fatalf("syncCascadeResource returned error: %v", res.Err)
	}
	if res.Warn != nil {
		t.Fatalf("hitting the time budget with zero failures should not be a Warn, got: %v", res.Warn)
	}
	if res.Count != 1 {
		t.Fatalf("count = %d, want 1 (only one parent should have fit in the budget)", res.Count)
	}
	if len(client.calls) != 1 {
		t.Fatalf("calls = %v, want exactly 1", client.calls)
	}
	out := buf.String()
	if !strings.Contains(out, `"remaining_parents":2`) {
		t.Fatalf("sync_complete event missing remaining_parents:2, got: %s", out)
	}
	if !strings.Contains(out, `"reason":"time_budget_reached"`) {
		t.Fatalf("expected a time_budget_reached sync_warning event, got: %s", out)
	}
}

// TestSyncCascadeResource_ResumesDifferentParentsAcrossCalls verifies the
// other half of the usability fix: a second call with the same tight
// budget makes progress on *different* parents than the first, instead of
// repeatedly re-fetching the same head of the list — the actual mechanism
// that lets a large account's backlog drain over a few calls.
func TestSyncCascadeResource_ResumesDifferentParentsAcrossCalls(t *testing.T) {
	withCascadeSyncBudget(t, 15*time.Millisecond)
	db := newTestStore(t)
	for _, acct := range []string{"act_1", "act_2", "act_3"} {
		if err := db.UpsertMe(json.RawMessage(fmt.Sprintf(`{"id":%q,"name":"acct"}`, acct))); err != nil {
			t.Fatalf("seed me: %v", err)
		}
	}

	client := &fakeHeadersClient{
		delay: 20 * time.Millisecond,
		responses: map[string]json.RawMessage{
			"/act_1/campaigns|limit=100": json.RawMessage(`[{"id":"c1"}]`),
			"/act_2/campaigns|limit=100": json.RawMessage(`[{"id":"c2"}]`),
			"/act_3/campaigns|limit=100": json.RawMessage(`[{"id":"c3"}]`),
		},
	}

	res1 := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId", nil, 1, io.Discard)
	if res1.Count != 1 {
		t.Fatalf("first call count = %d, want 1", res1.Count)
	}
	firstCall := client.calls[0]

	res2 := syncCascadeResource(context.Background(), client, db, "campaigns", "/{adAccountId}/campaigns", "me", "adAccountId", nil, 1, io.Discard)
	if res2.Count != 1 {
		t.Fatalf("second call count = %d, want 1", res2.Count)
	}
	if len(client.calls) != 2 {
		t.Fatalf("calls = %v, want exactly 2 across both invocations", client.calls)
	}
	secondCall := client.calls[1]

	if firstCall == secondCall {
		t.Fatalf("second call repeated the same parent (%q) instead of resuming with a different one — resumability is broken", secondCall)
	}

	ids, err := db.ListIDs("campaigns")
	if err != nil {
		t.Fatalf("ListIDs(campaigns): %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("stored campaign ids = %v, want 2 distinct campaigns (one per call)", ids)
	}
}

// TestOrderCascadeCandidates_NeverFetchedBeforeStale verifies the ordering
// primitive resumability depends on: never-fetched parents sort before a
// parent that was fetched (however recently), and among fetched parents,
// the oldest fetch sorts first.
func TestOrderCascadeCandidates_NeverFetchedBeforeStale(t *testing.T) {
	db := newTestStore(t)
	if err := ensureCascadeFetchStateTable(db); err != nil {
		t.Fatalf("ensureCascadeFetchStateTable: %v", err)
	}
	now := time.Now()
	recordCascadeFetched(db, "campaigns", "act_recent", now)
	recordCascadeFetched(db, "campaigns", "act_stale", now.Add(-24*time.Hour))
	// act_never intentionally has no row in bark_cascade_fetch_state.

	ordered := orderCascadeCandidates(db, "campaigns", []string{"act_recent", "act_stale", "act_never"}, io.Discard)
	want := []string{"act_never", "act_stale", "act_recent"}
	for i, id := range want {
		if ordered[i] != id {
			t.Fatalf("ordered = %v, want %v (never-fetched, then oldest-fetched first)", ordered, want)
		}
	}
}
