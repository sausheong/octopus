package router

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sausheong/harness/llm"
)

func TestClassifierMemoCachesSuccessfulProfileWithoutUsage(t *testing.T) {
	memo := newClassifierMemo(2, time.Minute)
	want := TaskProfile{Difficulty: "low", Domain: "qa"}
	wantUsage := &llm.Usage{InputTokens: 10, OutputTokens: 2}
	var calls atomic.Int32
	load := func(context.Context) (TaskProfile, *llm.Usage, bool) {
		calls.Add(1)
		return want, wantUsage, true
	}

	got, usage, cached, coalesced := memo.do(context.Background(), "digest", load)
	if got != want || usage != wantUsage || cached || coalesced {
		t.Fatalf("first result = (%+v, %+v, %v)", got, usage, cached)
	}
	got, usage, cached, coalesced = memo.do(context.Background(), "digest", load)
	if got != want || usage != nil || !cached {
		t.Fatalf("cached result = (%+v, %+v, %v)", got, usage, cached)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	if stats := memo.snapshot(); stats.Hits != 1 || stats.Misses != 1 || stats.Stores != 1 || stats.Entries != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestClassifierMemoDoesNotCacheFailures(t *testing.T) {
	memo := newClassifierMemo(2, time.Minute)
	var calls atomic.Int32
	load := func(context.Context) (TaskProfile, *llm.Usage, bool) {
		calls.Add(1)
		return DefaultProfile(), nil, false
	}

	_, _, _, _ = memo.do(context.Background(), "digest", load)
	_, _, _, _ = memo.do(context.Background(), "digest", load)
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", calls.Load())
	}
	if stats := memo.snapshot(); stats.Stores != 0 || stats.Entries != 0 {
		t.Fatalf("failed classifications were cached: %+v", stats)
	}
}

func TestClassifierMemoExpiresAndEvictsLRU(t *testing.T) {
	memo := newClassifierMemo(2, time.Minute)
	now := time.Unix(1_000, 0)
	memo.now = func() time.Time { return now }
	var calls atomic.Int32
	load := func(context.Context) (TaskProfile, *llm.Usage, bool) {
		return TaskProfile{EstTokensIn: int(calls.Add(1))}, nil, true
	}

	_, _, _, _ = memo.do(context.Background(), "a", load)
	_, _, _, _ = memo.do(context.Background(), "b", load)
	_, _, _, _ = memo.do(context.Background(), "a", load) // a becomes most recent
	_, _, _, _ = memo.do(context.Background(), "c", load) // b is evicted
	_, _, cached, _ := memo.do(context.Background(), "b", load)
	if cached {
		t.Fatal("least-recently-used entry was retained")
	}

	now = now.Add(2 * time.Minute)
	_, _, cached, _ = memo.do(context.Background(), "c", load)
	if cached {
		t.Fatal("expired entry was retained")
	}
	if stats := memo.snapshot(); stats.Evictions == 0 {
		t.Fatalf("expected an eviction, stats = %+v", stats)
	}
}

func TestClassifierMemoCoalescesConcurrentLoads(t *testing.T) {
	memo := newClassifierMemo(2, time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	load := func(context.Context) (TaskProfile, *llm.Usage, bool) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return TaskProfile{Difficulty: "high"}, nil, true
	}

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	type result struct {
		profile   TaskProfile
		coalesced bool
	}
	results := make(chan result, workers)
	for range workers {
		go func() {
			defer wg.Done()
			profile, _, _, coalesced := memo.do(context.Background(), "same-digest", load)
			results <- result{profile: profile, coalesced: coalesced}
		}()
	}
	<-started
	deadline := time.Now().Add(time.Second)
	for memo.snapshot().Coalesced != workers-1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(results)

	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	coalescedResults := 0
	for result := range results {
		if result.profile.Difficulty != "high" {
			t.Fatalf("shared profile = %+v", result.profile)
		}
		if result.coalesced {
			coalescedResults++
		}
	}
	if coalescedResults != workers-1 {
		t.Fatalf("coalesced result provenance = %d, want %d", coalescedResults, workers-1)
	}
	if stats := memo.snapshot(); stats.Coalesced != workers-1 {
		t.Fatalf("coalesced = %d, want %d", stats.Coalesced, workers-1)
	}
}

func TestClassifierMemoKeyIsDigestAndCoversLogicalInput(t *testing.T) {
	turn := llm.Message{Role: "user", Content: "private prompt text"}
	base := classifierMemoKey("provider/model", 256, turn)
	if len(base) != 32 || strings.Contains(base, turn.Content) {
		t.Fatalf("key is not an opaque SHA-256 digest: %q", base)
	}
	variants := []string{
		classifierMemoKey("provider/other", 256, turn),
		classifierMemoKey("provider/model", 512, turn),
		classifierMemoKey("provider/model", 256, llm.Message{Role: "user", Content: "other"}),
		classifierMemoKey("provider/model", 256, llm.Message{Role: "assistant", Content: turn.Content}),
	}
	for _, variant := range variants {
		if variant == base {
			t.Fatal("different logical classifier input produced the same key")
		}
	}
}
