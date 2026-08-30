// SPDX-License-Identifier: AGPL-3.0-or-later

package baremetal

import (
	"strings"
	"sync"
	"testing"
)

func TestLogBuffer_WriteAndString(t *testing.T) {
	b := newLogBuffer()
	n, err := b.Write([]byte("hello "))
	if err != nil || n != 6 {
		t.Fatalf("Write() = (%d, %v), want (6, nil)", n, err)
	}
	if _, err := b.Write([]byte("world")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if got := b.String(); got != "hello world" {
		t.Errorf("String() = %q, want %q", got, "hello world")
	}
}

func TestLogBuffer_TrimsToCapacity(t *testing.T) {
	b := newLogBuffer()
	// Write well past logBufferCapacity - the buffer must keep only the
	// most recent bytes, not grow unbounded.
	chunk := strings.Repeat("a", 1024)
	for i := 0; i < (logBufferCapacity/1024)+4; i++ {
		if _, err := b.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write() error: %v", err)
		}
	}
	// A final, distinctive marker - must survive the trim since it's the
	// most recent write.
	if _, err := b.Write([]byte("MARKER")); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	got := b.String()
	if len(got) > logBufferCapacity {
		t.Errorf("len(String()) = %d, want <= %d", len(got), logBufferCapacity)
	}
	if !strings.HasSuffix(got, "MARKER") {
		t.Error("String() does not end with the most recent write - trim kept the wrong end")
	}
}

func TestLogBuffer_ConcurrentWrites(t *testing.T) {
	b := newLogBuffer()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = b.Write([]byte("x"))
			}
		}()
	}
	wg.Wait()
	// Only checking this doesn't race/panic (run under `go test -race`) -
	// the exact final content isn't deterministic across goroutines.
	_ = b.String()
}
