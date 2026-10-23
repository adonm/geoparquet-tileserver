package tiles

import (
	"fmt"
	"sort"
	"time"

	"github.com/paulmach/orb"
)

const IndexSchemaVersion = 1

// maxIndexPartitions bounds one background index build. Larger catalogs would
// otherwise force a cold process to open every remote footer at once; they
// retain the normal lazy row-group path instead.
const maxIndexPartitions = 64

// Index is a compact, catalog-versioned snapshot of a layer's metadata. It is
// stored outside the source Parquet so a cold process can avoid reopening
// every footer and scanning bbox pages.
type Index struct {
	SchemaVersion  int              `json:"schema_version"`
	CatalogVersion string           `json:"catalog_version"`
	Collection     string           `json:"collection"`
	GeneratedAt    time.Time        `json:"generated_at"`
	Bounds         [4]float64       `json:"bounds"`
	Fields         []Field          `json:"fields"`
	Partitions     []PartitionIndex `json:"partitions"`
}

type PartitionIndex struct {
	Asset  string       `json:"asset"`
	Bounds [4]float64   `json:"bounds"`
	Levels []LevelIndex `json:"levels"`
}

type LevelIndex struct {
	Level     int             `json:"level"`
	RowGroups []RowGroupIndex `json:"row_groups"`
}

type RowGroupIndex struct {
	Index  int        `json:"index"`
	Bounds [4]float64 `json:"bounds"`
}

func (l *Layer) BuildIndex(catalogVersion string) (*Index, error) {
	if len(l.partitions) > maxIndexPartitions {
		return nil, fmt.Errorf("layer %q has %d partitions, above index limit %d", l.Name, len(l.partitions), maxIndexPartitions)
	}
	idx := &Index{
		SchemaVersion: IndexSchemaVersion, CatalogVersion: catalogVersion,
		Collection: l.Name, GeneratedAt: time.Now().UTC(),
		Bounds: [4]float64{l.Bounds.Min.X(), l.Bounds.Min.Y(), l.Bounds.Max.X(), l.Bounds.Max.Y()},
	}
	seenFields := map[string]Field{}
	for _, p := range l.partitions {
		ds, err := p.get()
		if err != nil {
			return nil, err
		}
		pi := PartitionIndex{
			Asset: p.asset,
			Bounds: [4]float64{
				p.bound.Min.X(), p.bound.Min.Y(), p.bound.Max.X(), p.bound.Max.Y(),
			},
		}
		for level := range ds.levels {
			bounds, err := ds.BoundsForLevel(level)
			if err != nil {
				return nil, err
			}
			li := LevelIndex{Level: level, RowGroups: make([]RowGroupIndex, 0, len(bounds))}
			for _, b := range bounds {
				li.RowGroups = append(li.RowGroups, RowGroupIndex{
					Index: b.Index,
					Bounds: [4]float64{
						b.Bound.Min.X(), b.Bound.Min.Y(), b.Bound.Max.X(), b.Bound.Max.Y(),
					},
				})
			}
			pi.Levels = append(pi.Levels, li)
		}
		for _, f := range ds.fields() {
			if _, ok := seenFields[f.Name]; !ok {
				seenFields[f.Name] = f
			}
		}
		idx.Partitions = append(idx.Partitions, pi)
	}
	for _, f := range seenFields {
		idx.Fields = append(idx.Fields, f)
	}
	sort.Slice(idx.Fields, func(i, j int) bool { return idx.Fields[i].Name < idx.Fields[j].Name })
	return idx, nil
}

// ApplyIndex installs precomputed schema and row-group bounds. It must be
// called before the first Fields()/Render()/Features() call for best effect.
func (l *Layer) ApplyIndex(idx *Index) error {
	if idx == nil {
		return nil
	}
	if idx.SchemaVersion != IndexSchemaVersion {
		return fmt.Errorf("unsupported index schema %d", idx.SchemaVersion)
	}
	l.fieldsOnce.Do(func() {
		l.fields = append([]Field(nil), idx.Fields...)
	})
	l.indexMu.Lock()
	defer l.indexMu.Unlock()
	if l.indexBounds == nil {
		l.indexBounds = map[int]map[int][]rgBound{}
	}
	for pi := range idx.Partitions {
		levels := map[int][]rgBound{}
		for _, li := range idx.Partitions[pi].Levels {
			bounds := make([]rgBound, 0, len(li.RowGroups))
			for _, rg := range li.RowGroups {
				bounds = append(bounds, rgBound{
					Index: rg.Index,
					Bound: orb.Bound{
						Min: orb.Point{rg.Bounds[0], rg.Bounds[1]},
						Max: orb.Point{rg.Bounds[2], rg.Bounds[3]},
					},
				})
			}
			levels[li.Level] = bounds
		}
		l.indexBounds[pi] = levels
	}
	return nil
}
