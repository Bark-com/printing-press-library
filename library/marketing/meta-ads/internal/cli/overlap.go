// Copyright 2026 dhilip-subramanian. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mvanhorn/printing-press-library/library/marketing/meta-ads/internal/store"

	"github.com/spf13/cobra"
)

type overlapPair struct {
	AudienceA  string  `json:"audience_a"`
	AudienceB  string  `json:"audience_b"`
	OverlapPct float64 `json:"overlap_pct,omitempty"`
	Verdict    string  `json:"verdict"`
	Note       string  `json:"note,omitempty"`
}

type overlapView struct {
	Audiences []string      `json:"audiences"`
	Pairs     []overlapPair `json:"pairs"`
	Note      string        `json:"note,omitempty"`
}

func newNovelOverlapCmd(flags *rootFlags) *cobra.Command {
	var flagAudience []string
	var dbPath string

	cmd := &cobra.Command{
		Use:   "overlap",
		Short: "Pairwise overlap percentages across custom audiences. NOT AVAILABLE: see Long.",
		Long: `Bark fork note (confirmed against Meta's current Custom Audience API
reference — no overlap-related field or edge exists, and no field exposes
audience membership either, so it can't be computed locally from member
lists): Meta's Marketing API does not expose audience overlap data in any
form. This command can never produce real output no matter what has been
synced — pairwise audience overlap is only available through Business
Manager's "Audience overlap" tool in the Ads Manager UI. This was true
even at this CLI's original design time (its own design brief hedged with
"uses Meta's audience_overlap endpoint when available, else local set
math" — neither path was ever actually reachable, which is also why this
command's own upstream verification only ever asserted the "no data"
placeholder response, never real output).

Kept in the tool surface (rather than removed) so an agent calling it on a
marketing team's behalf gets a clear, actionable answer instead of no
capability to ask about at all — see the not-available-via-api verdict
below.`,
		Example: `  meta-ads-pp-cli overlap --audience 23847001 --audience 23847002 --audience 23847003 --agent
  meta-ads-pp-cli overlap --audience 23847001 --audience 23847002 --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				fmt.Fprintln(cmd.OutOrStdout(), "would report not-available-via-api for every pair — Meta's API does not expose audience overlap data (see --help)")
				return nil
			}
			if len(flagAudience) < 2 {
				_ = cmd.Usage()
				return usageErr(fmt.Errorf("at least two --audience flags are required"))
			}

			if dbPath == "" {
				dbPath = defaultDBPath("meta-ads-pp-cli")
			}
			db, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			defer db.Close()

			view := overlapView{
				Audiences: flagAudience,
				Pairs:     make([]overlapPair, 0),
			}

			// Walk every unordered pair
			for i := 0; i < len(flagAudience); i++ {
				for j := i + 1; j < len(flagAudience); j++ {
					a := flagAudience[i]
					b := flagAudience[j]
					pct, ok := lookupOverlap(cmd.Context(), db, a, b)
					pair := overlapPair{AudienceA: a, AudienceB: b}
					if !ok {
						// Bark fork fix: was "no-data" / "sync ... for this
						// pair", which reads as "temporarily missing, try
						// syncing" when the true answer is "can never work
						// via the API, don't bother" — see the command's
						// --help for why. lookupOverlap will never find a
						// row: nothing populates resource_type
						// audience_overlap/customaudiences_overlap anywhere
						// in this codebase, and nothing ever can, since Meta
						// doesn't expose this data. The query is left in
						// place rather than short-circuited, in case a
						// future world (a manual import, or Meta adding
						// this to the API) ever does populate it.
						pair.Verdict = "not-available-via-api"
						pair.Note = "Meta's Marketing API does not expose audience overlap data for any custom audience pair; check Business Manager's Audience overlap tool in the Ads Manager UI instead"
					} else {
						pair.OverlapPct = pct
						if pct >= 30 {
							pair.Verdict = "cannibalization-risk"
							pair.Note = "consider consolidating or excluding one audience from the other's targeting"
						} else if pct >= 15 {
							pair.Verdict = "moderate-overlap"
						} else {
							pair.Verdict = "low-overlap"
						}
					}
					view.Pairs = append(view.Pairs, pair)
				}
			}

			unavailable := 0
			for _, p := range view.Pairs {
				if p.Verdict == "not-available-via-api" {
					unavailable++
				}
			}
			if unavailable == len(view.Pairs) {
				view.Note = "Meta's Marketing API does not expose audience overlap data in any form (confirmed against the current Custom Audience API reference — no overlap field/edge, and no member-list edge to compute it from locally either). This is a structural API limitation, not a sync gap: no amount of syncing will ever populate this. Check Business Manager's Audience overlap tool in the Ads Manager UI instead."
			}
			return printJSONFiltered(cmd.OutOrStdout(), view, flags)
		},
	}
	cmd.Flags().StringSliceVar(&flagAudience, "audience", nil, "Custom audience ID (specify two or more)")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (defaults to ~/.meta-ads-pp-cli/data.db)")
	return cmd
}

func lookupOverlap(ctx context.Context, db *store.Store, a, b string) (float64, bool) {
	q := `SELECT data FROM resources
		WHERE resource_type IN ('audience_overlap', 'customaudiences_overlap')
		  AND ((json_extract(data, '$.audience_a') = ? AND json_extract(data, '$.audience_b') = ?)
		    OR (json_extract(data, '$.audience_a') = ? AND json_extract(data, '$.audience_b') = ?))
		LIMIT 1`
	row := db.DB().QueryRowContext(ctx, q, a, b, b, a)
	var data []byte
	if err := row.Scan(&data); err != nil {
		return 0, false
	}
	var raw struct {
		OverlapPct float64 `json:"overlap_pct"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, false
	}
	return raw.OverlapPct, true
}
