package tiles

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"
	"github.com/paulmach/orb"
)

type overviewsMeta struct {
	Levels []struct {
		RowGroupEnd int     `json:"row_group_end"`
		GSD         float64 `json:"gsd"`
		Zoom        *int    `json:"zoom"`
	} `json:"levels"`
}

type BBox struct {
	Xmin float64 `parquet:"xmin"`
	Ymin float64 `parquet:"ymin"`
	Xmax float64 `parquet:"xmax"`
	Ymax float64 `parquet:"ymax"`
}

type Level struct {
	Start int
	End   int
	Zoom  int
}

type rgBound struct {
	Index int
	Bound orb.Bound
}

type Dataset struct {
	src      *source
	file     *parquet.File
	geomCol  string
	bboxCol  string
	levels   []Level
	overview bool
	hasBBox  bool

	mu     sync.Mutex
	bounds map[int][]rgBound
}

func openDataset(asset, geomCol string) (*Dataset, error) {
	src, err := openSource(asset)
	if err != nil {
		return nil, err
	}
	pf, err := parquet.OpenFile(src.ra, src.size)
	if err != nil {
		src.Close()
		return nil, err
	}
	d := &Dataset{src: src, file: pf, geomCol: geomCol, bounds: map[int][]rgBound{}}
	d.geomCol = detectGeometryColumn(pf, geomCol)
	d.bboxCol = detectBBoxColumn(pf)
	d.hasBBox = d.bboxCol != ""
	if raw, ok := pf.Lookup("geo:overviews"); ok {
		var meta overviewsMeta
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			src.Close()
			return nil, err
		}
		prev := -1
		for _, l := range meta.Levels {
			z := 0
			if l.Zoom != nil {
				z = *l.Zoom
			} else if l.GSD > 0 {
				z = int(math.Round(math.Log2(40075016.69 / (1024 * l.GSD))))
			}
			d.levels = append(d.levels, Level{Start: prev + 1, End: l.RowGroupEnd, Zoom: z})
			prev = l.RowGroupEnd
		}
		d.overview = len(d.levels) > 1
	}
	if len(d.levels) == 0 {
		d.levels = []Level{{Start: 0, End: len(pf.RowGroups()) - 1, Zoom: 0}}
	}
	return d, nil
}

func detectGeometryColumn(f *parquet.File, configured string) string {
	cols := f.Schema().Columns()
	has := func(name string) bool {
		for _, p := range cols {
			if len(p) == 1 && p[0] == name {
				return true
			}
		}
		return false
	}
	if configured != "" && has(configured) {
		return configured
	}
	if raw, ok := f.Lookup("geo"); ok {
		var meta struct {
			PrimaryColumn string `json:"primary_column"`
		}
		if json.Unmarshal([]byte(raw), &meta) == nil && meta.PrimaryColumn != "" && has(meta.PrimaryColumn) {
			return meta.PrimaryColumn
		}
	}
	for _, name := range []string{"geometry", "geom", "wkb_geometry"} {
		if has(name) {
			return name
		}
	}
	return configured
}

func detectBBoxColumn(f *parquet.File) string {
	var first string
	for _, p := range f.Schema().Columns() {
		if len(p) != 2 {
			continue
		}
		if p[1] != "xmin" && p[1] != "ymin" && p[1] != "xmax" && p[1] != "ymax" {
			continue
		}
		if p[0] == "bbox" {
			return "bbox"
		}
		if first == "" {
			first = p[0]
		}
	}
	return first
}

func (d *Dataset) LevelForZoom(z int) int {
	if len(d.levels) == 0 {
		return 0
	}
	best := 0
	for i, l := range d.levels {
		if l.Zoom <= z {
			best = i
		}
	}
	return best
}

type bboxOnly struct {
	BBox BBox `parquet:"bbox"`
}

func (d *Dataset) BoundsForLevel(level int) ([]rgBound, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.bounds[level]; ok {
		return b, nil
	}
	if level < 0 || level >= len(d.levels) {
		return nil, fmt.Errorf("level %d out of range", level)
	}
	lvl := d.levels[level]
	rgs := d.file.RowGroups()
	out := make([]rgBound, 0, lvl.End-lvl.Start+1)

	childIdx := map[string]int{}
	if d.hasBBox {
		for i, p := range d.file.Schema().Columns() {
			if len(p) == 2 && p[0] == d.bboxCol {
				childIdx[p[1]] = i
			}
		}
	}

	for i := lvl.Start; i <= lvl.End && i < len(rgs); i++ {
		b := d.rowGroupBound(i, rgs[i], childIdx)
		out = append(out, rgBound{Index: i, Bound: b})
	}
	d.bounds[level] = out
	return out, nil
}

