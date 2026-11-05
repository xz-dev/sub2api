package service

import (
	"runtime"
	"testing"
)

// The async queue is allocated at startup with maxContentModerationQueueSize
// slots. With by-value tasks that buffer was about 27 MB of live heap on every
// process, moderation on or off; with pointers it is 8 bytes per slot.
func TestContentModerationQueueBufferIsPointerSized(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	runtime.ReadMemStats(&after)
	if cap(svc.asyncQueue) != maxContentModerationQueueSize {
		t.Fatalf("queue capacity = %d, want %d", cap(svc.asyncQueue), maxContentModerationQueueSize)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	limit := uint64(maxContentModerationQueueSize)*8 + 1<<20
	if allocated > limit {
		t.Fatalf("NewContentModerationService allocated %d bytes, want at most %d", allocated, limit)
	}
	runtime.KeepAlive(svc)
}
