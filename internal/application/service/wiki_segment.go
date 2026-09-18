package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// This file implements pure content segmentation for the wiki map-reduce
// pipeline. Very long documents (multi-million chars) cannot be sent to the
// LLM as a single blob; splitContentSegments partitions the reconstructed
// text into budget-sized segments along chunk boundaries, preferring to cut
// at H1 ("\n" at line start of a heading) markdown boundaries so chapters
// are not sliced mid-argument.
//
// Design invariants:
//   - order-preserving: segments cover the flattened line stream in order,
//     with no gaps and no overlaps;
//   - never silently drops content: a chunk that alone exceeds the budget
//     occupies its own (over-budget) segment rather than being truncated;
//   - H1 alignment: if a heading line starts within +/-10% of the budget
//     around the natural cut point, the cut moves to that heading line's
//     start, so the next segment opens with the chapter title (the chunk
//     containing the heading is split across the two segments; both then
//     reference the same ChunkIndex - Start may equal the previous
//     segment's End);
//   - chunk-boundary fallback: with no alignable H1 nearby, the cut widens
//     to the end of the chunk containing the natural cut (chunk stays
//     whole) and the fallback is logged.

// contentSegment is one map-unit of a multi-segment wiki extraction run.
type contentSegment struct {
	// Content is the segment text: the "\n"-joined lines of the chunks
	// (or chunk line ranges) assigned to this segment.
	Content string
	// StartChunkIndex / EndChunkIndex delimit the chunk range this segment
	// was built from. When an H1 alignment splits a chunk across two
	// segments, the next segment's StartChunkIndex equals the previous
	// segment's EndChunkIndex (the shared chunk is referenced by both).
	StartChunkIndex int
	EndChunkIndex   int
	// SegmentIndex is the 0-based position of this segment in the run.
	SegmentIndex int
	// TotalSegments is the total number of segments produced for the run.
	TotalSegments int
	// DocTitle is filled by the caller (document / book name) and used by
	// renderSegmentHeader to phrase the per-segment header. Empty in the
	// pure split result; see renderSegmentHeader.
	DocTitle string
}

// segLine is one physical line of the flattened chunk stream, remembering
// which chunk it came from so segment boundaries can be reported per chunk.
type segLine struct {
	chunkIdx int
	text     string // without trailing newline
}

// splitContentSegments partitions content into segments of at most maxChars
// runes each, accumulating along chunk boundaries (mirroring the batching
// style of splitChunksIntoCitationBatches). The chunks list is the source of
// truth for segmentation; when it is empty but content is not (defensive
// path), content is split line-wise with chunkIdx -1. Nil and empty
// non-text chunks are skipped, matching reconstructContent's text-only view.
//
// maxChars <= 0 falls back to a defensive default so a misconfigured setting
// can never produce an unbounded single-segment run.
func splitContentSegments(content string, chunks []*types.Chunk, maxChars int) []contentSegment {
	if maxChars <= 0 {
		maxChars = 200000
	}

	lines := flattenChunkLines(chunks)
	if len(lines) == 0 && content != "" {
		// No chunk list available - fall back to slicing the raw content.
		for _, ln := range strings.Split(content, "\n") {
			lines = append(lines, segLine{chunkIdx: -1, text: ln})
		}
	}
	if len(lines) == 0 {
		return nil
	}

	// Precompute per-line rune lengths and line-start rune offsets in the
	// joined text ("\n" between lines), so H1-window scans work in the
	// same unit the budget is expressed in.
	lens := make([]int, len(lines))
	starts := make([]int, len(lines)+1)
	for i, ln := range lines {
		lens[i] = utf8.RuneCountInString(ln.text)
		if i == 0 {
			starts[i] = 0
		} else {
			starts[i] = starts[i-1] + lens[i-1] + 1
		}
	}
	starts[len(lines)] = starts[len(lines)-1] + lens[len(lines)-1] + 1

	segs := make([]contentSegment, 0, 64)
	segStart := 0
	for segStart < len(lines) {
		// Natural cut: first position where the running total reaches the
		// budget (the line that crosses it stays in the leading segment).
		acc := 0
		cut := len(lines)
		overflowed := false
		for i := segStart; i < len(lines); i++ {
			acc += lens[i]
			if acc >= maxChars {
				cut = i + 1
				overflowed = true
				break
			}
		}
		if !overflowed {
			// Remainder fits the budget - emit the final segment.
			segs = append(segs, buildSegment(lines, segStart, len(lines), len(segs)))
			segStart = len(lines)
			break
		}

		// H1 alignment: look for a heading line starting within +/-10% of the
		// budget around the natural cut and move the cut onto it.
		window := maxChars / 10
		posCut := starts[cut]
		bestH, bestDist := -1, -1
		for h := segStart + 1; h < len(lines); h++ {
			pos := starts[h]
			if pos > posCut+window {
				break
			}
			if pos < posCut-window {
				continue
			}
			if !isH1Line(lines[h].text) {
				continue
			}
			d := pos - posCut
			if d < 0 {
				d = -d
			}
			if bestH == -1 || d < bestDist {
				bestH, bestDist = h, d
			}
		}

		if bestH >= 0 {
			// Heading opens the next segment; the chunk containing it is
			// intentionally split across the boundary.
			cut = bestH
		} else {
			// Fallback: widen the cut to the end of the chunk containing the
			// natural cut so the chunk is never sliced mid-way. This is the
			// only place a segment may exceed the budget (by at most the
			// remainder of that chunk), and it is logged for observability.
			ci := lines[cut-1].chunkIdx
			j := cut - 1
			for j < len(lines) && lines[j].chunkIdx == ci {
				j++
			}
			logger.Warnf(context.Background(),
				"[wiki_segment] no H1 heading within +/-%d chars of cut at line %d; "+
					"falling back to hard chunk boundary (chunk %d ends at line %d)",
				window, cut-1, ci, j-1)
			cut = j
		}

		segs = append(segs, buildSegment(lines, segStart, cut, len(segs)))
		segStart = cut
	}

	total := len(segs)
	for i := range segs {
		segs[i].TotalSegments = total
	}
	return segs
}