func (d *Dataset) rowGroupBound(i int, rg parquet.RowGroup, childIdx map[string]int) orb.Bound {
	if !d.hasBBox {
		return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	}
	// Fast path: read only the bbox child columns' pages.
	if b, ok := d.pageBound(i, childIdx); ok {
		return b
	}
	if b, ok := d.metadataBound(i); ok {
		return b
	}
	chunks := rg.ColumnChunks()
	minX, minY, maxX, maxY := 180.0, 90.0, -180.0, -90.0
	ok := true
	for _, name := range []string{"xmin", "ymin", "xmax", "ymax"} {
		idx, exists := childIdx[name]
		if !exists || idx >= len(chunks) {
			ok = false
			break
		}
		ci, err := chunks[idx].ColumnIndex()
		if err != nil || ci == nil || ci.NumPages() == 0 {
			ok = false
			break
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for p := 0; p < ci.NumPages(); p++ {
			if ci.NullPage(p) {
				continue
			}
			lv := valueFloat(ci.MinValue(p))
			hv := valueFloat(ci.MaxValue(p))
			if lv < lo {
				lo = lv
			}
			if hv > hi {
				hi = hv
			}
		}
		if math.IsNaN(lo) || math.IsNaN(hi) {
			ok = false
			break
		}
		switch name {
		case "xmin":
			minX = lo
		case "ymin":
			minY = lo
		case "xmax":
			maxX = hi
		case "ymax":
			maxY = hi
		}
	}
	if ok && minX <= maxX && minY <= maxY {
		return orb.Bound{Min: orb.Point{minX, minY}, Max: orb.Point{maxX, maxY}}
	}

	// Fallback: read the bbox values. This is used by files without a page index.
	if rg.NumRows() > 50000 {
		return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	}
	minX, minY, maxX, maxY = 180.0, 90.0, -180.0, -90.0
	found := false
	_ = d.ScanRowGroup(i, func(_ []byte, b orb.Bound, has bool, _ map[string]any) {
		if !has {
			return
		}
		found = true
		if b.Min.X() < minX {
			minX = b.Min.X()
		}
		if b.Min.Y() < minY {
			minY = b.Min.Y()
		}
		if b.Max.X() > maxX {
			maxX = b.Max.X()
		}
		if b.Max.Y() > maxY {
			maxY = b.Max.Y()
		}
	})
	if !found || minX > maxX || minY > maxY {
		return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	}
	return orb.Bound{Min: orb.Point{minX, minY}, Max: orb.Point{maxX, maxY}}
}

func (d *Dataset) pageBound(rgIndex int, childIdx map[string]int) (orb.Bound, bool) {
	rgs := d.file.RowGroups()
	if rgIndex >= len(rgs) {
		return orb.Bound{}, false
	}
	chunks := rgs[rgIndex].ColumnChunks()
	vals := map[string]float64{}
	for _, name := range []string{"xmin", "ymin", "xmax", "ymax"} {
		idx, ok := childIdx[name]
		if !ok || idx >= len(chunks) {
			return orb.Bound{}, false
		}
		pages := chunks[idx].Pages()
		lo, hi := math.Inf(1), math.Inf(-1)
		found := false
		for {
			page, err := pages.ReadPage()
			if err == io.EOF {
				break
			}
			if err != nil {
				pages.Close()
				return orb.Bound{}, false
			}
			mn, mx, ok := page.Bounds()
			if ok {
				lv, hv := valueFloat(mn), valueFloat(mx)
				if !math.IsNaN(lv) && !math.IsNaN(hv) {
					if lv < lo {
						lo = lv
					}
					if hv > hi {
						hi = hv
					}
					found = true
				}
			} else {
				// Some writers omit page statistics. Read the values so a
				// missing page bound never silently truncates a row group.
				values := page.Values()
				buf := make([]parquet.Value, 256)
				for {
					n, err := values.ReadValues(buf)
					for i := 0; i < n; i++ {
						v := buf[i]
						if v.IsNull() {
							continue
						}
						f := valueFloat(v)
						if math.IsNaN(f) {
							continue
						}
						if f < lo {
							lo = f
						}
						if f > hi {
							hi = f
						}
						found = true
					}
					if err == io.EOF {
						break
					}
					if err != nil {
						pages.Close()
						return orb.Bound{}, false
					}
				}
			}
		}
		pages.Close()
		if !found {
			return orb.Bound{}, false
		}
		if name == "xmin" || name == "ymin" {
			vals[name] = lo
		} else {
			vals[name] = hi
		}
	}
	return orb.Bound{Min: orb.Point{vals["xmin"], vals["ymin"]}, Max: orb.Point{vals["xmax"], vals["ymax"]}}, true
}

func (d *Dataset) metadataBound(rgIndex int) (orb.Bound, bool) {
	meta := d.file.Metadata()
	if meta == nil || rgIndex >= len(meta.RowGroups) {
		return orb.Bound{}, false
	}
	rg := meta.RowGroups[rgIndex]
	vals := map[string]float64{}
	for _, name := range []string{"xmin", "ymin", "xmax", "ymax"} {
		found := false
		for _, ch := range rg.Columns {
			p := []string(ch.MetaData.PathInSchema)
			if len(p) != 2 || p[0] != d.bboxCol || p[1] != name {
				continue
			}
			st := ch.MetaData.Statistics
			raw := st.MinValue
			if len(raw) == 0 {
				raw = st.Min
			}
			if len(raw) == 0 {
				break
			}
			vals[name] = decodeStat(ch.MetaData.Type, raw)
			found = true
			break
		}
		if !found {
			return orb.Bound{}, false
		}
	}
	return orb.Bound{
		Min: orb.Point{vals["xmin"], vals["ymin"]},
		Max: orb.Point{vals["xmax"], vals["ymax"]},
	}, true
}

func decodeStat(t format.Type, b []byte) float64 {
	switch t {
	case format.Float:
		if len(b) >= 4 {
			return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
		}
	case format.Double:
		if len(b) >= 8 {
			return math.Float64frombits(binary.LittleEndian.Uint64(b))
		}
	case format.Int32:
		if len(b) >= 4 {
			return float64(int32(binary.LittleEndian.Uint32(b)))
		}
	case format.Int64:
		if len(b) >= 8 {
			return float64(int64(binary.LittleEndian.Uint64(b)))
		}
	}
	return math.NaN()
}

func valueFloat(v parquet.Value) float64 {
	switch v.Kind() {
	case parquet.Float:
		return float64(v.Float())
	case parquet.Double:
		return v.Double()
	case parquet.Int32:
		return float64(v.Int32())
	case parquet.Int64:
		return float64(v.Int64())
	}
	return math.NaN()
}

type rowFunc func(geom []byte, bound orb.Bound, hasBBox bool, props map[string]any)

func (d *Dataset) ScanRowGroup(rg int, fn rowFunc) error {
	return d.scanRowGroup(rg, func(geom []byte, bound orb.Bound, hasBBox bool, props map[string]any) bool {
		fn(geom, bound, hasBBox, props)
		return true
	})
}

// ScanRowGroupUntil scans one row group and stops early when fn returns false.
func (d *Dataset) ScanRowGroupUntil(rg int, fn func(geom []byte, bound orb.Bound, hasBBox bool, props map[string]any) bool) error {
	return d.scanRowGroup(rg, fn)
}

func (d *Dataset) scanRowGroup(rg int, fn func(geom []byte, bound orb.Bound, hasBBox bool, props map[string]any) bool) error {
	r := parquet.NewRowGroupReader(d.file.RowGroups()[rg])
	defer r.Close()
	columns := r.Schema().Columns()
	rows := make([]parquet.Row, 1024)
	for {
		n, err := r.ReadRows(rows)
		for i := 0; i < n; i++ {
			var geom []byte
			var bound orb.Bound
			hasBBox := false
			props := map[string]any{}
			rows[i].Range(func(col int, vals []parquet.Value) bool {
				if len(vals) == 0 || vals[0].IsNull() {
					return true
				}
				path := columns[col]
				if len(path) == 0 {
					return true
				}
				v := vals[0]
				if len(path) == 1 {
					if path[0] == d.geomCol {
						geom = append([]byte(nil), v.ByteArray()...)
						return true
					}
					if val, ok := decodeValue(v); ok {
						props[path[0]] = val
					}
					return true
				}
				if len(path) == 2 && path[0] == d.bboxCol {
					switch path[1] {
					case "xmin":
						bound.Min[0] = valueFloat(v)
						hasBBox = true
					case "ymin":
						bound.Min[1] = valueFloat(v)
					case "xmax":
						bound.Max[0] = valueFloat(v)
					case "ymax":
						bound.Max[1] = valueFloat(v)
					}
				}
				return true
			})
			if geom != nil {
				if !fn(geom, bound, hasBBox, props) {
					return nil
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func decodeValue(v parquet.Value) (any, bool) {
	switch v.Kind() {
	case parquet.Boolean:
		return v.Boolean(), true
	case parquet.Int32:
		return int64(v.Int32()), true
	case parquet.Int64:
		return v.Int64(), true
	case parquet.Float:
		return float64(v.Float()), true
	case parquet.Double:
		return v.Double(), true
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return string(v.ByteArray()), true
	}
	return nil, false
}
