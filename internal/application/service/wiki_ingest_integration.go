package service

import (
	"fmt"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

// planSegmentation decides how mapOneDocument processes a document and, when
// the document exceeds the budget, partitions it up front.
//
//   - tokenBudget > 0 enables the automatic language-density mode: the
//     effective char cap is derived by estimateSegmentCharBudget from the
//     content's CJK ratio (mode "auto_token"), so one setting serves both
//     Chinese and English books.
//   - tokenBudget <= 0 keeps the manual cap as-is (mode "manual").
//   - effectiveMax <= 0 (misconfigured setting slipping past the registry
//     validation) falls back to the same defensive default
//     splitContentSegments applies internally, so the reported cap matches
//     what the segmenter actually used.
//
// A document at or under the effective cap stays on the legacy single-shot
// path (segments == nil, multi == false - zero behavioural change); a longer
// one is split by splitContentSegments into budget-sized segments for the
// map-reduce pipeline.
func planSegmentation(rawRuneCount int, content string, chunks []*types.Chunk, maxChars, tokenBudget int) (segments []contentSegment, multi bool, effectiveMax int, mode string) {
	effectiveMax = maxChars
	mode = "manual"
	if tokenBudget > 0 {
		effectiveMax = estimateSegmentCharBudget(content, tokenBudget)
		mode = "auto_token"
	}
	if effectiveMax <= 0 {
		effectiveMax = 200000
		mode = "manual"
	}
	if rawRuneCount > effectiveMax {
		multi = true
		segments = splitContentSegments(content, chunks, effectiveMax)
	}
	return segments, multi, effectiveMax, mode
}

// segmentRuneRanges reports the rune range each segment covers within the
// flattened document, accumulating per-segment rune counts the same way
// generateSummaryMultiSegment lays out reduce input offsets.
func segmentRuneRanges(segments []contentSegment) []string {
	ranges := make([]string, 0, len(segments))
	offset := 0
	for _, seg := range segments {
		span := utf8.RuneCountInString(seg.Content)
		end := offset + span - 1
		if span == 0 {
			end = offset
		}
		ranges = append(ranges, fmt.Sprintf("%d-%d", offset, end))
		offset += span
	}
	return ranges
}
