// Bark fork addition (not upstream) — regression tests for the
// act_-prefix bug confirmed live in fatigue's --account scope and
// reconcile's account-side query: both compare against Meta's
// bare-numeric account_id with no prefix tolerance, so --account
// act_<id> matched nothing even against fully-synced data.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// TestFatigueAccountScope_MatchesBarePrefixedAccountID reproduces the
// live finding exactly: --campaign worked (campaign_id has no prefix
// ambiguity) while --account returned zero rows against the identical
// backfilled data, because ads' account_id is stored bare-numeric while
// --account is passed act_-prefixed.
func TestFatigueAccountScope_MatchesBarePrefixedAccountID(t *testing.T) {
	db := newTestStore(t)
	if err := db.Upsert("ads", "ad1", json.RawMessage(`{"id":"ad1","name":"Ad One","account_id":"992420150824125","campaign_id":"c1"}`)); err != nil {
		t.Fatalf("seed ad: %v", err)
	}
	// 3 daily insight rows: fatigue needs len(days) >= 3 to compute a slope.
	for i := 0; i < 3; i++ {
		date := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		row := fmt.Sprintf(`{"ad_id":"ad1","date_start":%q,"impressions":"1000","spend":"10.00","cpm":"10.0","ctr":"1.0","frequency":"1.0"}`, date)
		if err := db.Upsert("insights", fmt.Sprintf("ad1_%d", i), json.RawMessage(row)); err != nil {
			t.Fatalf("seed insight %d: %v", i, err)
		}
	}

	flags := &rootFlags{asJSON: true}
	cmd := newNovelFatigueCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--account", "act_992420150824125", "--window", "30d", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fatigue --account failed: %v", err)
	}

	var view fatigueView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if view.Total != 1 {
		t.Fatalf("total = %d, want 1 — account-scope match regressed. raw: %s", view.Total, out.String())
	}
	if len(view.Rows) != 1 || view.Rows[0].AdID != "ad1" {
		t.Fatalf("rows = %+v, want exactly ad1", view.Rows)
	}
}

// TestReconcileAccountScope_MatchesBarePrefixedAccountID reproduces the
// live finding: account_spend read 0 on every day because the SQL-level
// account filter did a plain `= ?` against act_-prefixed input while
// account_id is stored bare-numeric — the same ambiguity this file
// already handles correctly in Go for the ad-level rows, just missed here.
func TestReconcileAccountScope_MatchesBarePrefixedAccountID(t *testing.T) {
	db := newTestStore(t)
	today := time.Now().Format("2006-01-02")
	// Account-level row: no ad_id at all, matching what an account-scoped
	// insights sync (no ad_id in its field list) actually produces.
	accountRow := fmt.Sprintf(`{"account_id":"992420150824125","date_start":%q,"spend":"500.00"}`, today)
	if err := db.Upsert("insights", "acct_row", json.RawMessage(accountRow)); err != nil {
		t.Fatalf("seed account insight: %v", err)
	}
	adRow := fmt.Sprintf(`{"ad_id":"ad1","account_id":"992420150824125","date_start":%q,"spend":"120.00"}`, today)
	if err := db.Upsert("insights", "ad_row", json.RawMessage(adRow)); err != nil {
		t.Fatalf("seed ad insight: %v", err)
	}

	flags := &rootFlags{asJSON: true}
	cmd := newNovelReconcileCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--account", "act_992420150824125", "--since", "7d", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("reconcile --account failed: %v", err)
	}

	var view reconcileView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if len(view.Rows) != 1 {
		t.Fatalf("rows = %v, want exactly 1 day. raw: %s", view.Rows, out.String())
	}
	if view.Rows[0].AccountSpend != 500.00 {
		t.Fatalf("account_spend = %v, want 500.00 — account-scope match regressed (would previously read 0). raw: %s", view.Rows[0].AccountSpend, out.String())
	}
	if view.Rows[0].InsightsSpend != 120.00 {
		t.Fatalf("insights_spend = %v, want 120.00", view.Rows[0].InsightsSpend)
	}
}

