package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

// Tests for planSegmentation, the pure decision function behind
// mapOneDocument's single-shot vs multi-segment branch. They pin down the
// contract: small documents stay on the legacy path untouched, over-budget
// documents are partitioned losslessly, the token-budget mode derives the
// effective cap from content density, and chunk/content disagreement has a
// well-defined behaviour (chunks are the segmentation source of truth).

// segTestChunk builds a single-line text chunk (the shape
// flattenChunkLines consumes).
func segTestChunk(idx int, content string) *types.Chunk {
	return &types.Chunk{ChunkIndex: idx, Content: content, ChunkType: types.ChunkTypeText}
}

// joinTestSegments rebuilds the flattened line stream from segments (segment
// boundaries are always line boundaries, so joining with a newline restores
// the original stream exactly).
func joinTestSegments(segs []contentSegment) string {
	var sb strings.Builder
	for i, seg := range segs {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(seg.Content)
	}
	return sb.String()
}

func TestPlanSegmentationSmallDocStaysSingle(t *testing.T) {
	content := strings.Repeat("小", 100)
	segs, multi, effectiveMax, mode := planSegmentation(100, content, nil, 200000, 0)
	if multi {
		t.Fatalf("small doc must stay on the single-shot path, got multi=true")
	}
	if segs != nil {
		t.Fatalf("single-shot path must not produce segments, got %d", len(segs))
	}
	if effectiveMax != 200000 {
		t.Fatalf("manual mode must keep the configured cap, got %d", effectiveMax)
	}
	if mode != "manual" {
		t.Fatalf("tokenBudget=0 must report mode=manual, got %q", mode)
	}
}

func TestPlanSegmentationOverLimitSplitsByChunks(t *testing.T) {
	var chunks []*types.Chunk
	var parts []string
	for i := 0; i < 4; i++ {
		c := "第" + string(rune('A'+i)) + "章" + strings.Repeat("字", 58) // 60 runes, no H1
		chunks = append(chunks, segTestChunk(i, c))
		parts = append(parts, c)
	}
	want := strings.Join(parts, "\n") // 243 runes
	segs, multi, effectiveMax, mode := planSegmentation(utf8.RuneCountInString(want), want, chunks, 100, 0)
	if !multi {
		t.Fatalf("243 runes > cap 100 must take the multi-segment path")
	}
	if effectiveMax != 100 || mode != "manual" {
		t.Fatalf("manual cap/mode mismatch: effectiveMax=%d mode=%q", effectiveMax, mode)
	}
	if len(segs) < 2 {
		t.Fatalf("over-budget doc must split into >= 2 segments, got %d", len(segs))
	}
	if got := joinTestSegments(segs); got != want {
		t.Fatalf("segmentation must be lossless: rebuilt %d runes vs original %d",
			utf8.RuneCountInString(got), utf8.RuneCountInString(want))
	}
	for i, seg := range segs {
		if seg.SegmentIndex != i || seg.TotalSegments != len(segs) {
			t.Fatalf("segment %d index/total mismatch: idx=%d total=%d", i, seg.SegmentIndex, seg.TotalSegments)
		}
	}
	if segs[0].StartChunkIndex != 0 || segs[len(segs)-1].EndChunkIndex != 3 {
		t.Fatalf("chunk coverage broken: first=%d last=%d", segs[0].StartChunkIndex, segs[len(segs)-1].EndChunkIndex)
	}
}

func TestPlanSegmentationAutoTokenMode(t *testing.T) {
	zh := strings.Repeat("中", 3000) // CJK ratio p=1 -> density 0.65
	segs, multi, effectiveMax, mode := planSegmentation(3000, zh, nil, 200000, 130000)
	if mode != "auto_token" {
		t.Fatalf("tokenBudget>0 must report auto_token, got %q", mode)
	}
	if effectiveMax != estimateSegmentCharBudget(zh, 130000) {
		t.Fatalf("auto cap must equal the density estimate: %d vs %d", effectiveMax, estimateSegmentCharBudget(zh, 130000))
	}
	if effectiveMax != 200000 { // 130000 / 0.65 = 200000 exactly
		t.Fatalf("Chinese density 0.65 with budget 130000 must cap at 200000, got %d", effectiveMax)
	}
	if multi || segs != nil {
		t.Fatalf("3000 runes within the 200000 cap must stay single-shot")
	}

	en := strings.Repeat("a", 3000) // ASCII ratio p=0 -> density 0.25
	_, _, enMax, _ := planSegmentation(3000, en, nil, 200000, 130000)
	if enMax != 520000 { // 130000 / 0.25 = 520000 exactly
		t.Fatalf("English density 0.25 with budget 130000 must cap at 520000, got %d", enMax)
	}
	if enMax <= effectiveMax {
		t.Fatalf("English books must get a larger cap than Chinese ones: %d vs %d", enMax, effectiveMax)
	}

	// Clamp bounds: tiny budgets floor at 50000, huge ones ceiling at 800000.
	_, _, lo, _ := planSegmentation(0, zh, nil, 200000, 30000)
	if lo != 50000 {
		t.Fatalf("budget 30000/0.65 must clamp up to 50000, got %d", lo)
	}
	_, _, hi, _ := planSegmentation(0, zh, nil, 200000, 600000)
	if hi != 800000 {
		t.Fatalf("budget 600000/0.65 must clamp down to 800000, got %d", hi)
	}
}

