package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// Prompt fingerprints used to route fake LLM responses. They match stable
// lines of the corresponding templates in internal/agent/prompts_wiki.go.
const (
	multisegExtractFingerprint = "knowledge extraction system"
	multisegDedupFingerprint   = "strict deduplication system"
	// multisegSegmentRejectErr must stay free of transient-retry markers
	// ("timeout", "connection refused", ...) so generateWithTemplate fails
	// fast instead of backing off.
	multisegSegmentRejectErr = "segment rejected by upstream model"
)

// multisegJSON marshals the combined extraction payload the fake returns
// for one segment.
func multisegJSON(entities, concepts []extractedItem) string {
	data, _ := json.Marshal(struct {
		Entities []extractedItem `json:"entities"`
		Concepts []extractedItem `json:"concepts"`
	}{Entities: entities, Concepts: concepts})
	return string(data)
}

// multisegFakeChat routes prompts by template fingerprint: candidate-slug
// extraction calls are answered per segment marker found in the prompt,
// deduplication calls are counted and answered with an empty merge table.
type multisegFakeChat struct {
	mu           sync.Mutex
	prompts      []string
	extractCalls int
	dedupCalls   int
	segmentJSON  map[string]string // segment marker -> extraction JSON
	segmentErr   map[string]bool   // segment marker -> force Chat error
}

func (m *multisegFakeChat) Chat(_ context.Context, messages []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prompt := ""
	if len(messages) > 0 {
		prompt = messages[0].Content
	}
	m.prompts = append(m.prompts, prompt)
	if strings.Contains(prompt, multisegDedupFingerprint) {
		m.dedupCalls++
		return &types.ChatResponse{Content: `{"merges": {}}`}, nil
	}
	if strings.Contains(prompt, multisegExtractFingerprint) {
		m.extractCalls++
		for marker, payload := range m.segmentJSON {
			if strings.Contains(prompt, marker) {
				if m.segmentErr[marker] {
					return nil, errors.New(multisegSegmentRejectErr)
				}
				return &types.ChatResponse{Content: payload}, nil
			}
		}
		return &types.ChatResponse{Content: `{"entities": [], "concepts": []}`}, nil
	}
	return nil, errors.New("multisegFakeChat: unexpected prompt")
}

func (m *multisegFakeChat) ChatStream(context.Context, []chat.Message, *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, nil
}

func (m *multisegFakeChat) GetModelName() string { return "multiseg-fake" }
func (m *multisegFakeChat) GetModelID() string   { return "multiseg-fake" }

// multisegStubWiki makes deduplicateExtractedBatch take the LLM-backed path
// (FindSimilarPages always surfaces one candidate page) while counting
// probes; FindPagesByNormalizedTitles stays empty so no exact targets form.
type multisegStubWiki struct {
	interfaces.WikiPageService
	mu    sync.Mutex
	calls int
}

func (s *multisegStubWiki) FindSimilarPages(context.Context, string, string, []string, int) ([]*types.WikiPageLite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return []*types.WikiPageLite{{Slug: "entity/existing-page", Title: "Existing Page"}}, nil
}

func (s *multisegStubWiki) FindPagesByNormalizedTitles(context.Context, string, string, []string) ([]*types.WikiPageLite, error) {
	return nil, nil
}

// multisegSegments builds ordered segments with per-segment marker text.
func multisegSegments(specs ...string) []contentSegment {
	segs := make([]contentSegment, len(specs))
	for i, text := range specs {
		segs[i] = contentSegment{
			Content:       text,
			SegmentIndex:  i,
			TotalSegments: len(specs),
			DocTitle:      "测试之书",
		}
	}
	return segs
}

func multisegService(fake *multisegFakeChat) (*wikiIngestService, *multisegStubWiki) {
	stub := &multisegStubWiki{}
	return &wikiIngestService{wikiService: stub}, stub
}

