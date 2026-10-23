// Package tiles turns a STAC catalog of optimized GeoParquet files into MVT.
package tiles

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/paulmach/orb"
	"github.com/paulmach/orb/clip"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/encoding/wkb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	"github.com/paulmach/orb/simplify"

	"github.com/adonm/geoparquet-tileserver/internal/catalog"
	filt "github.com/adonm/geoparquet-tileserver/internal/filter"
)

type Options struct {
	Name           string
	Collection     string
	GeometryColumn string
	MinZoom        int
	MaxZoom        int
	Preload        bool
	ExcludeProps   []string
	IDColumn       string
	Simplify       float64
}

type Layer struct {
	Name    string
	MinZoom int
	MaxZoom int
	Bounds  orb.Bound

	geomCol    string
	partitions []*partitionRef
	exclude    map[string]bool
	idColumn   string
	simplify   float64

	fieldsOnce sync.Once
	fields     []Field

	indexMu     sync.RWMutex
	indexBounds map[int]map[int][]rgBound
}

type partitionRef struct {
	index   int
	asset   string
	geomCol string
	bound   orb.Bound
	once    sync.Once
	ds      *Dataset
	err     error
}

func (p *partitionRef) get() (*Dataset, error) {
	p.once.Do(func() {
		p.ds, p.err = openDataset(p.asset, p.geomCol)
	})
	return p.ds, p.err
}

func New(opts Options, items []catalog.Item) (*Layer, error) {
	l := &Layer{
		Name: opts.Name, MinZoom: opts.MinZoom, MaxZoom: opts.MaxZoom,
		geomCol: opts.GeometryColumn, idColumn: opts.IDColumn, simplify: opts.Simplify,
	}
	if len(opts.ExcludeProps) > 0 {
		l.exclude = make(map[string]bool, len(opts.ExcludeProps))
		for _, name := range opts.ExcludeProps {
			l.exclude[name] = true
		}
	}
	for _, it := range items {
		asset, ok := it.ParquetAsset()
		if !ok {
			continue
		}
		pr := &partitionRef{index: len(l.partitions), asset: asset, geomCol: opts.GeometryColumn, bound: it.Bound()}
		l.partitions = append(l.partitions, pr)
		if opts.Preload {
			ds, err := pr.get()
			if err != nil {
				return nil, fmt.Errorf("preload %s: %w", opts.Name, err)
			}
			for i := range ds.levels {
				if _, err := ds.BoundsForLevel(i); err != nil {
					return nil, fmt.Errorf("preload %s: %w", opts.Name, err)
				}
			}
		}
	}
	if len(l.partitions) == 0 {
		return nil, fmt.Errorf("layer %q: no GeoParquet assets", opts.Name)
	}
	l.Bounds = itemsBounds(items)
	return l, nil
}

type RenderStats struct {
	OpenMS   float64
	BoundsMS float64
	ScanMS   float64
	EncodeMS float64
	Features int
}

func (s RenderStats) QueryMS() float64 { return s.OpenMS + s.BoundsMS + s.ScanMS }

func (l *Layer) boundsFor(p *partitionRef, ds *Dataset, level int) ([]rgBound, error) {
	l.indexMu.RLock()
	levels, ok := l.indexBounds[p.index]
	var b []rgBound
	if ok {
		b, ok = levels[level]
	}
	l.indexMu.RUnlock()
	if ok {
		return b, nil
	}
	return ds.BoundsForLevel(level)
}

func (l *Layer) Render(ctx context.Context, z uint8, x, y uint32, minCount *int, f *filt.Filter) ([]byte, error) {
	b, _, err := l.RenderDetailed(ctx, z, x, y, minCount, f)
	return b, err
}

