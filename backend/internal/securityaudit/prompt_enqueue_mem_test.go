package securityaudit

import (
	"context"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
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