func TestPlanSegmentationExactBoundary(t *testing.T) {
	content := strings.Repeat("界", 500)
	_, multi, _, _ := planSegmentation(500, content, nil, 500, 0)
	if multi {
		t.Fatalf("raw == maxChars must stay single-shot (boundary inclusive)")
	}
	content501 := strings.Repeat("界", 501)
	segs, multi2, _, _ := planSegmentation(501, content501, nil, 500, 0)
	if !multi2 {
		t.Fatalf("raw == maxChars+1 must take the multi-segment path")
	}
	if len(segs) == 0 {
		t.Fatalf("multi path must produce at least one segment")
	}
}

func TestPlanSegmentationTokenBudgetDisabled(t *testing.T) {
	segs, multi, effectiveMax, mode := planSegmentation(10, "短", nil, 123456, 0)
	if mode != "manual" || effectiveMax != 123456 || multi || segs != nil {
		t.Fatalf("tokenBudget=0: manual cap must pass through untouched (mode=%q cap=%d multi=%v)", mode, effectiveMax, multi)
	}
	_, _, effNeg, modeNeg := planSegmentation(10, "短", nil, 123456, -7)
	if modeNeg != "manual" || effNeg != 123456 {
		t.Fatalf("negative tokenBudget must behave as disabled (mode=%q cap=%d)", modeNeg, effNeg)
	}
	// Misconfigured manual cap (0 slipping past registry validation) must
	// fall back to the defensive default so multi=false is impossible for a
	// real document and the reported cap matches the segmenter's own
	// internal fallback.
	_, _, effZero, modeZero := planSegmentation(10, "短", nil, 0, 0)
	if modeZero != "manual" || effZero != 200000 {
		t.Fatalf("maxChars=0 must fall back to the 200000 defensive default, got mode=%q cap=%d", modeZero, effZero)
	}
}

func TestPlanSegmentationChunkContentMismatch(t *testing.T) {
	// Chunks are the segmentation source of truth: when content and chunks
	// disagree (stale reconstruction, enriched markup differences), segment
	// text must come from the chunks and must never include content-only
	// text.
	var chunks []*types.Chunk
	var parts []string
	for i := 0; i < 3; i++ {
		c := strings.Repeat("块", 80)
		chunks = append(chunks, segTestChunk(i, c))
		parts = append(parts, c)
	}
	want := strings.Join(parts, "\n")         // 242 runes vs an unrelated content below
	unrelated := strings.Repeat("无关正文", 1000) // 4000 runes, absent from chunks
	segs, multi, _, _ := planSegmentation(4000, unrelated, chunks, 100, 0)
	if !multi {
		t.Fatalf("over-budget chunk stream must take the multi path")
	}
	got := joinTestSegments(segs)
	if got != want {
		t.Fatalf("chunks must drive segmentation: got %d runes, want chunk stream %d runes",
			utf8.RuneCountInString(got), utf8.RuneCountInString(want))
	}
	if strings.Contains(got, "无关") {
		t.Fatalf("content-only text must never leak into segments")
	}

	// Empty chunk list: line-level fallback over content (chunkIdx sentinel
	// -1). mapOneDocument returns early on len(chunks)==0, so this path is
	// defensive only; the contract here is "no content lost".
	fbSegs, fbMulti, _, _ := planSegmentation(4000, unrelated, nil, 100, 0)
	if !fbMulti || len(fbSegs) == 0 {
		t.Fatalf("fallback path must flag multi and emit at least one segment")
	}
	if joined := joinTestSegments(fbSegs); joined != unrelated {
		t.Fatalf("fallback must not lose content: rebuilt %d vs %d runes",
			utf8.RuneCountInString(joined), 4000)
	}
}
