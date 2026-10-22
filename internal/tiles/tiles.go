// Package tiles turns a STAC catalog of optimized GeoParquet files into MVT.
package tiles

import (
	"context"
	"fmt"
	"sync"

	"github.com/parquet-go/parquet-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/clip"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/encoding/wkb"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"

	"github.com/adonm/geoparquet-tileserver/internal/catalog"
)

type Options struct {
	Name           string
	Collection     string
	GeometryColumn string
	MinZoom        int
	MaxZoom        int
	Preload        bool
}

type Row struct {
	Geometry []byte  `parquet:"geometry"`
	BBox     BBox    `parquet:"bbox"`
	Count    int64   `parquet:"count"`
	SumValue float64 `parquet:"sum_value"`
}

type BBox struct {
	Xmin float64 `parquet:"xmin"`
	Ymin float64 `parquet:"ymin"`
	Xmax float64 `parquet:"xmax"`
	Ymax float64 `parquet:"ymax"`
}

type Feature struct {
	Geom  orb.Geometry
	Bound orb.Bound
	Count int64
	Sum   float64
}

type Partition struct {
	Asset    string
	Bound    orb.Bound
	mu       sync.Mutex
	loaded   bool
	features []Feature
}

func (p *Partition) load() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return nil
	}
	rows, err := parquet.ReadFile[Row](p.Asset)
	if err != nil {
		return fmt.Errorf("read %s: %w", p.Asset, err)
	}
	fs := make([]Feature, 0, len(rows))
	for _, r := range rows {
		g, err := wkb.Unmarshal(r.Geometry)
		if err != nil {
			continue
		}
		b := orb.Bound{Min: orb.Point{r.BBox.Xmin, r.BBox.Ymin}, Max: orb.Point{r.BBox.Xmax, r.BBox.Ymax}}
		if b.IsZero() {
			b = g.Bound()
		}
		fs = append(fs, Feature{Geom: g, Bound: b, Count: r.Count, Sum: r.SumValue})
	}
	p.features = fs
	p.loaded = true
	return nil
}

type Layer struct {
	Name    string
	MinZoom int
	MaxZoom int
	Bounds  orb.Bound

	overview   *Overview
	partitions []*Partition
}

func New(opts Options, items []catalog.Item) (*Layer, error) {
	l := &Layer{Name: opts.Name, MinZoom: opts.MinZoom, MaxZoom: opts.MaxZoom}
	for _, it := range items {
		asset, ok := it.ParquetAsset()
		if !ok {
			continue
		}
		if ov, err := openOverview(asset); err == nil && ov != nil {
			l.overview = ov
			if opts.Preload {
				for i := range ov.levels {
					if _, err := ov.BoundsForLevel(i); err != nil {
						return nil, fmt.Errorf("preload %s: %w", opts.Name, err)
					}
				}
			}
			break
		}
		l.partitions = append(l.partitions, &Partition{Asset: asset, Bound: it.Bound()})
	}
	if l.overview == nil && len(l.partitions) == 0 {
		return nil, fmt.Errorf("layer %q: no GeoParquet assets", opts.Name)
	}
	l.Bounds = itemsBounds(items)
	return l, nil
}

func (l *Layer) Render(ctx context.Context, z uint8, x, y uint32, minCount *int) ([]byte, error) {
	tile := maptile.New(x, y, maptile.Zoom(z))
	tb := tile.Bound()
	fc := &geojson.FeatureCollection{}
	var min int64
	filter := false
	if minCount != nil {
		min = int64(*minCount)
		filter = true
	}

	if l.overview != nil {
		level := l.overview.LevelForZoom(int(z))
		bounds, err := l.overview.BoundsForLevel(level)
		if err != nil {
			return nil, err
		}
		for _, rgb := range bounds {
			if !intersects(rgb.Bound, tb) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			err := l.overview.ScanRowGroup(rgb.Index, func(r Row) {
				if filter && r.Count < min {
					return
				}
				b := orb.Bound{Min: orb.Point{r.BBox.Xmin, r.BBox.Ymin}, Max: orb.Point{r.BBox.Xmax, r.BBox.Ymax}}
				if !intersects(b, tb) {
					return
				}
				g, err := wkb.Unmarshal(r.Geometry)
				if err != nil {
					return
				}
				g = clip.Geometry(tb, g)
				if g == nil {
					return
				}
				fc.Features = append(fc.Features, &geojson.Feature{
					Geometry:   g,
					Properties: geojson.Properties{"count": r.Count, "sum_value": r.SumValue},
				})
			})
			if err != nil {
				return nil, err
			}
		}
	} else {
		for _, p := range l.partitions {
			if !intersects(p.Bound, tb) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := p.load(); err != nil {
				return nil, err
			}
			for _, f := range p.features {
				if filter && f.Count < min {
					continue
				}
				if !intersects(f.Bound, tb) {
					continue
				}
				g := clip.Geometry(tb, orb.Clone(f.Geom))
				if g == nil {
					continue
				}
				fc.Features = append(fc.Features, &geojson.Feature{
					Geometry:   g,
					Properties: geojson.Properties{"count": f.Count, "sum_value": f.Sum},
				})
			}
		}
	}

	ls := mvt.NewLayers(map[string]*geojson.FeatureCollection{l.Name: fc})
	ls.ProjectToTile(tile)
	ls.Clip(mvt.MapboxGLDefaultExtentBound)
	return mvt.Marshal(ls)
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
