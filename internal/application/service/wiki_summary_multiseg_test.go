package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// summaryTestNewline is the newline inside fake LLM payloads; a raw string
// literal keeps it free of escape-sequence plumbing.
const summaryTestNewline = `
`

// Prompt fingerprints used to route fake LLM responses: the reduce prompt
// carries the <partial_summaries> block, the legacy map prompt carries the
// <document> block.
const (
	summaryReduceFingerprint = "<partial_summaries>"
	summaryMapFingerprint    = "<document>"
	summarySegmentRejectErr  = "segment rejected by upstream model"
)

// summaryFakeChat answers WikiSummaryPrompt calls per segment marker and
// WikiSummaryReducePrompt calls with a fixed combined page, counting each.
type summaryFakeChat struct {
	mu           sync.Mutex
	prompts      []string
	summaryMap   map[string]string // segment marker -> summary payload
	summaryErr   map[string]bool   // segment marker -> force Chat error
	reduceOut    string
	summaryCalls int
	reduceCalls  int
}

func (m *summaryFakeChat) Chat(_ context.Context, messages []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prompt := ""
	if len(messages) > 0 {
		prompt = messages[0].Content
	}
	m.prompts = append(m.prompts, prompt)
	if strings.Contains(prompt, summaryReduceFingerprint) {
		m.reduceCalls++
		out := m.reduceOut
		if out == "" {
			out = "SUMMARY: combined summary line" + summaryTestNewline + "Combined body."
		}
		return &types.ChatResponse{Content: out}, nil
	}
	if strings.Contains(prompt, summaryMapFingerprint) {
		m.summaryCalls++
		for marker, text := range m.summaryMap {
			if strings.Contains(prompt, marker) {
				if m.summaryErr[marker] {
					return nil, errors.New(summarySegmentRejectErr)
				}
				return &types.ChatResponse{Content: text}, nil
			}
		}
		return &types.ChatResponse{Content: "SUMMARY: default part" + summaryTestNewline + "Default body."}, nil
	}
	return nil, errors.New("summaryFakeChat: unexpected prompt")
}

func (m *summaryFakeChat) ChatStream(context.Context, []chat.Message, *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, nil
}

func (m *summaryFakeChat) GetModelName() string { return "summary-fake" }
func (m *summaryFakeChat) GetModelID() string   { return "summary-fake" }

// reducePrompt returns the single captured reduce-phase prompt.
func (m *summaryFakeChat) reducePrompt(t *testing.T) string {
	t.Helper()
	for _, p := range m.prompts {
		if strings.Contains(p, summaryReduceFingerprint) {
			return p
		}
	}
	t.Fatalf("no reduce-phase prompt captured among %d prompts", len(m.prompts))
	return ""
}

func TestGenerateSummaryMultiSegmentSingleSegmentUsesLegacyPath(t *testing.T) {
	fake := &summaryFakeChat{summaryMap: map[string]string{
		"SOLO": "SUMMARY: solo line" + summaryTestNewline + "Solo body.",
	}}
	svc := &wikiIngestService{}

	got, err := svc.generateSummaryMultiSegment(
		context.Background(), fake, multisegSegments("SOLO content"),
		"- [[entity/x]] = X"+summaryTestNewline, "Chinese", &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("generateSummaryMultiSegment() error = %v", err)
	}
	if fake.summaryCalls != 1 {
		t.Fatalf("single segment should make exactly one summary call, got %d", fake.summaryCalls)
	}
	if fake.reduceCalls != 0 {
		t.Fatalf("single segment must not trigger reduce, got %d reduce calls", fake.reduceCalls)
	}
	if !strings.Contains(got, "solo line") {
		t.Fatalf("legacy path output should pass through unchanged: %q", got)
	}
	// Single-segment runs reproduce the legacy input: no segment header.
	if strings.Contains(fake.prompts[0], "segment_header") {
		t.Fatalf("single-segment path must not inject a segment header: %q", fake.prompts[0])
	}
	if !strings.Contains(fake.prompts[0], "SOLO content") {
		t.Fatalf("single-segment prompt should carry the segment content: %q", fake.prompts[0])
	}
}

