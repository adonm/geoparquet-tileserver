package tilecache

import (
	"fmt"
	"testing"
)

func TestByteBoundAndLRU(t *testing.T) {
	c := New(10)
	c.Put("a", []byte("1234"))
	c.Put("b", []byte("5678"))
	if b, ok := c.Get("a"); !ok || string(b) != "1234" {
		t.Fatalf("get a = %q, %v", b, ok)
	}
	// "a" is now most recently used, so adding "c" evicts "b".
	c.Put("c", []byte("901"))
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if b, ok := c.Get("a"); !ok || string(b) != "1234" {
		t.Fatalf("get a after eviction = %q, %v", b, ok)
	}
	if b, ok := c.Get("c"); !ok || string(b) != "901" {
		t.Fatalf("get c = %q, %v", b, ok)
	}
	if got := c.Stats(); got.Bytes != 7 || got.Evictions != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestUpdateExistingKey(t *testing.T) {
	c := New(10)
	c.Put("a", []byte("1234"))
	c.Put("a", []byte("12"))
	if got := c.Stats(); got.Bytes != 2 || got.Entries != 1 {
		t.Fatalf("stats = %+v", got)
	}
	if b, ok := c.Get("a"); !ok || string(b) != "12" {
		t.Fatalf("get a = %q, %v", b, ok)
	}
}

func TestOversizedEntryIsNotCached(t *testing.T) {
	c := New(4)
	c.Put("a", []byte("12345"))
	if _, ok := c.Get("a"); ok {
		t.Fatal("oversized entry should not be cached")
	}
	if got := c.Stats(); got.Bytes != 0 || got.Entries != 0 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestZeroBudget(t *testing.T) {
	c := New(0)
	c.Put("a", []byte("x"))
	if _, ok := c.Get("a"); ok {
		t.Fatal("zero-budget cache should not store entries")
	}
}

func BenchmarkPutGet(b *testing.B) {
	c := New(64 << 20)
	value := make([]byte, 4096)
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("tile/%d", i%1024)
		c.Put(key, value)
		c.Get(key)
	}
}
