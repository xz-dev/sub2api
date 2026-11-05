package securityaudit

import (
	"context"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
)

// blockingJobRepository holds the background commit until release closes.
type blockingJobRepository struct {
	fakeJobRepository
	entered, release chan struct{}
}

func (r *blockingJobRepository) CreateStagingWithCapacity(ctx context.Context, snapshot PromptSnapshot, v int64, a, b int) (*Job, error) {
	close(r.entered)
	<-r.release
	return r.fakeJobRepository.CreateStagingWithCapacity(ctx, snapshot, v, a, b)
}

func liveHeap() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	runtime.GC()
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// settledLiveHeap is the live heap after the pools' victim caches are gone:
// a sync.Pool keeps what it was given (json's encoder buffer, the loggers')
// for one more GC, and under -race it drops items at random.
func settledLiveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	return liveHeap()
}

// largeTranscriptBody is a Responses body of about size bytes: many tool
// calls and outputs, like the Codex transcripts the large-request work uses.
func largeTranscriptBody(size int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"start"}]}`)
	out := strings.Repeat("output line of a tool ", 200)
	for i := 0; b.Len() < size; i++ {
		id := strconv.Itoa(i)
		b.WriteString(`,{"type":"function_call","call_id":"c` + id + `","name":"shell","arguments":"{\"cmd\":\"ls ` + id + `\"}"}`)
		b.WriteString(`,{"type":"function_call_output","call_id":"c` + id + `","output":"` + out + `"}`)
	}
	b.WriteString(`,{"type":"message","role":"user","content":[{"type":"input_text","text":"latest question"}]}]}`)
	return []byte(b.String())
}

// TestPromptServiceAsyncKeepsNoBodyAfterEnqueue: in async mode the snapshot
// is built on the request's goroutine and only it reaches the background
// goroutine. After Enqueue returns, the body is neither copied nor decoded
// (before, the goroutine cloned it and decoded it, about 3.3 × its size,
// after the handler had released its memory charge). While the goroutine is
// still running, the body is collectable and the heap it keeps (the
// snapshot) is a small fraction of the body.
func TestPromptServiceAsyncKeepsNoBodyAfterEnqueue(t *testing.T) {
	repo := &blockingJobRepository{entered: make(chan struct{}), release: make(chan struct{})}
	repo.createJob = &Job{ID: 7}
	cfg := &fakeConfigStore{cfg: asyncConfig(), active: true}
	svc := &PromptService{config: cfg, enqueuer: NewEnqueuer(cfg, repo, &fakePayloadStore{values: map[int64]string{}}),
		enqueueSlots: make(chan struct{}, 1), background: context.Background()}

	body := largeTranscriptBody(8 << 20)
	n := len(body)
	weakBody := weak.Make(&body[0])
	base := liveHeap() - uint64(n)
	var before, after runtime.MemStats
	require.NoError(t, svc.Enqueue(context.Background(), Request{RequestID: "r", Protocol: "openai_responses", Body: body}))
	runtime.ReadMemStats(&before)
	body = nil
	select {
	case <-repo.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the background commit did not start")
	}
	runtime.ReadMemStats(&after)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(n/16), "the body was copied or decoded after Enqueue returned")
	kept := int64(liveHeap()) - int64(base)
	require.Nil(t, weakBody.Value(), "the background goroutine still references the body")
	require.Less(t, kept, int64(n/16), "the background goroutine keeps %d bytes of an %d-byte body", kept, n)
	close(repo.release)
	svc.enqueueWG.Wait()
	require.NotEmpty(t, repo.createdSnapshot.PromptHash)
}

// TestPromptServiceAsyncHoldsChargeUntilCommitDone (review a4a7d843 major):
// the snapshot handed to the commit goroutine holds all the text the guard
// reads. Its share of the request's charge is held from Enqueue until the
// goroutine has dropped the job, and it covers the heap the job keeps.
//
// Review rr3 major: the heap the job keeps is measured from a settled heap
// taken before the body exists (the body is built without json's pooled
// encoder) to a settled heap while the commit waits, with the body
// dropped; the text the body was built from stays live in both. Before,
// the base included the encoder's pooled buffer, so "kept" was negative
// without -race and the test failed under it.
func TestPromptServiceAsyncHoldsChargeUntilCommitDone(t *testing.T) {
	repo := &blockingJobRepository{entered: make(chan struct{}), release: make(chan struct{})}
	repo.createJob = &Job{ID: 7}
	cfg := &fakeConfigStore{cfg: asyncConfig(), active: true}
	svc := &PromptService{config: cfg, enqueuer: NewEnqueuer(cfg, repo, &fakePayloadStore{values: map[int64]string{}}),
		enqueueSlots: make(chan struct{}, 1), background: context.Background()}

	text := strings.Repeat("pasted text the guard reads ", (4<<20)/28)
	for _, segments := range []int{1, 2} {
		parts := [][2]string{{"user", text}, {"assistant", text}}[:segments]
		base := settledLiveHeap()
		// One segment: the scan text is the arena's only segment; two: joined.
		body := rawResponsesBody(parts...)
		var held atomic.Int64
		var released atomic.Bool
		retain := func(n int64) func() {
			held.Store(n)
			return func() { released.Store(true) }
		}
		require.NoError(t, svc.Enqueue(context.Background(), Request{RequestID: "r", Protocol: "openai_responses", Body: body, RetainMemory: retain}))
		body = nil
		<-repo.entered
		kept := int64(settledLiveHeap()) - int64(base)
		t.Logf("%d segments: the job keeps %d B, the hold is %d B (%.3f)", segments, kept, held.Load(), float64(held.Load())/float64(kept))
		require.False(t, released.Load(), "released while the commit still holds the snapshot")
		require.Greater(t, kept, int64(len(text))*int64(segments), "the job keeps the scan text")
		require.GreaterOrEqual(t, held.Load(), kept, "the hold covers the heap the job keeps")
		repo.release <- struct{}{}
		svc.enqueueWG.Wait()
		require.True(t, released.Load(), "released once the commit is done")
		repo.entered = make(chan struct{})
		runtime.KeepAlive(text)
	}
}
