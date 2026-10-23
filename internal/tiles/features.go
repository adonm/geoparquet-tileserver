package tiles

import (
	"context"
	"fmt"
	"strings"

	"github.com/parquet-go/parquet-go"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/clip"
	"github.com/paulmach/orb/encoding/wkb"

	filt "github.com/adonm/geoparquet-tileserver/internal/filter"
)

// Field describes one queryable property of a layer.
type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Format   string `json:"format,omitempty"`
	Nullable bool   `json:"nullable,omitempty"`
	Geometry bool   `json:"geometry,omitempty"`
}

// Feature is a full-resolution feature read from the layer's GeoParquet.
type Feature struct {
	ID         string
	Geometry   orb.Geometry
	BBox       []float64
	Properties map[string]any
}

// Fields returns the union of the layer's Parquet schemas. The result is
// cached because schema discovery can open remote datasets.
func (l *Layer) Fields() []Field {
	l.fieldsOnce.Do(func() {
		seen := map[string]Field{}
		for _, p := range l.partitions {
			ds, err := p.get()
			if err != nil {
				continue
			}
			for _, f := range ds.fields() {
				if _, ok := seen[f.Name]; !ok {
					seen[f.Name] = f
				}
			}
		}
		for _, f := range seen {
			l.fields = append(l.fields, f)
		}
		sortFields(l.fields)
	})
	return l.fields
}

func (d *Dataset) fields() []Field {
	var out []Field
	for _, f := range d.file.Schema().Fields() {
		name := f.Name()
		switch {
		case name == d.geomCol:
			out = append(out, Field{Name: "geometry", Format: "geometry-any", Geometry: true, Nullable: f.Optional()})
			continue
		case name == d.bboxCol:
			continue
		case !f.Leaf():
			continue
		}
		field := Field{Name: name, Nullable: f.Optional()}
		typ := strings.ToUpper(f.Type().String())
		kind := f.Type().Kind()
		switch {
		case strings.Contains(typ, "TIMESTAMP"):
			field.Type = "string"
			field.Format = "date-time"
		case strings.Contains(typ, "DATE"):
			field.Type = "string"
			field.Format = "date"
		case strings.Contains(typ, "UUID"):
			field.Type = "string"
			field.Format = "uuid"
		case strings.Contains(typ, "JSON"):
			field.Type = "string"
			field.Format = "json"
		case kind == parquet.Boolean:
			field.Type = "boolean"
		case kind == parquet.Int32 || kind == parquet.Int64:
			field.Type = "integer"
		case kind == parquet.Float || kind == parquet.Double:
			field.Type = "number"
		default:
			field.Type = "string"
		}
		out = append(out, field)
	}
	return out
}

func sortFields(fields []Field) {
	for i := 1; i < len(fields); i++ {
		for j := i; j > 0 && fields[j].Name < fields[j-1].Name; j-- {
			fields[j], fields[j-1] = fields[j-1], fields[j]
		}
	}
}

// Features scans full-resolution row groups intersecting bound. It returns the
// page, the number of matching features, and whether the scan completed. A
// limit of zero or less means no limit (callers must impose one).
func (l *Layer) Features(ctx context.Context, bound orb.Bound, limit, offset int, f *filt.Filter) ([]Feature, int, bool, error) {
	if limit <= 0 {
		limit = int(^uint(0) >> 1)
	}
	features := make([]Feature, 0, minInt(limit, 1024))
	matched := 0
	complete := true
	for pi, p := range l.partitions {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, err
		}
		if !intersects(p.bound, bound) {
			continue
		}
		ds, err := p.get()
		if err != nil {
			return nil, 0, false, err
		}
		bounds, err := l.boundsFor(p, ds, 0)
		if err != nil {
			return nil, 0, false, err
		}
		for _, rgb := range bounds {
			if !intersects(rgb.Bound, bound) {
				continue
			}
			row := 0
			stop := false
			var scanErr error
			err := ds.ScanRowGroupUntil(rgb.Index, func(geom []byte, gb orb.Bound, hasBBox bool, props map[string]any) bool {
				if err := ctx.Err(); err != nil {
					scanErr = err
					return false
				}
				rowIndex := row
				row++
				g, err := wkb.Unmarshal(geom)
				if err != nil {
					return true
				}
				if !hasBBox {
					gb = g.Bound()
				}
				if !intersects(gb, bound) {
					return true
				}
				if clip.Geometry(bound, g) == nil {
					return true
				}
				if f != nil {
					ok, err := f.Match(props)
					if err != nil {
						scanErr = err
						return false
					}
					if !ok {
						return true
					}
				}
				matched++
				if matched <= offset {
					return true
				}
				id := l.featureID(props, pi, rgb.Index, rowIndex)
				gb = g.Bound()
				features = append(features, Feature{
					ID: id, Geometry: g, Properties: props,
					BBox: []float64{gb.Min.X(), gb.Min.Y(), gb.Max.X(), gb.Max.Y()},
				})
				if len(features) >= limit {
					stop = true
					return false
				}
				return true
			})
			if scanErr != nil {
				return nil, 0, false, scanErr
			}
			if err != nil {
				return nil, 0, false, err
			}
			if stop {
				complete = false
				return features, matched, complete, nil
			}
		}
	}
	return features, matched, complete, nil
}

func (l *Layer) featureID(props map[string]any, partition, rowGroup, row int) string {
	if l.idColumn != "" {
		if v, ok := props[l.idColumn]; ok && v != nil {
			return fmt.Sprint(v)
		}
	}
	for _, name := range []string{"id", "gid", "fid", "feature_id"} {
		if v, ok := props[name]; ok && v != nil {
			return fmt.Sprint(v)
		}
	}
	return fmt.Sprintf("%d-%d-%d", partition, rowGroup, row)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