func (l *Layer) RenderDetailed(ctx context.Context, z uint8, x, y uint32, minCount *int, f *filt.Filter) ([]byte, RenderStats, error) {
	var stats RenderStats
	tile := maptile.New(x, y, maptile.Zoom(z))
	tb := tile.Bound()
	fc := &geojson.FeatureCollection{}
	var min int64
	filterMin := false
	if minCount != nil {
		min = int64(*minCount)
		filterMin = true
	}

	add := func(geom []byte, bound orb.Bound, hasBBox bool, props map[string]any) error {
		if filterMin {
			if c, ok := propInt(props, "count"); ok && c < min {
				return nil
			}
		}
		if f != nil {
			ok, err := f.Match(props)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
		if len(l.exclude) > 0 {
			for name := range l.exclude {
				delete(props, name)
			}
		}
		g, err := wkb.Unmarshal(geom)
		if err != nil {
			return nil
		}
		if !hasBBox {
			bound = g.Bound()
		}
		if !intersects(bound, tb) {
			return nil
		}
		g = clip.Geometry(tb, g)
		if g == nil {
			return nil
		}
		fc.Features = append(fc.Features, &geojson.Feature{
			Geometry:   g,
			Properties: geojson.Properties(props),
		})
		return nil
	}

	for _, p := range l.partitions {
		if !intersects(p.bound, tb) {
			continue
		}
		t0 := time.Now()
		ds, err := p.get()
		stats.OpenMS += time.Since(t0).Seconds() * 1000
		if err != nil {
			return nil, stats, err
		}
		level := 0
		if ds.overview {
			level = ds.LevelForZoom(int(z))
		}
		t0 = time.Now()
		bounds, err := l.boundsFor(p, ds, level)
		stats.BoundsMS += time.Since(t0).Seconds() * 1000
		if err != nil {
			return nil, stats, err
		}
		for _, rgb := range bounds {
			if !intersects(rgb.Bound, tb) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, stats, err
			}
			t0 = time.Now()
			var addErr error
			if err := ds.ScanRowGroup(rgb.Index, func(geom []byte, bound orb.Bound, hasBBox bool, props map[string]any) {
				if addErr == nil {
					addErr = add(geom, bound, hasBBox, props)
				}
			}); err != nil {
				return nil, stats, err
			}
			stats.ScanMS += time.Since(t0).Seconds() * 1000
			if addErr != nil {
				return nil, stats, addErr
			}
		}
	}
	stats.Features = len(fc.Features)

	t0 := time.Now()
	ls := mvt.NewLayers(map[string]*geojson.FeatureCollection{l.Name: fc})
	ls.ProjectToTile(tile)
	if l.simplify > 0 {
		ls.Simplify(simplify.DouglasPeucker(l.simplify))
	}
	ls.Clip(mvt.MapboxGLDefaultExtentBound)
	b, err := mvt.Marshal(ls)
	stats.EncodeMS = time.Since(t0).Seconds() * 1000
	if err != nil {
		return nil, stats, err
	}
	return b, stats, nil
}

func propInt(props map[string]any, name string) (int64, bool) {
	v, ok := props[name]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	}
	return 0, false
}

func intersects(a, b orb.Bound) bool {
	return !(a.Max.X() < b.Min.X() || a.Min.X() > b.Max.X() || a.Max.Y() < b.Min.Y() || a.Min.Y() > b.Max.Y())
}

func itemsBounds(items []catalog.Item) orb.Bound {
	minX, minY, maxX, maxY := 180.0, 90.0, -180.0, -90.0
	for _, it := range items {
		ib := it.Bound()
		if ib.Min.X() < minX {
			minX = ib.Min.X()
		}
		if ib.Min.Y() < minY {
			minY = ib.Min.Y()
		}
		if ib.Max.X() > maxX {
			maxX = ib.Max.X()
		}
		if ib.Max.Y() > maxY {
			maxY = ib.Max.Y()
		}
	}
	if minX > maxX {
		return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	}
	return orb.Bound{Min: orb.Point{minX, minY}, Max: orb.Point{maxX, maxY}}
}
