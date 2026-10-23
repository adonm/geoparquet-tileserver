package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMaxFeatures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("catalog: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxFeatures != 100000 {
		t.Fatalf("default max_features = %d", cfg.MaxFeatures)
	}
	if err := os.WriteFile(path, []byte("catalog: test\nmax_features: 100001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected max_features > 100000 to fail")
	}
}
