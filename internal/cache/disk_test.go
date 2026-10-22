package cache

import "testing"

func TestDisk(t *testing.T) {
	d, err := Open(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	d.Put("ranges", "https://example.com/a.parquet|0", []byte("hello"))
	if b, ok := d.Get("ranges", "https://example.com/a.parquet|0"); !ok || string(b) != "hello" {
		t.Fatalf("get = %q, %v", b, ok)
	}
}
