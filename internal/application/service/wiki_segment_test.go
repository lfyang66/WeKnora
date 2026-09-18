package service

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

// Test fixtures are self-contained: fixed-size filler lines assembled from
// ASCII and CJK runes, sized so the arithmetic around maxChars, the +/-10%
// H1 window and the density conversion is easy to verify by hand.

// fillLines builds a chunk body of n lines, each exactly lineLen copies of
// fill, joined with newlines.
func fillLines(fill string, n, lineLen int) string {
	line := strings.Repeat(fill, lineLen)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = line
	}
	return strings.Join(parts, "\n")
}

// mkSegChunk builds a text chunk with a stable index.
func mkSegChunk(idx int, content string) *types.Chunk {
	return &types.Chunk{
		ID:         fmt.Sprintf("seg-chunk-%03d", idx),
		ChunkIndex: idx,
		Content:    content,
		ChunkType:  types.ChunkTypeText,
	}
}

// joinedRoundTrip reconstructs the full document the same way the pipeline
// does (chunks joined with newlines) for zero-loss assertions.
func joinedRoundTrip(chunks []*types.Chunk) string {
	parts := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if c == nil || c.Content == "" {
			continue
		}
		if c.ChunkType != types.ChunkTypeText && c.ChunkType != "" {
			continue
		}
		parts = append(parts, c.Content)
	}
	return strings.Join(parts, "\n")
}

// --- splitContentSegments ---

func TestSplitContentSegmentsEmptyInput(t *testing.T) {
	if segs := splitContentSegments("", nil, 100000); len(segs) != 0 {
		t.Fatalf("empty input: want 0 segments, got %d", len(segs))
	}
	if segs := splitContentSegments("", []*types.Chunk{{ChunkIndex: 0, Content: ""}}, 100000); len(segs) != 0 {
		t.Fatalf("empty chunk content: want 0 segments, got %d", len(segs))
	}
}

func TestSplitContentSegmentsSingleOversizedChunkOwnsSegment(t *testing.T) {
	// 150k chars in ONE line, budget 100k, no H1 anywhere: the chunk cannot
	// be cut at a chunk boundary without losing content, so it must occupy
	// its own (over-budget) segment - cite-batch style, never truncated.
	body := strings.Repeat("a", 150000)
	segs := splitContentSegments("", []*types.Chunk{mkSegChunk(0, body)}, 100000)

	if len(segs) != 1 {
		t.Fatalf("want 1 segment for a single oversized chunk, got %d", len(segs))
	}
	seg := segs[0]
	if seg.StartChunkIndex != 0 || seg.EndChunkIndex != 0 {
		t.Fatalf("oversized chunk must own its segment, got start=%d end=%d", seg.StartChunkIndex, seg.EndChunkIndex)
	}
	if seg.Content != body {
		t.Fatalf("oversized chunk content must be preserved verbatim (got %d runes, want %d)",
			utf8.RuneCountInString(seg.Content), 150000)
	}
	if seg.SegmentIndex != 0 || seg.TotalSegments != 1 {
		t.Fatalf("single run indexing wrong: idx=%d total=%d", seg.SegmentIndex, seg.TotalSegments)
	}
}

