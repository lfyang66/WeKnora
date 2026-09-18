package service

import (
	"context"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"golang.org/x/sync/errgroup"
)

// maxPass0SegmentConcurrency bounds parallelism across content segments for
// the multi-segment Pass 0 (and, reused, the summary map phase in
// wiki_summary_multiseg.go). Three concurrent segment calls overlap most of
// the per-segment latency without saturating the synthesis model; each call
// is additionally throttled by the model's own rate limiter, so this limit
// deliberately does NOT couple to model.max_concurrency.
const maxPass0SegmentConcurrency = 3

// maxPass0MergedCandidates caps the merged candidate list produced by the
// multi-segment Pass 0. Thirty segments with 40+ candidates each would flood
// the classification and editor passes with a list no prompt can meaningfully
// consume; when the cap trips, strongest-evidence candidates (most cited
// chunks first) survive and a warning is logged. Truncation only narrows
// this batch's working list; it never fails the document.
const maxPass0MergedCandidates = 500

// segPass0Outcome is the per-segment collection slot for Pass 0 results.
// Each worker writes only its own slot; the deterministic merge runs after
// eg.Wait() establishes happens-before for every slot.
type segPass0Outcome struct {
	entities []extractedItem
	concepts []extractedItem
}

// extractCandidateSlugsMultiSegment runs Pass 0 of the chunk-cited pipeline
// over a segmented (map-reduce) document: every segment is extracted
// concurrently with the single-segment extractCandidateSlugs, results are
// merged per slug across segments, and the merged list receives one final
// LLM-level deduplicateExtractedBatch pass to collapse cross-segment
// variants ("Minsky" vs "明斯基") that per-segment dedup cannot see.
//
// Failure policy mirrors classifyChunkCitations: one failing segment never
// aborts its peers; the call only errors when EVERY segment failed.
//
// Returns (entities, concepts, slugItems, error) with the same shapes as
// extractCandidateSlugs.
func (s *wikiIngestService) extractCandidateSlugsMultiSegment(
	ctx context.Context,
	chatModel chat.Chat,
	kbID string,
	segments []contentSegment,
	lang string,
	oldPageSlugs map[string]bool,
	batchCtx *WikiBatchContext,
) ([]extractedItem, []extractedItem, map[string]extractedItem, error) {
	if len(segments) == 0 {
		return nil, nil, nil, nil
	}
	if len(segments) == 1 {
		return s.extractCandidateSlugs(ctx, chatModel, kbID, segments[0].Content, lang, oldPageSlugs, batchCtx)
	}

	limit := maxPass0SegmentConcurrency
	if len(segments) < limit {
		limit = len(segments)
	}

	outcomes := make([]segPass0Outcome, len(segments))
	failed := make([]bool, len(segments))

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(limit)
	for i := range segments {
		seg := segments[i]
		slot := i
		eg.Go(func() error {
			entities, concepts, _, err := s.extractCandidateSlugs(
				ectx, chatModel, kbID, seg.Content, lang, oldPageSlugs, batchCtx,
			)
			if err != nil {
				logger.Warnf(ectx, "wiki ingest: pass0 segment %d/%d failed, continuing with peers: %v",
					seg.SegmentIndex+1, seg.TotalSegments, err)
				failed[slot] = true
				return nil // don't abort peer segments
			}
			outcomes[slot] = segPass0Outcome{entities: entities, concepts: concepts}
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
	if successCount == 0 {
		return nil, nil, nil, fmt.Errorf("pass0 multi-segment extraction: all %d segments failed", len(segments))
	}
	if successCount < len(segments) {
		logger.Warnf(ctx, "wiki ingest: pass0 multi-segment extraction partially failed: %d/%d segments succeeded",
			successCount, len(segments))
	}

	entities, concepts := mergeSegmentExtractions(ctx, outcomes)

	// LLM-level safety net across segments: every segment already ran its own
	// in-extraction dedup inside extractCandidateSlugs, but cross-segment
	// same-entity variants can only be resolved now that the full merged
	// list exists.
	entities, concepts = s.deduplicateExtractedBatch(ctx, chatModel, kbID, entities, concepts, batchCtx)

	entities, concepts = truncateMergedCandidates(ctx, entities, concepts)

	slugItems := make(map[string]extractedItem, len(entities)+len(concepts))
	for _, item := range entities {
		if item.Slug != "" && item.Name != "" {
			slugItems[item.Slug] = item
		}
	}
	for _, item := range concepts {
		if item.Slug != "" && item.Name != "" {
			slugItems[item.Slug] = item
		}
	}
	return entities, concepts, slugItems, nil
}

// mergeSegmentExtractions folds per-segment Pass 0 results into a single
// (entities, concepts) pair. Merging is deterministic: segments are visited
// in document order, entities before concepts within each segment, so the
// same input always yields the same list order regardless of which segment
// finished first. A slug is owned by the list where it FIRST appeared; a
// later appearance on the other side is dropped with a debug note (keeping
// the first type assignment prevents one candidate from materializing two
// wiki pages). Same-list repeats merge into one item: aliases and source
// chunks union in first-seen order, the longer description/details win.
func mergeSegmentExtractions(ctx context.Context, outcomes []segPass0Outcome) ([]extractedItem, []extractedItem) {
	entityIdx := make(map[string]int)
	conceptIdx := make(map[string]int)
	var entities, concepts []extractedItem

	addOrMerge := func(list []extractedItem, idx map[string]int, item extractedItem) []extractedItem {
		if pos, ok := idx[item.Slug]; ok {
			list[pos] = mergeExtractedSegmentItems(list[pos], item)
			return list
		}
		idx[item.Slug] = len(list)
		return append(list, item)
	}

	for _, out := range outcomes {
		for _, item := range out.entities {
			if item.Slug == "" || item.Name == "" {
				continue
			}
			if _, claimed := conceptIdx[item.Slug]; claimed {
				logger.Debugf(ctx, "wiki ingest: candidate %s extracted as both entity and concept across segments; keeping the concept listing", item.Slug)
				continue
			}
			entities = addOrMerge(entities, entityIdx, item)
		}
		for _, item := range out.concepts {
			if item.Slug == "" || item.Name == "" {
				continue
			}
			if _, claimed := entityIdx[item.Slug]; claimed {
				logger.Debugf(ctx, "wiki ingest: candidate %s extracted as both concept and entity across segments; keeping the entity listing", item.Slug)
				continue
			}
			concepts = addOrMerge(concepts, conceptIdx, item)
		}
	}
	return entities, concepts
}

// mergeExtractedSegmentItems folds src (same slug, from a later segment)
// into dst. Aliases and source chunks union in first-seen order without
// duplicates; the longer description and details win — same-slug items from
// different segments paraphrase the same fact, the longer phrasing carries
// more signal, and the choice is deterministic.
func mergeExtractedSegmentItems(dst, src extractedItem) extractedItem {
	dst.Aliases = unionPreserveOrder(dst.Aliases, src.Aliases)
	dst.SourceChunks = unionPreserveOrder(dst.SourceChunks, src.SourceChunks)
	if utf8.RuneCountInString(src.Description) > utf8.RuneCountInString(dst.Description) {
		dst.Description = src.Description
	}
	if utf8.RuneCountInString(src.Details) > utf8.RuneCountInString(dst.Details) {
		dst.Details = src.Details
	}
	return dst
}

// unionPreserveOrder appends src values to dst, skipping empty strings and
// duplicates, preserving first-seen order on both sides.
func unionPreserveOrder(dst, src []string) []string {
	if len(src) == 0 {
		return dst
	}
	seen := make(map[string]bool, len(dst)+len(src))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range src {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		dst = append(dst, v)
	}
	return dst
}

// truncateMergedCandidates caps the merged candidate pool at
// maxPass0MergedCandidates. When the cap trips, candidates compete on
// evidence strength: items substantively cited by more chunks first, then
// shorter names as the deterministic tie-breaker (shorter surface forms
// tend to be the more canonical naming). Survivors keep their original
// first-seen order within each list.
func truncateMergedCandidates(ctx context.Context, entities, concepts []extractedItem) ([]extractedItem, []extractedItem) {
	total := len(entities) + len(concepts)
	if total <= maxPass0MergedCandidates {
		return entities, concepts
	}

	type weightedCandidate struct {
		item extractedItem
	}
	pool := make([]weightedCandidate, 0, total)
	for _, item := range entities {
		pool = append(pool, weightedCandidate{item: item})
	}
	for _, item := range concepts {
		pool = append(pool, weightedCandidate{item: item})
	}
	sort.SliceStable(pool, func(i, j int) bool {
		ci, cj := len(pool[i].item.SourceChunks), len(pool[j].item.SourceChunks)
		if ci != cj {
			return ci > cj
		}
		return utf8.RuneCountInString(pool[i].item.Name) < utf8.RuneCountInString(pool[j].item.Name)
	})

	keep := make(map[string]bool, maxPass0MergedCandidates)
	for _, w := range pool[:maxPass0MergedCandidates] {
		keep[w.item.Slug] = true
	}
	logger.Warnf(ctx, "wiki ingest: pass0 merged candidates (%d) exceed cap %d; keeping the strongest-evidence candidates",
		total, maxPass0MergedCandidates)

	filter := func(items []extractedItem) []extractedItem {
		out := make([]extractedItem, 0, len(items))
		for _, item := range items {
			if keep[item.Slug] {
				out = append(out, item)
			}
		}
		return out
	}
	return filter(entities), filter(concepts)
}