// buildSegment materialises the segment for lines [lo, hi). DocTitle is left
// empty - the caller fills it before rendering headers.
func buildSegment(lines []segLine, lo, hi, segIdx int) contentSegment {
	texts := make([]string, hi-lo)
	for i := lo; i < hi; i++ {
		texts[i-lo] = lines[i].text
	}
	return contentSegment{
		Content:         strings.Join(texts, "\n"),
		StartChunkIndex: lines[lo].chunkIdx,
		EndChunkIndex:   lines[hi-1].chunkIdx,
		SegmentIndex:    segIdx,
	}
}

// flattenChunkLines converts text chunks into the ordered line stream the
// segmenter walks. Non-text chunks are skipped (their payload is already
// merged into text content by reconstructEnrichedContent), as are nil and
// empty chunks. Chunk order follows ChunkIndex (then StartAt), preserving
// document order regardless of the caller's slice order.
func flattenChunkLines(chunks []*types.Chunk) []segLine {
	filtered := make([]*types.Chunk, 0, len(chunks))
	for _, c := range chunks {
		if c == nil || c.Content == "" {
			continue
		}
		if c.ChunkType != types.ChunkTypeText && c.ChunkType != "" {
			continue
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return nil
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].ChunkIndex == filtered[j].ChunkIndex {
			return filtered[i].StartAt < filtered[j].StartAt
		}
		return filtered[i].ChunkIndex < filtered[j].ChunkIndex
	})

	lines := make([]segLine, 0, len(filtered)*4)
	for _, c := range filtered {
		for _, ln := range strings.Split(c.Content, "\n") {
			lines = append(lines, segLine{chunkIdx: c.ChunkIndex, text: ln})
		}
	}
	return lines
}

// isH1Line reports whether the line opens a markdown H1 heading ("# " prefix,
// matching the alignment contract; deeper heading levels are left alone so we
// only cut at chapter boundaries).
func isH1Line(line string) bool {
	return strings.HasPrefix(line, "# ")
}

// renderSegmentHeader builds the per-segment preamble injected at the start
// of each segment's prompt content. It warns the model that entities may have
// been introduced in earlier segments and that earlier naming wins, which is
// what keeps slug identity stable across the map phase.
//
// docTitle (book / document name, supplied by the caller) personalises the
// subject; when empty the generic subject is used. N is 1-based.
func renderSegmentHeader(seg contentSegment, docTitle string) string {
	subject := "本文档"
	if t := strings.TrimSpace(docTitle); t != "" {
		subject = "《" + t + "》"
	}
	chars := utf8.RuneCountInString(seg.Content)
	return fmt.Sprintf(
		"<segment_header>%s较长，当前为第 %d/%d 部分（约 %d 字符）。实体或概念可能在前文段落中已出现，若与前文重复请沿用前文命名。</segment_header>",
		subject, seg.SegmentIndex+1, seg.TotalSegments, chars)
}

// estimateSegmentCharBudget converts a token budget into a per-segment
// character budget using the content's CJK density, so one configuration
// serves Chinese and English books without manual switching:
//
//	p       = CJK rune ratio of the first 8000 runes (sample)
//	density  = 0.65*p + 0.25*(1-p)   (tokens per char: ~0.65 CJK, ~0.25 ASCII)
//	budget   = clamp(tokenBudget / density, 50000, 800000)
//
// tokenBudget <= 0 returns 0, meaning "automatic mode disabled" - the caller
// then uses the manual wiki.segment_max_chars value as-is.
func estimateSegmentCharBudget(content string, tokenBudget int) int {
	if tokenBudget <= 0 {
		return 0
	}
	p := cjkRuneRatio(firstRunes(content, 8000))
	density := 0.65*p + 0.25*(1-p)
	if density <= 0 {
		density = 0.25 // defensive: p is always within [0,1], so unreachable
	}
	budget := int(float64(tokenBudget) / density)
	if budget < 50000 {
		return 50000
	}
	if budget > 800000 {
		return 800000
	}
	return budget
}

// cjkRuneRatio returns the fraction of runes in s that belong to CJK scripts
// (Han / Hiragana / Katakana / Hangul), using only the standard unicode tables.
func cjkRuneRatio(s string) float64 {
	runes := []rune(s)
	if len(runes) == 0 {
		return 0
	}
	cjk := 0
	for _, r := range runes {
		if unicode.Is(unicode.Han, r) ||
			unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) ||
			unicode.Is(unicode.Hangul, r) {
			cjk++
		}
	}
	return float64(cjk) / float64(len(runes))
}

// firstRunes returns at most the first n runes of s (rune-safe truncation for
// the density sample).
func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