func TestSplitContentSegmentsPreservesChunkOrder(t *testing.T) {
	// 10 chunks x 30k (10 lines x 3000). Budget 100k with no H1: cuts land
	// on chunk boundaries after the overflow chunk is swallowed whole.
	var chunks []*types.Chunk
	for i := 0; i < 10; i++ {
		chunks = append(chunks, mkSegChunk(i, fillLines("x", 10, 3000)))
	}
	segs := splitContentSegments("", chunks, 100000)

	if len(segs) != 3 {
		t.Fatalf("want 3 segments for 10x30k chunks at 100k budget, got %d", len(segs))
	}
	wantStarts := []int{0, 4, 8}
	wantEnds := []int{3, 7, 9}
	for i, seg := range segs {
		if seg.StartChunkIndex != wantStarts[i] || seg.EndChunkIndex != wantEnds[i] {
			t.Fatalf("segment %d: want chunk range [%d,%d], got [%d,%d]",
				i, wantStarts[i], wantEnds[i], seg.StartChunkIndex, seg.EndChunkIndex)
		}
		if seg.SegmentIndex != i {
			t.Fatalf("segment %d carries index %d", i, seg.SegmentIndex)
		}
		if seg.TotalSegments != 3 {
			t.Fatalf("segment %d: TotalSegments=%d, want 3", i, seg.TotalSegments)
		}
	}
	// Order preservation: strictly increasing starts, seamless coverage.
	for i := 1; i < len(segs); i++ {
		if segs[i].StartChunkIndex <= segs[i-1].StartChunkIndex {
			t.Fatalf("starts must strictly increase: seg%d start=%d, seg%d start=%d",
				i, segs[i].StartChunkIndex, i-1, segs[i-1].StartChunkIndex)
		}
		if segs[i].StartChunkIndex != segs[i-1].EndChunkIndex+1 {
			t.Fatalf("coverage gap: seg%d ends at chunk %d but seg%d starts at %d",
				i-1, segs[i-1].EndChunkIndex, i, segs[i].StartChunkIndex)
		}
	}
	if segs[0].StartChunkIndex != 0 || segs[len(segs)-1].EndChunkIndex != 9 {
		t.Fatalf("must cover the whole chunk list: first=%d last=%d",
			segs[0].StartChunkIndex, segs[len(segs)-1].EndChunkIndex)
	}
	// Zero loss.
	want := joinedRoundTrip(chunks)
	got := joinSegments(segs)
	if got != want {
		t.Fatalf("round-trip mismatch: got %d runes, want %d", utf8.RuneCountInString(got), utf8.RuneCountInString(want))
	}
}

// joinSegments stitches segments back with the same separator the splitter
// removed segments at (each cut replaces one newline).
func joinSegments(segs []contentSegment) string {
	parts := make([]string, len(segs))
	for i, seg := range segs {
		parts[i] = seg.Content
	}
	return strings.Join(parts, "\n")
}

func TestSplitContentSegmentsH1AlignmentSplitsAtHeading(t *testing.T) {
	// chunk0: 95 lines x 1000 = 95000. chunk1: a 12-char tail line, then an
	// H1 "# Chapter Two", then 5 x 1000. The natural cut for a 100k budget
	// lands ~50k... precisely: after chunk1 line 6 (acc=100025), whose
	// offset is ~5024 runes PAST the heading - well inside the +/-10k
	// window - so the cut must move onto the heading line.
	chunk0 := fillLines("x", 95, 1000)
	chunk1 := "tail padding" + "\n" + "# Chapter Two" + "\n" + fillLines("y", 5, 1000)
	chunks := []*types.Chunk{mkSegChunk(0, chunk0), mkSegChunk(1, chunk1)}
	segs := splitContentSegments("", chunks, 100000)

	if len(segs) != 2 {
		t.Fatalf("want 2 segments with H1 alignment, got %d", len(segs))
	}
	if !strings.HasPrefix(segs[1].Content, "# Chapter Two") {
		t.Fatalf("segment 2 must open with the H1 heading, got prefix %q", firstN(segs[1].Content, 40))
	}
	if strings.Contains(segs[0].Content, "# Chapter Two") {
		t.Fatalf("segment 1 must not contain the heading")
	}
	// The heading lived inside chunk 1: both segments reference it, so the
	// next segment's Start equals the previous segment's End.
	if segs[0].EndChunkIndex != 1 || segs[1].StartChunkIndex != 1 {
		t.Fatalf("split chunk must be shared: seg0 end=%d seg1 start=%d", segs[0].EndChunkIndex, segs[1].StartChunkIndex)
	}
	if segs[0].StartChunkIndex != 0 || segs[1].EndChunkIndex != 1 {
		t.Fatalf("coverage wrong: [%d..%d] [%d..%d]",
			segs[0].StartChunkIndex, segs[0].EndChunkIndex, segs[1].StartChunkIndex, segs[1].EndChunkIndex)
	}
	if got, want := joinSegments(segs), joinedRoundTrip(chunks); got != want {
		t.Fatalf("H1 split must not lose content")
	}
}

