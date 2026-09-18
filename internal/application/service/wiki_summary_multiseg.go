package service

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"golang.org/x/sync/errgroup"
)

// segJoinNewline is the newline separator between a segment header and its
// content, and between <partial_summary> blocks in the reduce prompt input.
// Declared as a raw string literal so the newline stays visible without any
// escape-sequence plumbing in generated sources.
const segJoinNewline = `
`

// segmentPartialSummary is one map-phase output: the per-segment summary
// together with the rune range of the flattened document that segment
// covered. Offsets accumulate per-segment rune counts in document order,
// which mirrors how splitContentSegments laid out the segments.
type segmentPartialSummary struct {
	segment   contentSegment
	summary   string
	startChar int // inclusive rune offset of the segment in the document
	endChar   int // inclusive rune offset of the segment in the document
}

// renderPartialSummaries lays out partial summaries for the reduce prompt.
// Each part is wrapped in a <partial_summary> element tagged with its part
// number and source character range so the model knows which slice of the
// document every partial came from while merging.
func renderPartialSummaries(partials []segmentPartialSummary) string {
	parts := make([]string, 0, len(partials))
	for _, p := range partials {
		partLabel := fmt.Sprintf("%d/%d", p.segment.SegmentIndex+1, p.segment.TotalSegments)
		charsLabel := fmt.Sprintf("%d-%d", p.startChar, p.endChar)
		open := fmt.Sprintf("<partial_summary part=%q chars=%q>", partLabel, charsLabel)
		parts = append(parts, open+segJoinNewline+strings.TrimSpace(p.summary)+segJoinNewline+"</partial_summary>"+segJoinNewline)
	}
	return strings.Join(parts, "")
}

// generateSummaryMultiSegment produces the wiki summary page for a
// segmented document.
//
//   - One segment: reproduce the legacy single-shot call exactly (same
//     WikiSummaryPrompt, same template data as mapOneDocument) so small
//     documents see zero behavioral change.
//   - Multiple segments: MAP — summarize every segment concurrently with
//     WikiSummaryPrompt (segment header prepended, same slug listing for
//     wiki-link injection); REDUCE — merge the successful partials into one
//     full-document page with WikiSummaryReducePrompt.
//
// Failure policy: a failing map segment never aborts its peers; the call
// only errors when fewer than half of the segments produced a summary (the
// reduce would misrepresent the document) or the reduce itself fails. The
// returned string keeps the legacy "SUMMARY: ..." + Markdown contract so
// splitSummaryLine consumes it unchanged.
func (s *wikiIngestService) generateSummaryMultiSegment(
	ctx context.Context,
	chatModel chat.Chat,
	segments []contentSegment,
	slugListing, lang string,
	batchCtx *WikiBatchContext,
) (string, error) {
	if len(segments) == 0 {
		return "", fmt.Errorf("summary multi-segment: no segments to summarize")
	}
	if len(segments) == 1 {
		return s.generateWithTemplate(ctx, chatModel, agent.WikiSummaryPrompt, map[string]string{
			"Content":            segments[0].Content,
			"Language":           lang,
			"ExtractedSlugs":     slugListing,
			"CustomInstructions": batchCtx.ContentInstructions,
			"InstructionScope":   "wiki_content",
		})
	}

	limit := maxPass0SegmentConcurrency
	if len(segments) < limit {
		limit = len(segments)
	}

	// Map. Slots keep per-segment outputs so the reduce input is assembled
	// in deterministic document order regardless of completion order.
	summaries := make([]string, len(segments))
	failed := make([]bool, len(segments))

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(limit)
	for i := range segments {
		seg := segments[i]
		slot := i
		eg.Go(func() error {
			content := seg.Content
			if header := renderSegmentHeader(seg, seg.DocTitle); header != "" {
				content = header + segJoinNewline + content
			}
			partial, err := s.generateWithTemplate(ectx, chatModel, agent.WikiSummaryPrompt, map[string]string{
				"Content":            content,
				"Language":           lang,
				"ExtractedSlugs":     slugListing,
				"CustomInstructions": batchCtx.ContentInstructions,
				"InstructionScope":   "wiki_content",
			})
			if err != nil {
				logger.Warnf(ectx, "wiki ingest: summary segment %d/%d failed, continuing with peers: %v",
					seg.SegmentIndex+1, seg.TotalSegments, err)
				failed[slot] = true
				return nil // don't abort peer segments
			}
			summaries[slot] = partial
			return nil
		})
	}
	_ = eg.Wait()

	successCount := len(segments)
	for _, f := range failed {
		if f {
			successCount--
		}
	}
	if successCount*2 < len(segments) {
		return "", fmt.Errorf("summary multi-segment: %d of %d segment summaries failed, too few to reduce",
			len(segments)-successCount, len(segments))
	}
	if successCount < len(segments) {
		logger.Warnf(ctx, "wiki ingest: summary map partially failed: %d/%d segments succeeded",
			successCount, len(segments))
	}

	// Reduce input: document-order partials with part numbers and the rune
	// range each segment covered (failed slots skipped).
	partials := make([]segmentPartialSummary, 0, successCount)
	offset := 0
	for i := range segments {
		if failed[i] {
			continue
		}
		seg := segments[i]
		span := utf8.RuneCountInString(seg.Content)
		end := offset + span - 1
		if span == 0 {
			end = offset
		}
		partials = append(partials, segmentPartialSummary{
			segment:   seg,
			summary:   summaries[i],
			startChar: offset,
			endChar:   end,
		})
		offset += span
	}

	return s.generateWithTemplate(ctx, chatModel, agent.WikiSummaryReducePrompt, map[string]string{
		"PartialSummaries":   renderPartialSummaries(partials),
		"ExtractedSlugs":     slugListing,
		"Language":           lang,
		"CustomInstructions": batchCtx.ContentInstructions,
		"InstructionScope":   "wiki_content",
	})
}