// TestInventoryAccountScope_MatchesBarePrefixedAccountID reproduces the
// marketing team's exact live report: "the account filter on inventory
// returns nothing." Same act_-prefix bug as fatigue/reconcile, just in a
// fourth (of five total) copy-pasted occurrence.
func TestInventoryAccountScope_MatchesBarePrefixedAccountID(t *testing.T) {
	db := newTestStore(t)
	if err := db.Upsert("ads", "ad1", json.RawMessage(`{"id":"ad1","account_id":"992420150824125","effective_status":"WITH_ISSUES","status":"ACTIVE"}`)); err != nil {
		t.Fatalf("seed ad: %v", err)
	}

	flags := &rootFlags{asJSON: true}
	cmd := newNovelInventoryCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--account", "act_992420150824125", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("inventory --account failed: %v", err)
	}

	var view inventoryView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if view.TotalAds != 1 {
		t.Fatalf("total_ads = %d, want 1 — account-scope match regressed (this is the exact bug the marketing team reported). raw: %s", view.TotalAds, out.String())
	}
}

// TestStaleAccountScope_MatchesBarePrefixedAccountID: same bug, fifth
// occurrence.
func TestStaleAccountScope_MatchesBarePrefixedAccountID(t *testing.T) {
	db := newTestStore(t)
	if err := db.Upsert("ads", "ad1", json.RawMessage(`{"id":"ad1","account_id":"992420150824125","status":"ACTIVE"}`)); err != nil {
		t.Fatalf("seed ad: %v", err)
	}
	// No insights rows at all -> zero impressions in the window -> stale.

	flags := &rootFlags{asJSON: true}
	cmd := newNovelStaleCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--account", "act_992420150824125", "--days", "90", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("stale --account failed: %v", err)
	}

	var view staleView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if view.Total != 1 {
		t.Fatalf("total = %d, want 1 — account-scope match regressed. raw: %s", view.Total, out.String())
	}
}

// TestBottleneckAccountScope_MatchesBarePrefixedAccountID: bottleneck
// filters adsets (not ads) by account_id — a field that, unlike ads',
// hadn't been added to accountScopedSyncFields at all until this test's
// bug was found: the earlier act_-prefix SQL fix alone did NOT unblock
// --account here, because the underlying field was simply never
// requested from Meta. Full integration test (not just a field-list
// guard) specifically because that gap survived one whole round of "fix
// the query" without being caught.
func TestBottleneckAccountScope_MatchesBarePrefixedAccountID(t *testing.T) {
	db := newTestStore(t)
	if err := db.Upsert("adsets", "as1", json.RawMessage(`{"id":"as1","name":"Adset One","campaign_id":"c1","account_id":"992420150824125","effective_status":"ACTIVE"}`)); err != nil {
		t.Fatalf("seed adset: %v", err)
	}

	flags := &rootFlags{asJSON: true}
	cmd := newNovelBottleneckCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--account", "act_992420150824125", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bottleneck --account failed: %v", err)
	}

	var view bottleneckView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if view.Total != 1 {
		t.Fatalf("total = %d, want 1 — account-scope match regressed (missing account_id field on synced adsets, or the act_-prefix bug). raw: %s", view.Total, out.String())
	}
}

// TestLearningAccountScope_MatchesBarePrefixedAccountID: same root cause
// as bottleneck above — learning also filters adsets by account_id.
func TestLearningAccountScope_MatchesBarePrefixedAccountID(t *testing.T) {
	db := newTestStore(t)
	// No start_time: hits learning.go's "missing start_time sentinel" path
	// (daysInLearning == -1), which is kept regardless of --min-days —
	// avoids the test depending on wall-clock date math.
	if err := db.Upsert("adsets", "as1", json.RawMessage(`{"id":"as1","name":"Adset One","campaign_id":"c1","account_id":"992420150824125","learning_stage_info":{"status":"LEARNING"}}`)); err != nil {
		t.Fatalf("seed adset: %v", err)
	}

	flags := &rootFlags{asJSON: true}
	cmd := newNovelLearningCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--account", "act_992420150824125", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("learning --account failed: %v", err)
	}

	var view learningView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if view.Total != 1 {
		t.Fatalf("total = %d, want 1 — account-scope match regressed. raw: %s", view.Total, out.String())
	}
}

