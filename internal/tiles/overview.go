package tiles

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sync"

	"github.com/parquet-go/parquet-go"
	"github.com/paulmach/orb"
)

type overviewsMeta struct {
	Levels []struct {
		RowGroupEnd int     `json:"row_group_end"`
		GSD         float64 `json:"gsd"`
		Zoom        *int    `json:"zoom"`
	} `json:"levels"`
}

type Level struct {
	Start int
	End   int
	Zoom  int
}

type rgbBound struct {
	Index int
	Bound orb.Bound
}

type Overview struct {
	f      *os.File
	file   *parquet.File
	levels []Level
	mu     sync.Mutex
	bounds map[int][]rgbBound
}

func openOverview(path string) (*Overview, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	pf, err := parquet.OpenFile(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	raw, ok := pf.Lookup("geo:overviews")
	if !ok {
		f.Close()
		return nil, nil
	}
	var meta overviewsMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		f.Close()
		return nil, err
	}
	o := &Overview{f: f, file: pf, bounds: map[int][]rgbBound{}}
	prev := -1
	for _, l := range meta.Levels {
		z := 0
		if l.Zoom != nil {
			z = *l.Zoom
		} else if l.GSD > 0 {
			z = int(math.Round(math.Log2(40075016.69 / (1024 * l.GSD))))
		}
		o.levels = append(o.levels, Level{Start: prev + 1, End: l.RowGroupEnd, Zoom: z})
		prev = l.RowGroupEnd
	}
	return o, nil
}

func (o *Overview) LevelForZoom(z int) int {
	if len(o.levels) == 0 {
		return 0
	}
	best := 0
	for i, l := range o.levels {
		if l.Zoom <= z {
			best = i
		}
	}
	return best
}

type bboxOnly struct {
	BBox BBox `parquet:"bbox"`
}

func (o *Overview) BoundsForLevel(level int) ([]rgbBound, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if b, ok := o.bounds[level]; ok {
		return b, nil
	}
	if level < 0 || level >= len(o.levels) {
		return nil, fmt.Errorf("level %d out of range", level)
	}
	lvl := o.levels[level]
	rgs := o.file.RowGroups()
	out := make([]rgbBound, 0, lvl.End-lvl.Start+1)
	for i := lvl.Start; i <= lvl.End && i < len(rgs); i++ {
		r := parquet.NewGenericRowGroupReader[bboxOnly](rgs[i])
		rows := make([]bboxOnly, 1024)
		minX, minY := math.Inf(1), math.Inf(1)
		maxX, maxY := math.Inf(-1), math.Inf(-1)
		for {
			n, err := r.Read(rows)
			for j := 0; j < n; j++ {
				b := rows[j].BBox
				if b.Xmin < minX {
					minX = b.Xmin
				}
				if b.Ymin < minY {
					minY = b.Ymin
				}
				if b.Xmax > maxX {
					maxX = b.Xmax
				}
				if b.Ymax > maxY {
					maxY = b.Ymax
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				r.Close()
				return nil, err
			}
		}
		r.Close()
		if math.IsInf(minX, 1) {
			minX, minY, maxX, maxY = -180, -85, 180, 85
		}
		out = append(out, rgbBound{Index: i, Bound: orb.Bound{Min: orb.Point{minX, minY}, Max: orb.Point{maxX, maxY}}})
	}
	o.bounds[level] = out
	return out, nil
}

func (o *Overview) ScanRowGroup(rg int, fn func(Row)) error {
	r := parquet.NewGenericRowGroupReader[Row](o.file.RowGroups()[rg])
	defer r.Close()
	rows := make([]Row, 1024)
	for {
		n, err := r.Read(rows)
		for i := 0; i < n; i++ {
			fn(rows[i])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