func TestGenerateSummaryMultiSegmentMapsAndReduces(t *testing.T) {
	fake := &summaryFakeChat{summaryMap: map[string]string{
		"PART-ONE": "SUMMARY: part one" + summaryTestNewline + "PART-ONE-SUMMARY-TEXT",
		"PART-TWO": "SUMMARY: part two" + summaryTestNewline + "PART-TWO-SUMMARY-TEXT",
	}}
	svc := &wikiIngestService{}

	got, err := svc.generateSummaryMultiSegment(
		context.Background(), fake, multisegSegments("PART-ONE content", "PART-TWO content"),
		"slugs", "Chinese", &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("generateSummaryMultiSegment() error = %v", err)
	}
	if fake.summaryCalls != 2 {
		t.Fatalf("map phase should summarize both segments, got %d calls", fake.summaryCalls)
	}
	if fake.reduceCalls != 1 {
		t.Fatalf("reduce phase should run exactly once, got %d calls", fake.reduceCalls)
	}

	reduce := fake.reducePrompt(t)
	if !strings.Contains(reduce, "PART-ONE-SUMMARY-TEXT") || !strings.Contains(reduce, "PART-TWO-SUMMARY-TEXT") {
		t.Fatalf("reduce must receive every successful partial: %q", reduce)
	}
	// Map prompts carry the injected segment header.
	if !strings.Contains(fake.prompts[0], "第 1/2 部分") && !strings.Contains(fake.prompts[1], "第 1/2 部分") {
		t.Fatalf("map prompts should carry part labels: %q / %q", fake.prompts[0], fake.prompts[1])
	}
	// Output stays splitSummaryLine-compatible.
	sumLine, body := splitSummaryLine(got)
	if sumLine != "combined summary line" {
		t.Fatalf("splitSummaryLine mismatch: summary=%q body=%q", sumLine, body)
	}
	if body != "Combined body." {
		t.Fatalf("splitSummaryLine body mismatch: %q", body)
	}
}

func TestGenerateSummaryMultiSegmentSurvivesPartialFailure(t *testing.T) {
	fake := &summaryFakeChat{
		summaryMap: map[string]string{
			"PART-ONE": "SUMMARY: p1" + summaryTestNewline + "P1-TEXT",
			"PART-TWO": "SUMMARY: p2" + summaryTestNewline + "P2-TEXT",
			"PART-BAD": "SUMMARY: bad" + summaryTestNewline + "BAD-TEXT",
		},
		summaryErr: map[string]bool{"PART-BAD": true},
	}
	svc := &wikiIngestService{}

	got, err := svc.generateSummaryMultiSegment(
		context.Background(), fake,
		multisegSegments("PART-ONE content", "PART-BAD content", "PART-TWO content"),
		"slugs", "Chinese", &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("a single failed segment must not fail the run: %v", err)
	}
	if fake.summaryCalls != 3 {
		t.Fatalf("all segments should be attempted, got %d calls", fake.summaryCalls)
	}
	if fake.reduceCalls != 1 {
		t.Fatalf("reduce should still run after partial failure, got %d calls", fake.reduceCalls)
	}
	reduce := fake.reducePrompt(t)
	if !strings.Contains(reduce, "P1-TEXT") || !strings.Contains(reduce, "P2-TEXT") {
		t.Fatalf("successful partials should reach reduce: %q", reduce)
	}
	if strings.Contains(reduce, "BAD-TEXT") {
		t.Fatalf("failed segment must not contribute a partial: %q", reduce)
	}
	if !strings.Contains(got, "combined summary line") {
		t.Fatalf("unexpected final summary: %q", got)
	}
}

func TestGenerateSummaryMultiSegmentAllSegmentsFailed(t *testing.T) {
	fake := &summaryFakeChat{
		summaryMap: map[string]string{
			"PART-ONE": "SUMMARY: p1" + summaryTestNewline + "P1-TEXT",
			"PART-TWO": "SUMMARY: p2" + summaryTestNewline + "P2-TEXT",
		},
		summaryErr: map[string]bool{"PART-ONE": true, "PART-TWO": true},
	}
	svc := &wikiIngestService{}

	_, err := svc.generateSummaryMultiSegment(
		context.Background(), fake, multisegSegments("PART-ONE content", "PART-TWO content"),
		"slugs", "Chinese", &WikiBatchContext{},
	)
	if err == nil {
		t.Fatalf("all segments failing must surface an error")
	}
	if fake.reduceCalls != 0 {
		t.Fatalf("reduce must not run when no partial survives, got %d calls", fake.reduceCalls)
	}
}

func TestGenerateSummaryMultiSegmentReducePromptCarriesSegmentLabels(t *testing.T) {
	fake := &summaryFakeChat{summaryMap: map[string]string{
		"PART-ONE": "SUMMARY: p1" + summaryTestNewline + "P1-TEXT",
		"PART-TWO": "SUMMARY: p2" + summaryTestNewline + "P2-TEXT",
	}}
	svc := &wikiIngestService{}

	if _, err := svc.generateSummaryMultiSegment(
		context.Background(), fake, multisegSegments("PART-ONE content", "PART-TWO content"),
		"slugs", "Chinese", &WikiBatchContext{},
	); err != nil {
		t.Fatalf("generateSummaryMultiSegment() error = %v", err)
	}
	reduce := fake.reducePrompt(t)
	if !strings.Contains(reduce, `part="1/2"`) || !strings.Contains(reduce, `part="2/2"`) {
		t.Fatalf("reduce prompt should label every partial with its part number: %q", reduce)
	}
	if !strings.Contains(reduce, `chars="0-15"`) {
		t.Fatalf("reduce prompt should carry per-part char ranges (PART-ONE content = 16 runes): %q", reduce)
	}
	if !strings.Contains(reduce, `chars="16-31"`) {
		t.Fatalf("second segment range should start right after the first: %q", reduce)
	}
}