func TestSplitContentSegmentsNoH1HardCutKeepsChunkWhole(t *testing.T) {
	// 3 chunks x 60k (20 lines x 3000), budget 100k, no H1. The natural cut
	// lands inside chunk 1 (acc crosses 100k at its 14th line), so the
	// fallback must swallow the rest of chunk 1: segment 1 = chunks 0+1
	// (over budget but whole), segment 2 = chunk 2.
	var chunks []*types.Chunk
	for i := 0; i < 3; i++ {
		chunks = append(chunks, mkSegChunk(i, fillLines("z", 20, 3000)))
	}
	segs := splitContentSegments("", chunks, 100000)

	if len(segs) != 2 {
		t.Fatalf("want 2 segments, got %d", len(segs))
	}
	if segs[0].StartChunkIndex != 0 || segs[0].EndChunkIndex != 1 {
		t.Fatalf("fallback must end at chunk boundary: got [%d,%d]", segs[0].StartChunkIndex, segs[0].EndChunkIndex)
	}
	wantSeg1 := chunks[0].Content + "\n" + chunks[1].Content
	if segs[0].Content != wantSeg1 {
		t.Fatalf("chunk 1 must stay whole inside segment 1: got %d runes, want %d",
			utf8.RuneCountInString(segs[0].Content), utf8.RuneCountInString(wantSeg1))
	}
	if segs[1].StartChunkIndex != 2 || segs[1].EndChunkIndex != 2 {
		t.Fatalf("segment 2 must be chunk 2 alone: got [%d,%d]", segs[1].StartChunkIndex, segs[1].EndChunkIndex)
	}
	if got, want := joinSegments(segs), joinedRoundTrip(chunks); got != want {
		t.Fatalf("hard cut must not lose content")
	}
}

func TestSplitContentSegmentsSkipsNilEmptyAndNonTextChunks(t *testing.T) {
	chunks := []*types.Chunk{
		nil,
		{ChunkIndex: 0, ChunkType: types.ChunkTypeImageOCR, Content: "ocr payload"},
		{ChunkIndex: 1, Content: ""},
		mkSegChunk(2, fillLines("w", 3, 100)),
		mkSegChunk(3, fillLines("v", 3, 100)),
	}
	segs := splitContentSegments("", chunks, 100000)

	if len(segs) != 1 {
		t.Fatalf("want 1 segment, got %d", len(segs))
	}
	if strings.Contains(segs[0].Content, "ocr payload") || strings.Contains(segs[0].Content, "chunk-001") {
		t.Fatalf("non-text/empty chunks must be skipped")
	}
	if segs[0].StartChunkIndex != 2 || segs[0].EndChunkIndex != 3 {
		t.Fatalf("indices must reflect only text chunks: got [%d,%d]", segs[0].StartChunkIndex, segs[0].EndChunkIndex)
	}
}

func TestSplitContentSegmentsFallsBackToRawContentWithoutChunks(t *testing.T) {
	content := "alpha line" + "\n" + "beta line" + "\n" + "# Heading" + "\n" + "gamma line"
	segs := splitContentSegments(content, nil, 100000)

	if len(segs) != 1 {
		t.Fatalf("small content fits one segment, got %d", len(segs))
	}
	if segs[0].Content != content {
		t.Fatalf("raw-content fallback must preserve text verbatim")
	}
	if segs[0].StartChunkIndex != -1 || segs[0].EndChunkIndex != -1 {
		t.Fatalf("no-chunk path must use sentinel -1, got [%d,%d]",
			segs[0].StartChunkIndex, segs[0].EndChunkIndex)
	}
}

func TestSplitContentSegmentsDefensiveDefaultBudget(t *testing.T) {
	// maxChars<=0 must not produce an unbounded run. With the 200k default,
	// three 120k chunks accumulate past the budget during chunk 1, which is
	// then swallowed whole -> segments [0,1] and [2]. (A broken default of
	// e.g. 100k would yield three single-chunk segments instead.)
	var chunks []*types.Chunk
	for i := 0; i < 3; i++ {
		chunks = append(chunks, mkSegChunk(i, fillLines("q", 120, 1000)))
	}
	segs := splitContentSegments("", chunks, 0)

	if len(segs) != 2 {
		t.Fatalf("defensive default (200k) must merge chunks 0+1, got %d segments", len(segs))
	}
	if segs[0].EndChunkIndex != 1 || segs[1].StartChunkIndex != 2 {
		t.Fatalf("expected ranges [0,1] and [2,2], got [%d,%d] and [%d,%d]",
			segs[0].StartChunkIndex, segs[0].EndChunkIndex, segs[1].StartChunkIndex, segs[1].EndChunkIndex)
	}
}

// --- renderSegmentHeader ---

