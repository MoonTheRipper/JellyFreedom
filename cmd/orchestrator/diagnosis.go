package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"jellyfreedom/internal/indexer"
	"jellyfreedom/internal/picker"
)

// ── "no suitable release found" diagnostics ───────────────────────────────────
//
// A failed request used to end at the string "no suitable release found", which tells
// the user nothing: were there no results at all, were they all under the seeder floor,
// were they all camera rips, or were they all too large? Answering that meant reading
// the server log.
//
// noReleaseError carries the scored candidate list and the rule that rejected each one,
// so GET /api/queue/{id}/diagnosis can show "these 12 releases were found and here is
// exactly which rule rejected each" (API contract §7).

type diagFilters struct {
	MinSeeders int  `json:"min_seeders"`
	MaxSizeGB  int  `json:"max_size_gb"`
	RejectCAM  bool `json:"reject_cam"`
}

type diagCandidate struct {
	Title      string  `json:"title"`
	Seeders    int     `json:"seeders"`
	SizeGB     float64 `json:"size_gb"`
	RejectedBy string  `json:"rejected_by"`
}

type diagnosis struct {
	Reason     string          `json:"reason"`
	Filters    diagFilters     `json:"filters"`
	Candidates []diagCandidate `json:"candidates"`
	// TotalFound is the raw indexer result count BEFORE filtering, so "0 results" and
	// "40 results, all rejected" are distinguishable at a glance.
	TotalFound int `json:"total_found"`
}

// noReleaseError is the typed failure the queue worker records for a request that found
// no usable release.
type noReleaseError struct {
	d diagnosis
}

// maxDiagCandidates bounds what is stored per failed queue row. A broad search can
// return hundreds of results and the diagnosis is persisted in the database.
const maxDiagCandidates = 40

func newNoReleaseError(releases []indexer.Release, cfg picker.Config, title, year string) *noReleaseError {
	d := diagnosis{
		Reason: "no_release",
		Filters: diagFilters{
			MinSeeders: cfg.MinSeeders,
			MaxSizeGB:  cfg.MaxSizeGB,
			RejectCAM:  cfg.RejectCAM,
		},
		Candidates: []diagCandidate{},
		TotalFound: len(releases),
	}
	for i, r := range releases {
		if i >= maxDiagCandidates {
			break
		}
		d.Candidates = append(d.Candidates, diagCandidate{
			Title:   r.Title,
			Seeders: r.Seeders,
			SizeGB:  float64(r.SizeBytes) / (1024 * 1024 * 1024),
			// requireTitleMatch=false: the resolve pipeline only PREFERS a title match
			// (Score adds a bonus), so reporting a mismatch as a rejection would be a lie.
			RejectedBy: picker.RejectedBy(r, cfg, title, year, false),
		})
	}
	return &noReleaseError{d: d}
}

// Error is the human-readable message stored in the queue row's error_msg.
func (e *noReleaseError) Error() string {
	if e.d.TotalFound == 0 {
		return "No releases were found for this title. The indexer returned nothing — " +
			"check that Prowlarr has working indexers for this content type."
	}
	rejected := map[string]int{}
	for _, c := range e.d.Candidates {
		if c.RejectedBy != "" {
			rejected[c.RejectedBy]++
		}
	}
	switch {
	case rejected[picker.RejectMinSeeders] == len(e.d.Candidates):
		return fmt.Sprintf("Found %d releases, but every one is below the minimum of %d seeders. "+
			"Lower it in Settings → Quality, or try again later.", e.d.TotalFound, e.d.Filters.MinSeeders)
	case rejected[picker.RejectCAMRule] == len(e.d.Candidates):
		return fmt.Sprintf("Found %d releases, but every one is a camera/telesync rip. "+
			"Wait for a proper release, or allow them in Settings → Quality.", e.d.TotalFound)
	case rejected[picker.RejectMaxSize] == len(e.d.Candidates):
		return fmt.Sprintf("Found %d releases, but every one is larger than the %d GB limit. "+
			"Raise it in Settings → Quality.", e.d.TotalFound, e.d.Filters.MaxSizeGB)
	default:
		return fmt.Sprintf("Found %d releases, but none passed the quality filters. "+
			"Open the request for the per-release breakdown.", e.d.TotalFound)
	}
}

// JSON is the body served by GET /api/queue/{id}/diagnosis.
func (e *noReleaseError) JSON() string {
	b, err := json.Marshal(e.d)
	if err != nil {
		slog.Error("could not encode the release diagnosis", "err", err)
		return ""
	}
	return string(b)
}