func TestExtractCandidateSlugsMultiSegmentMergesSameSlugAcrossSegments(t *testing.T) {
	fake := &multisegFakeChat{segmentJSON: map[string]string{
		"SEG-ONE": multisegJSON([]extractedItem{{
			Name: "明斯基", Slug: "entity/minsky",
			Aliases:      []string{"Minsky"},
			Description:  "short",
			Details:      "d1",
			SourceChunks: []string{"c1"},
		}}, nil),
		"SEG-TWO": multisegJSON([]extractedItem{{
			Name: "明斯基", Slug: "entity/minsky",
			Aliases:      []string{"Marvin Minsky", "Minsky"},
			Description:  "a much longer description wins",
			Details:      "d2-longer-details",
			SourceChunks: []string{"c2", "c1"},
		}}, nil),
	}}
	svc, _ := multisegService(fake)

	entities, concepts, slugItems, err := svc.extractCandidateSlugsMultiSegment(
		context.Background(), fake, "kb-1",
		multisegSegments("SEG-ONE text", "SEG-TWO text"), "Chinese", nil, &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("extractCandidateSlugsMultiSegment() error = %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("same slug across segments should merge into 1 entity, got %d: %#v", len(entities), entities)
	}
	if len(concepts) != 0 {
		t.Fatalf("expected no concepts, got %d", len(concepts))
	}
	got := entities[0]
	if got.Name != "明斯基" || got.Slug != "entity/minsky" {
		t.Fatalf("merged identity changed: %#v", got)
	}
	wantAliases := []string{"Minsky", "Marvin Minsky"}
	if !reflect.DeepEqual(got.Aliases, wantAliases) {
		t.Fatalf("alias union = %#v, want %#v", got.Aliases, wantAliases)
	}
	if got.Description != "a much longer description wins" {
		t.Fatalf("longer description should win, got %q", got.Description)
	}
	if got.Details != "d2-longer-details" {
		t.Fatalf("longer details should win, got %q", got.Details)
	}
	wantChunks := []string{"c1", "c2"}
	if !reflect.DeepEqual(got.SourceChunks, wantChunks) {
		t.Fatalf("source chunks union = %#v, want %#v", got.SourceChunks, wantChunks)
	}
	if len(slugItems) != 1 {
		t.Fatalf("slugItems should carry exactly the merged candidate, got %d", len(slugItems))
	}
}

func TestExtractCandidateSlugsMultiSegmentKeepsDistinctCandidates(t *testing.T) {
	fake := &multisegFakeChat{segmentJSON: map[string]string{
		"SEG-ONE": multisegJSON([]extractedItem{{Name: "阿波罗", Slug: "entity/apollo"}}, nil),
		"SEG-TWO": multisegJSON(nil, []extractedItem{{Name: "螺旋算法", Slug: "concept/spiral"}}),
	}}
	svc, _ := multisegService(fake)

	entities, concepts, _, err := svc.extractCandidateSlugsMultiSegment(
		context.Background(), fake, "kb-1",
		multisegSegments("SEG-ONE text", "SEG-TWO text"), "Chinese", nil, &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("extractCandidateSlugsMultiSegment() error = %v", err)
	}
	if len(entities) != 1 || entities[0].Slug != "entity/apollo" {
		t.Fatalf("entity from segment one should survive: %#v", entities)
	}
	if len(concepts) != 1 || concepts[0].Slug != "concept/spiral" {
		t.Fatalf("concept from segment two should survive: %#v", concepts)
	}
}

func TestExtractCandidateSlugsMultiSegmentKeepsFirstTypeForCrossListSlug(t *testing.T) {
	fake := &multisegFakeChat{segmentJSON: map[string]string{
		"SEG-ONE": multisegJSON([]extractedItem{{Name: "幽灵节点", Slug: "entity/ghost"}}, nil),
		// Same slug resurfaces on the concept side in a later segment.
		"SEG-TWO": multisegJSON(nil, []extractedItem{{Name: "幽灵节点", Slug: "entity/ghost"}}),
	}}
	svc, _ := multisegService(fake)

	entities, concepts, _, err := svc.extractCandidateSlugsMultiSegment(
		context.Background(), fake, "kb-1",
		multisegSegments("SEG-ONE text", "SEG-TWO text"), "Chinese", nil, &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("extractCandidateSlugsMultiSegment() error = %v", err)
	}
	if len(entities) != 1 || entities[0].Slug != "entity/ghost" {
		t.Fatalf("first type assignment (entity) should survive: %#v", entities)
	}
	if len(concepts) != 0 {
		t.Fatalf("duplicate cross-list slug should be dropped, got %d concepts", len(concepts))
	}
}

func TestExtractCandidateSlugsMultiSegmentSurvivesPartialSegmentFailure(t *testing.T) {
	fake := &multisegFakeChat{
		segmentJSON: map[string]string{
			"SEG-ONE": multisegJSON([]extractedItem{{Name: "甲", Slug: "entity/jia"}}, nil),
			"SEG-TWO": multisegJSON([]extractedItem{{Name: "乙", Slug: "entity/yi"}}, nil),
			"SEG-BAD": multisegJSON([]extractedItem{{Name: "坏", Slug: "entity/bad"}}, nil),
		},
		segmentErr: map[string]bool{"SEG-BAD": true},
	}
	svc, _ := multisegService(fake)

	entities, _, _, err := svc.extractCandidateSlugsMultiSegment(
		context.Background(), fake, "kb-1",
		multisegSegments("SEG-ONE text", "SEG-BAD text", "SEG-TWO text"), "Chinese", nil, &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("one failing segment must not abort peers: %v", err)
	}
	got := make(map[string]bool, len(entities))
	for _, item := range entities {
		got[item.Slug] = true
	}
	if !got["entity/jia"] || !got["entity/yi"] || got["entity/bad"] {
		t.Fatalf("peers should survive the failed segment: %#v", entities)
	}
}

func TestExtractCandidateSlugsMultiSegmentAllSegmentsFailed(t *testing.T) {
	fake := &multisegFakeChat{
		segmentJSON: map[string]string{
			"SEG-ONE": multisegJSON([]extractedItem{{Name: "甲", Slug: "entity/jia"}}, nil),
			"SEG-TWO": multisegJSON([]extractedItem{{Name: "乙", Slug: "entity/yi"}}, nil),
		},
		segmentErr: map[string]bool{"SEG-ONE": true, "SEG-TWO": true},
	}
	svc, _ := multisegService(fake)

	_, _, _, err := svc.extractCandidateSlugsMultiSegment(
		context.Background(), fake, "kb-1",
		multisegSegments("SEG-ONE text", "SEG-TWO text"), "Chinese", nil, &WikiBatchContext{},
	)
	if err == nil {
		t.Fatalf("all segments failing must surface an error")
	}
}

func TestExtractCandidateSlugsMultiSegmentTruncatesCandidateExplosion(t *testing.T) {
	const perSeg = 300 // 2 x 300 = 600 > cap 500
	build := func(prefix string) string {
		items := make([]extractedItem, 0, perSeg)
		for i := 0; i < perSeg; i++ {
			items = append(items, extractedItem{
				Name:         fmt.Sprintf("%s%03d", prefix, i),
				Slug:         fmt.Sprintf("entity/%s-%03d", prefix, i),
				SourceChunks: []string{"c000"},
			})
		}
		return multisegJSON(items, nil)
	}
	fake := &multisegFakeChat{segmentJSON: map[string]string{
		"SEG-ONE": build("aa"),
		"SEG-TWO": build("bb"),
	}}
	svc, _ := multisegService(fake)

	entities, concepts, slugItems, err := svc.extractCandidateSlugsMultiSegment(
		context.Background(), fake, "kb-1",
		multisegSegments("SEG-ONE text", "SEG-TWO text"), "Chinese", nil, &WikiBatchContext{},
	)
	if err != nil {
		t.Fatalf("truncation must not fail the document: %v", err)
	}
	if len(entities)+len(concepts) != maxPass0MergedCandidates {
		t.Fatalf("merged pool should truncate to %d, got %d", maxPass0MergedCandidates, len(entities)+len(concepts))
	}
	if len(slugItems) != maxPass0MergedCandidates {
		t.Fatalf("slugItems should mirror the truncated pool, got %d", len(slugItems))
	}
	// Equal evidence weight + equal name length -> stable first-seen order.
	if entities[0].Slug != "entity/aa-000" {
		t.Fatalf("first-seen candidate should survive truncation, got %s", entities[0].Slug)
	}
}

func TestExtractCandidateSlugsMultiSegmentRunsFinalDedupAfterMerge(t *testing.T) {
	fake := &multisegFakeChat{segmentJSON: map[string]string{
		"SEG-ONE":   multisegJSON([]extractedItem{{Name: "甲", Slug: "entity/jia"}}, nil),
		"SEG-TWO":   multisegJSON([]extractedItem{{Name: "乙", Slug: "entity/yi"}}, nil),
		"SEG-THREE": multisegJSON([]extractedItem{{Name: "丙", Slug: "entity/bing"}}, nil),
	}}
	svc, stub := multisegService(fake)

	if _, _, _, err := svc.extractCandidateSlugsMultiSegment(
		context.Background(), fake, "kb-1",
		multisegSegments("SEG-ONE text", "SEG-TWO text", "SEG-THREE text"), "Chinese", nil, &WikiBatchContext{},
	); err != nil {
		t.Fatalf("extractCandidateSlugsMultiSegment() error = %v", err)
	}
	// 3 per-segment in-extraction dedups + 1 merged-list safety net.
	if fake.dedupCalls != 4 {
		t.Fatalf("expected 3 per-segment + 1 merged-list dedup LLM calls, got %d", fake.dedupCalls)
	}
	if stub.calls == 0 {
		t.Fatalf("merged-list dedup should probe existing pages via FindSimilarPages")
	}
}

func TestExtractCandidateSlugsMultiSegmentSlugItemsMatchListsAndAreDeterministic(t *testing.T) {
	segmentJSON := map[string]string{
		"SEG-ONE": multisegJSON([]extractedItem{
			{Name: "甲", Slug: "entity/jia"},
			{Name: "乙", Slug: "entity/yi"},
		}, []extractedItem{{Name: "主题", Slug: "concept/theme"}}),
		"SEG-TWO": multisegJSON([]extractedItem{
			{Name: "乙", Slug: "entity/yi"},
			{Name: "丙", Slug: "entity/bing"},
		}, nil),
		"SEG-THREE": multisegJSON(nil, []extractedItem{{Name: "方法", Slug: "concept/method"}}),
	}

	var runs [][]string
	for run := 0; run < 3; run++ {
		fake := &multisegFakeChat{segmentJSON: segmentJSON}
		svc, _ := multisegService(fake)
		entities, concepts, slugItems, err := svc.extractCandidateSlugsMultiSegment(
			context.Background(), fake, "kb-1",
			multisegSegments("SEG-ONE text", "SEG-TWO text", "SEG-THREE text"), "Chinese", nil, &WikiBatchContext{},
		)
		if err != nil {
			t.Fatalf("run %d: error = %v", run, err)
		}
		if len(slugItems) != len(entities)+len(concepts) {
			t.Fatalf("run %d: slugItems = %d, entities+concepts = %d", run, len(slugItems), len(entities)+len(concepts))
		}
		for _, item := range entities {
			got, ok := slugItems[item.Slug]
			if !ok || got.Name != item.Name {
				t.Fatalf("run %d: slugItems missing entity %s", run, item.Slug)
			}
		}
		for _, item := range concepts {
			got, ok := slugItems[item.Slug]
			if !ok || got.Name != item.Name {
				t.Fatalf("run %d: slugItems missing concept %s", run, item.Slug)
			}
		}
		var seq []string
		for _, item := range entities {
			seq = append(seq, item.Slug)
		}
		for _, item := range concepts {
			seq = append(seq, item.Slug)
		}
		runs = append(runs, seq)
	}
	for i := 1; i < len(runs); i++ {
		if !reflect.DeepEqual(runs[0], runs[i]) {
			t.Fatalf("concurrent merging is nondeterministic: run0=%v run%d=%v", runs[0], i, runs[i])
		}
	}
}