// TestAccountScopedSyncFields_AdsetsCarriesAccountID guards the field this
// round's live testing found missing: bottleneck/learning both filter
// synced adsets by account_id, which accountScopedSyncFields["adsets"]
// never requested.
func TestAccountScopedSyncFields_AdsetsCarriesAccountID(t *testing.T) {
	fields, ok := accountScopedSyncFields["adsets"]
	if !ok {
		t.Fatalf("accountScopedSyncFields has no entry for adsets")
	}
	if !strings.Contains(fields, "account_id") {
		t.Fatalf("adsets fields = %q, missing account_id — bottleneck/learning --account would regress to matching nothing", fields)
	}
}

// TestOverlap_ReportsNotAvailableViaAPI verifies overlap reports the
// honest, actionable verdict — confirmed against Meta's current Custom
// Audience API reference that no overlap or membership data is exposed at
// all, so this can never resolve to real data no matter what's synced.
// Regression guard against reintroducing the old "no-data"/"sync ... for
// this pair" wording, which reads as a temporary gap rather than a
// structural API limitation.
func TestOverlap_ReportsNotAvailableViaAPI(t *testing.T) {
	db := newTestStore(t)

	flags := &rootFlags{asJSON: true}
	cmd := newNovelOverlapCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--audience", "a1", "--audience", "a2", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("overlap failed: %v", err)
	}

	var view overlapView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if len(view.Pairs) != 1 || view.Pairs[0].Verdict != "not-available-via-api" {
		t.Fatalf("verdict = %+v, want exactly one pair with not-available-via-api", view.Pairs)
	}
	if view.Note == "" {
		t.Fatalf("expected a top-level Note explaining the structural API limitation, got none")
	}
}

// fieldFilteringClient simulates Meta's actual "fields are authoritative"
// behavior: a response only contains exactly the fields listed in the
// request's `fields` param (plus id, which Meta always includes). This is
// what actually catches "a field is missing from the sync field list"
// bugs — unlike a hand-seeded test fixture that already has every field
// present regardless of what sync would really have requested, which is
// exactly how the learning_stage_info/daily_budget/start_time and clicks
// gaps survived two earlier rounds of tests that seeded fixtures directly.
type fieldFilteringClient struct {
	responses map[string]map[string]any // full, unfiltered field set per path
	calls     []string
}

func (f *fieldFilteringClient) GetWithHeaders(_ context.Context, path string, params map[string]string, _ map[string]string) (json.RawMessage, error) {
	f.calls = append(f.calls, path)
	full, ok := f.responses[path]
	if !ok {
		return json.RawMessage(`[]`), nil
	}
	requested := map[string]bool{"id": true} // Meta always includes id
	for _, field := range strings.Split(params["fields"], ",") {
		requested[field] = true
	}
	filtered := map[string]any{}
	for k, v := range full {
		if requested[k] {
			filtered[k] = v
		}
	}
	data, err := json.Marshal([]map[string]any{filtered})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// TestSyncThenLearning_EndToEnd_CatchesMissingFieldGaps is the real
// regression guard for the learning_stage_info/daily_budget/start_time
// gap: it runs the actual sync cascade with the real
// accountScopedSyncFields["adsets"] list against a client that only
// returns what was actually requested, then runs the real `learning`
// command against the resulting store. A future removal of any field
// learning.go depends on fails this test; a hand-seeded fixture would not.
func TestSyncThenLearning_EndToEnd_CatchesMissingFieldGaps(t *testing.T) {
	db := newTestStore(t)
	if err := db.UpsertMe(json.RawMessage(`{"id":"act_1","name":"acct"}`)); err != nil {
		t.Fatalf("seed me: %v", err)
	}

	client := &fieldFilteringClient{responses: map[string]map[string]any{
		"/act_1/adsets": {
			"id": "as1", "name": "Adset One", "campaign_id": "c1", "account_id": "1",
			"learning_stage_info": map[string]any{"status": "LEARNING"},
			"daily_budget":        "5000",
			"status":              "ACTIVE", "effective_status": "ACTIVE",
		},
	}}

	fields := accountScopedSyncFields["adsets"]
	syncRes := syncCascadeResource(context.Background(), client, db, "adsets", "/{adAccountId}/adsets", "me", "adAccountId",
		map[string]string{"fields": fields}, 1, io.Discard)
	if syncRes.Err != nil {
		t.Fatalf("sync adsets failed: %v", syncRes.Err)
	}
	if syncRes.Count != 1 {
		t.Fatalf("sync adsets stored %d rows, want 1", syncRes.Count)
	}

	cmd := newNovelLearningCmd(&rootFlags{asJSON: true})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--account", "act_1", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("learning --account failed: %v", err)
	}

	var view learningView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if view.Total != 1 {
		t.Fatalf("learning found %d adsets after a real sync (want 1) — accountScopedSyncFields[\"adsets\"] is missing a field learning.go needs. raw: %s", view.Total, out.String())
	}
}

