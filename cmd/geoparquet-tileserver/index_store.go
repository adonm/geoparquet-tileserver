package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/adonm/geoparquet-tileserver/internal/artifact"
	"github.com/adonm/geoparquet-tileserver/internal/tiles"
)

func sLoadLayerIndex(store *artifact.Store, key string) (*tiles.Index, bool) {
	if store == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, ok, err := store.Get(ctx, key)
	if err != nil || !ok {
		return nil, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		return nil, false
	}
	var idx tiles.Index
	if json.Unmarshal(data, &idx) != nil || idx.SchemaVersion != tiles.IndexSchemaVersion {
		return nil, false
	}
	return &idx, true
}

func sSaveLayerIndex(store *artifact.Store, key string, idx *tiles.Index) {
	if store == nil || idx == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := json.Marshal(idx)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return
	}
	if err := zw.Close(); err != nil {
		return
	}
	_ = store.Put(ctx, key, "application/gzip", buf.Bytes())
}