func TestRenderSegmentHeaderContainsCountAndTitle(t *testing.T) {
	seg := contentSegment{Content: strings.Repeat("x", 12345), SegmentIndex: 1, TotalSegments: 5}
	hdr := renderSegmentHeader(seg, "Enneagram Book")

	if !strings.HasPrefix(hdr, "<segment_header>") || !strings.HasSuffix(hdr, "</segment_header>") {
		t.Fatalf("header must be wrapped in segment_header tags: %q", hdr)
	}
	if !strings.Contains(hdr, "《Enneagram Book》") {
		t.Fatalf("header must carry the doc title, got %q", hdr)
	}
	if !strings.Contains(hdr, "第 2/5 部分") {
		t.Fatalf("header must carry 1-based N/M counts, got %q", hdr)
	}
	if !strings.Contains(hdr, "约 12345 字符") {
		t.Fatalf("header must carry the rune count, got %q", hdr)
	}
}

func TestRenderSegmentHeaderFallsBackToGenericSubject(t *testing.T) {
	seg := contentSegment{Content: "abc", SegmentIndex: 0, TotalSegments: 2}
	hdr := renderSegmentHeader(seg, "")
	if !strings.Contains(hdr, "本文档较长") {
		t.Fatalf("empty title must use the generic subject, got %q", hdr)
	}
	if strings.Contains(hdr, "《") {
		t.Fatalf("generic subject must not add book-title marks, got %q", hdr)
	}
	// Whitespace-only title degrades the same way.
	if hdr2 := renderSegmentHeader(seg, "   "); !strings.Contains(hdr2, "本文档较长") {
		t.Fatalf("whitespace title must use the generic subject, got %q", hdr2)
	}
}

// --- estimateSegmentCharBudget ---

func TestEstimateSegmentCharBudgetDisabled(t *testing.T) {
	if got := estimateSegmentCharBudget("任何内容", 0); got != 0 {
		t.Fatalf("tokenBudget=0 must disable auto mode, got %d", got)
	}
	if got := estimateSegmentCharBudget("任何内容", -5); got != 0 {
		t.Fatalf("negative tokenBudget must disable auto mode, got %d", got)
	}
}

func TestEstimateSegmentCharBudgetChineseDensity(t *testing.T) {
	content := strings.Repeat("字", 8000)
	got := estimateSegmentCharBudget(content, 130000)
	// density 0.65 -> 130000/0.65 = 200000 (float tolerance for the 0.65 literal)
	if got < 199000 || got > 201000 {
		t.Fatalf("chinese sample: want ~200000, got %d", got)
	}
}

func TestEstimateSegmentCharBudgetEnglishDensity(t *testing.T) {
	content := strings.Repeat("a", 8000)
	got := estimateSegmentCharBudget(content, 130000)
	// density 0.25 -> 130000/0.25 = 520000 exactly
	if got != 520000 {
		t.Fatalf("english sample: want 520000, got %d", got)
	}
}

func TestEstimateSegmentCharBudgetMixedSitsBetweenExtremes(t *testing.T) {
	content := strings.Repeat("字", 4000) + strings.Repeat("b", 4000)
	got := estimateSegmentCharBudget(content, 130000)
	if got <= 201000 || got >= 520000 {
		t.Fatalf("mixed sample must sit strictly between the CJK and ASCII results, got %d", got)
	}
}

func TestEstimateSegmentCharBudgetClampsToBounds(t *testing.T) {
	// Huge budget clamps to the 800k ceiling...
	if got := estimateSegmentCharBudget(strings.Repeat("a", 8000), 1000000000); got != 800000 {
		t.Fatalf("huge budget must clamp to 800000, got %d", got)
	}
	// ...and a budget that would land under the floor clamps up to 50000
	// (30000 tokens / 0.25 = 120000 is fine, but /0.65 = ~46153 is not).
	if got := estimateSegmentCharBudget(strings.Repeat("字", 8000), 30000); got != 50000 {
		t.Fatalf("under-floor budget must clamp up to 50000, got %d", got)
	}
}

func TestEstimateSegmentCharBudgetSamplesOnlyHead(t *testing.T) {
	// 8001 ASCII runes then one CJK rune: the 8000-rune sample window ends
	// before the CJK rune (rune index 8001), so the sampled ratio must stay
	// 0 and yield the pure-English density.
	content := strings.Repeat("c", 8001) + "字"
	got := estimateSegmentCharBudget(content, 130000)
	if got != 520000 {
		t.Fatalf("sample must cover only the first 8000 runes: want 520000, got %d", got)
	}
}

// firstN is a test helper for readable failure messages.
func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