// TestSyncThenDecay_EndToEnd_CatchesMissingClicksField is the real
// regression guard for decay's clicks gap: runs the actual ads+insights
// sync cascade with the real field lists against a client that only
// returns what was requested, then runs the real `decay` command. Without
// clicks in insightsSyncFields, decay's CTR always computes as 0/impressions
// = 0 regardless of real data — this test asserts a nonzero CTR instead.
func TestSyncThenDecay_EndToEnd_CatchesMissingClicksField(t *testing.T) {
	db := newTestStore(t)
	if err := db.UpsertMe(json.RawMessage(`{"id":"act_1","name":"acct"}`)); err != nil {
		t.Fatalf("seed me: %v", err)
	}

	client := &fieldFilteringClient{responses: map[string]map[string]any{
		"/act_1/ads": {
			"id": "ad1", "name": "Ad One", "adset_id": "as1", "campaign_id": "c1", "account_id": "1",
			"creative": map[string]any{"id": "cr1"},
			"status":   "ACTIVE", "effective_status": "ACTIVE",
		},
		"/ad1/insights": {
			"ad_id": "ad1", "date_start": "2026-09-01", "date_stop": "2026-09-01",
			"impressions": "1000", "clicks": "50", "spend": "10.00",
		},
	}}

	adsFields := accountScopedSyncFields["ads"]
	adsRes := syncCascadeResource(context.Background(), client, db, "ads", "/{adAccountId}/ads", "me", "adAccountId",
		map[string]string{"fields": adsFields}, 1, io.Discard)
	if adsRes.Err != nil || adsRes.Count != 1 {
		t.Fatalf("sync ads: count=%d err=%v", adsRes.Count, adsRes.Err)
	}

	// This day goes through the real sync path with the real
	// insightsSyncFields list — the actual thing under test: does that
	// field list, round-tripped through a client that only returns what
	// was requested, still carry clicks?
	insightsRes := syncCascadeResource(context.Background(), client, db, "insights", "/{adId}/insights", "ads", "adId",
		map[string]string{"fields": insightsSyncFields, "time_increment": "1"}, 1, io.Discard)
	if insightsRes.Err != nil || insightsRes.Count != 1 {
		t.Fatalf("sync insights: count=%d err=%v", insightsRes.Count, insightsRes.Err)
	}
	// decay needs >= 2 days to compute a slope; a second day is upserted
	// directly rather than round-tripped through the fake client a second
	// time (fieldFilteringClient models one fixed response per path, and
	// the field-list claim under test is already covered by the day above).
	if err := db.Upsert("insights", "ad1_2026-09-02",
		json.RawMessage(`{"id":"ad1_2026-09-02","ad_id":"ad1","date_start":"2026-09-02","date_stop":"2026-09-02","impressions":"1000","clicks":"50","spend":"10.00"}`)); err != nil {
		t.Fatalf("seed second insights day: %v", err)
	}

	cmd := newNovelDecayCmd(&rootFlags{asJSON: true})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--creative-id", "cr1", "--db", db.Path()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("decay --creative-id failed: %v", err)
	}

	var view decayView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal output: %v, raw: %s", err, out.String())
	}
	if view.Verdict == "no-data" {
		t.Fatalf("decay found no data after a real sync — creative->ads lookup regressed. raw: %s", out.String())
	}
	if view.FirstCtr == 0 {
		t.Fatalf("first_ctr = 0, want 5.0 (50 clicks / 1000 impressions * 100) — clicks is missing from insightsSyncFields again. raw: %s", out.String())
	}
}
