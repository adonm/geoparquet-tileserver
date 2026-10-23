// Package catalog loads a STAC Catalog/Collection tree and exposes the
// collections and GeoParquet items needed by the tile server.
package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/paulmach/orb"
)

type Link struct {
	Rel   string `json:"rel"`
	Href  string `json:"href"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type Asset struct {
	Href  string   `json:"href"`
	Type  string   `json:"type"`
	Roles []string `json:"roles"`
}

type Item struct {
	Type       string           `json:"type"`
	ID         string           `json:"id"`
	Collection string           `json:"collection"`
	BBox       []float64        `json:"bbox"`
	Assets     map[string]Asset `json:"assets"`
	Links      []Link           `json:"links"`
	Properties map[string]any   `json:"properties"`
	Base       string           `json:"-"`
	Raw        json.RawMessage  `json:"-"`
}

// Collection is a STAC collection. Items are loaded lazily so a catalog root
// with hundreds of item links can be browsed without fetching them all.
type Collection struct {
	ID          string
	Title       string
	Description string
	License     string
	Keywords    []string
	BBox        []float64
	Assets      map[string]Asset
	Links       []Link
	Base        string
	Raw         json.RawMessage

	directItems []Item
	once        sync.Once
	items       []Item
	err         error
}

type Catalog struct {
	ID          string
	Title       string
	Description string
	Base        string
	Collections []*Collection
	Groups      []*Group

	byID    map[string]*Collection
	groupID map[string]*Group
	mu      sync.Mutex
}

// Group is a virtual collection stitched from the direct child collections of
// a STAC catalog (for example an Overture theme such as "divisions"). It lets
// one MVT layer combine several GeoParquet sub-collections while still using
// the normal filter pipeline.
type Group struct {
	ID          string
	Title       string
	Description string
	Base        string
	PMTiles     string
	Collections []*Collection
	once        sync.Once
	items       []Item
	err         error
}

type node struct {
	id           string
	title        string
	description  string
	base         string
	isCollection bool
	collection   *Collection
	children     []*node
}

type rawHead struct {
	Type        string           `json:"type"`
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	License     string           `json:"license"`
	Keywords    []string         `json:"keywords"`
	BBox        []float64        `json:"bbox"`
	Assets      map[string]Asset `json:"assets"`
	Links       []Link           `json:"links"`
	Extent      struct {
		Spatial struct {
			BBox [][]float64 `json:"bbox"`
		} `json:"spatial"`
	} `json:"extent"`
}

// Load reads a STAC root, release, collection, or item. It walks child
// catalogs and collection metadata, but does not fetch item documents until a
// collection is actually requested. If a catalog has versioned child releases,
// only the newest release is followed.
func Load(path string) (*Catalog, error) {
	root := Resolve(".", path)
	c := &Catalog{Base: root, byID: map[string]*Collection{}, groupID: map[string]*Group{}}
	seen := map[string]bool{}
	var seenMu sync.Mutex
	fetchSem := make(chan struct{}, 8)

	var walk func(string, int) (*node, error)
	walk = func(p string, depth int) (*node, error) {
		seenMu.Lock()
		if depth > 16 || seen[p] {
			seenMu.Unlock()
			return nil, nil
		}
		seen[p] = true
		seenMu.Unlock()
		fetchSem <- struct{}{}
		data, err := Fetch(p)
		<-fetchSem
		if err != nil {
			return nil, err
		}
		var head rawHead
		if err := json.Unmarshal(data, &head); err != nil {
			return nil, err
		}

		switch {
		case head.Type == "Feature":
			var it Item
			if err := json.Unmarshal(data, &it); err != nil {
				return nil, err
			}
			it.Base = p
			if it.ID == "" {
				it.ID = pathID(p)
			}
			id := firstNonEmpty(it.Collection, it.ID)
			col := &Collection{
				ID:          id,
				Title:       id,
				BBox:        it.BBox,
				Base:        p,
				directItems: []Item{it},
			}
			c.add(col)
			return &node{id: id, title: id, isCollection: true, collection: col}, nil
		case isCollection(head):
			col := newCollection(p, head, data)
			c.add(col)
			return &node{id: col.ID, title: col.Title, isCollection: true, collection: col}, nil
		}

		n := &node{id: head.ID, title: head.Title, description: head.Description, base: p}
		if n.title == "" {
			n.title = n.id
		}
		if c.ID == "" {
			c.ID = n.id
			c.Title = n.title
			c.Description = n.description
		}
		// A catalog can itself expose items. Treat it as a synthetic
		// collection when it has item links but no child catalogs.
		if hasRel(head.Links, "item") && !hasRel(head.Links, "child") {
			col := newCollection(p, head, data)
			c.add(col)
			return &node{id: col.ID, title: col.Title, isCollection: true, collection: col}, nil
		}
		links := childLinks(head.Links)
		type childResult struct {
			node *node
			err  error
		}
		results := make([]childResult, len(links))
		if len(links) == 1 {
			hp := Resolve(p, links[0].Href)
			if hp != "" {
				results[0].node, results[0].err = walk(hp, depth+1)
			}
		} else if len(links) > 1 {
			var wg sync.WaitGroup
			for i, l := range links {
				hp := Resolve(p, l.Href)
				if hp == "" {
					continue
				}
				wg.Add(1)
				go func(i int, hp string) {
					defer wg.Done()
					results[i].node, results[i].err = walk(hp, depth+1)
				}(i, hp)
			}
			wg.Wait()
		}
		for _, r := range results {
			if r.err != nil {
				return nil, r.err
			}
			if r.node != nil {
				n.children = append(n.children, r.node)
			}
		}
		if len(n.children) > 0 {
			direct := make([]*Collection, 0, len(n.children))
			for _, child := range n.children {
				if child.isCollection && child.collection != nil {
					direct = append(direct, child.collection)
				}
			}
			if len(direct) > 1 && n.id != "" {
				var pmtiles string
				for _, l := range head.Links {
					if l.Rel == "pmtiles" {
						pmtiles = Resolve(p, l.Href)
						break
					}
				}
				c.addGroup(&Group{
					ID:          n.id,
					Title:       firstNonEmpty(n.title, n.id),
					Description: n.description,
					Base:        n.base,
					PMTiles:     pmtiles,
					Collections: direct,
				})
			}
		}
		return n, nil
	}

	if _, err := walk(root, 0); err != nil {
		return nil, err
	}
	if c.ID == "" && len(c.Collections) > 0 {
		c.ID = c.Collections[0].ID
		c.Title = c.Collections[0].Title
	}
	sort.Slice(c.Collections, func(i, j int) bool {
		return c.Collections[i].ID < c.Collections[j].ID
	})
	sort.Slice(c.Groups, func(i, j int) bool {
		return c.Groups[i].ID < c.Groups[j].ID
	})
	return c, nil
}

func (c *Catalog) addGroup(g *Group) {
	if g == nil || g.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.groupID[g.ID]; exists {
		return
	}
	// Do not shadow a real collection with the same id.
	if _, exists := c.byID[g.ID]; exists {
		return
	}
	c.groupID[g.ID] = g
	c.Groups = append(c.Groups, g)
}

func (c *Catalog) Group(id string) *Group {
	if c == nil {
		return nil
	}
	return c.groupID[id]
}

func (g *Group) Bound() orb.Bound {
	minX, minY, maxX, maxY := 180.0, 90.0, -180.0, -90.0
	found := false
	for _, col := range g.Collections {
		b := col.Bound()
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
		found = true
	}
	if !found || minX > maxX || minY > maxY {
		return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	}
	return orb.Bound{Min: orb.Point{minX, minY}, Max: orb.Point{maxX, maxY}}
}

func (g *Group) Items() ([]Item, error) {
	g.once.Do(func() {
		type result struct {
			items []Item
			err   error
		}
		results := make(chan result, len(g.Collections))
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for _, col := range g.Collections {
			wg.Add(1)
			go func(col *Collection) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				items, err := col.Items()
				results <- result{items: items, err: err}
			}(col)
		}
		wg.Wait()
		close(results)
		var firstErr error
		for r := range results {
			if r.err != nil {
				if firstErr == nil {
					firstErr = r.err
				}
				continue
			}
			g.items = append(g.items, r.items...)
		}
		sort.Slice(g.items, func(i, j int) bool { return g.items[i].ID < g.items[j].ID })
		if len(g.items) == 0 && firstErr != nil {
			g.err = firstErr
		}
	})
	return g.items, g.err
}

func isCollection(head rawHead) bool {
	if head.Type == "Collection" {
		return true
	}
	return len(head.Extent.Spatial.BBox) > 0 && len(head.Links) > 0
}

func newCollection(p string, head rawHead, raw json.RawMessage) *Collection {
	bbox := head.BBox
	if len(bbox) < 4 && len(head.Extent.Spatial.BBox) > 0 {
		bbox = unionBBoxes(head.Extent.Spatial.BBox)
	}
	id := head.ID
	if id == "" {
		id = pathID(p)
	}
	return &Collection{
		ID:          id,
		Title:       firstNonEmpty(head.Title, id),
		Description: head.Description,
		License:     head.License,
		Keywords:    head.Keywords,
		BBox:        bbox,
		Assets:      head.Assets,
		Links:       head.Links,
		Base:        p,
		Raw:         append(json.RawMessage(nil), raw...),
	}
}

func (c *Catalog) add(col *Collection) {
	if col == nil || col.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.byID[col.ID]; exists {
		return
	}
	c.byID[col.ID] = col
	c.Collections = append(c.Collections, col)
}

func (c *Catalog) Collection(id string) *Collection {
	if c == nil {
		return nil
	}
	return c.byID[id]
}

func (c *Catalog) Bounds() orb.Bound {
	minX, minY, maxX, maxY := 180.0, 90.0, -180.0, -90.0
	found := false
	for _, col := range c.Collections {
		b := col.Bound()
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
		found = true
	}
	if !found || minX > maxX || minY > maxY {
		return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
	}
	return orb.Bound{Min: orb.Point{minX, minY}, Max: orb.Point{maxX, maxY}}
}

func (c *Collection) Bound() orb.Bound {
	if len(c.BBox) >= 4 {
		return orb.Bound{
			Min: orb.Point{c.BBox[0], c.BBox[1]},
			Max: orb.Point{c.BBox[2], c.BBox[3]},
		}
	}
	return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
}

// Temporal returns the collection's declared temporal extent, if any.
func (c *Collection) Temporal() (string, string, bool) {
	if c == nil || len(c.Raw) == 0 {
		return "", "", false
	}
	var raw struct {
		Extent struct {
			Temporal struct {
				Interval [][]*string `json:"interval"`
			} `json:"temporal"`
		} `json:"extent"`
	}
	if json.Unmarshal(c.Raw, &raw) != nil || len(raw.Extent.Temporal.Interval) == 0 {
		return "", "", false
	}
	iv := raw.Extent.Temporal.Interval[0]
	if len(iv) < 2 || iv[0] == nil {
		return "", "", false
	}
	end := *iv[0]
	if iv[1] != nil {
		end = *iv[1]
	}
	return *iv[0], end, true
}

// Items loads the collection's item documents on first use. Collections with
// their own GeoParquet assets (common for single-file Source Cooperative
// datasets) are represented as one synthetic item.
func (c *Collection) Items() ([]Item, error) {
	c.once.Do(func() {
		c.items, c.err = c.loadItems()
	})
	return c.items, c.err
}

func (c *Collection) loadItems() ([]Item, error) {
	if c.directItems != nil {
		return append([]Item(nil), c.directItems...), nil
	}

	var itemLinks []Link
	var itemsHref string
	for _, l := range c.Links {
		switch l.Rel {
		case "item":
			itemLinks = append(itemLinks, l)
		case "items":
			if itemsHref == "" {
				itemsHref = l.Href
			}
		}
	}
	if len(itemLinks) == 0 && itemsHref != "" {
		items, err := fetchItemCollection(Resolve(c.Base, itemsHref))
		if err != nil {
			return nil, err
		}
		for i := range items {
			if items[i].Collection == "" {
				items[i].Collection = c.ID
			}
		}
		return items, nil
	}
	if len(itemLinks) == 0 {
		if len(c.Assets) == 0 {
			return nil, fmt.Errorf("collection %q has no items or assets", c.ID)
		}
		props := map[string]any{}
		if start, end, ok := c.Temporal(); ok {
			props["datetime"] = start
			if start != end {
				props["start_datetime"] = start
				props["end_datetime"] = end
			}
		}
		return []Item{{
			Type:       "Feature",
			ID:         c.ID,
			Collection: c.ID,
			BBox:       c.BBox,
			Assets:     c.Assets,
			Properties: props,
			Base:       c.Base,
		}}, nil
	}

	type result struct {
		index int
		item  Item
		err   error
	}
	results := make(chan result, len(itemLinks))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, l := range itemLinks {
		wg.Add(1)
		go func(i int, l Link) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			href := Resolve(c.Base, l.Href)
			it, err := fetchItem(href)
			if err == nil && it.Collection == "" {
				it.Collection = c.ID
			}
			results <- result{index: i, item: it, err: err}
		}(i, l)
	}
	wg.Wait()
	close(results)

	items := make([]Item, 0, len(itemLinks))
	var firstErr error
	for r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		items = append(items, r.item)
	}
	if len(items) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func fetchItem(href string) (Item, error) {
	data, err := Fetch(href)
	if err != nil {
		return Item{}, err
	}
	var it Item
	if err := json.Unmarshal(data, &it); err != nil {
		return Item{}, err
	}
	it.Base = href
	it.Raw = append(json.RawMessage(nil), data...)
	if it.ID == "" {
		it.ID = pathID(href)
	}
	return it, nil
}

func fetchItemCollection(href string) ([]Item, error) {
	data, err := Fetch(href)
	if err != nil {
		return nil, err
	}
	var fc struct {
		Features []json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(fc.Features))
	for _, raw := range fc.Features {
		var it Item
		if err := json.Unmarshal(raw, &it); err != nil {
			continue
		}
		it.Base = href
		it.Raw = append(json.RawMessage(nil), raw...)
		if it.ID == "" {
			it.ID = pathID(href)
		}
		items = append(items, it)
	}
	return items, nil
}

// childLinks returns the children to follow. STAC roots often contain several
// versioned releases; when all child titles look like versions, follow only
// the newest one. A rel=latest link always wins.
func childLinks(links []Link) []Link {
	for _, l := range links {
		if l.Rel == "latest" {
			return []Link{l}
		}
	}
	var children []Link
	for _, l := range links {
		if l.Rel == "child" {
			children = append(children, l)
		}
	}
	if len(children) > 1 {
		allVersions := true
		for _, l := range children {
			if !isVersionTitle(l.Title) {
				allVersions = false
				break
			}
		}
		if allVersions {
			sort.Slice(children, func(i, j int) bool {
				return children[i].Title < children[j].Title
			})
			return children[len(children)-1:]
		}
	}
	return children
}

func isVersionTitle(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 10 || strings.Count(s, "-") != 2 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func hasRel(links []Link, rel string) bool {
	for _, l := range links {
		if l.Rel == rel {
			return true
		}
	}
	return false
}

func unionBBoxes(boxes [][]float64) []float64 {
	minX, minY, maxX, maxY := 180.0, 90.0, -180.0, -90.0
	found := false
	for _, b := range boxes {
		if len(b) < 4 {
			continue
		}
		if b[0] < minX {
			minX = b[0]
		}
		if b[1] < minY {
			minY = b[1]
		}
		if b[2] > maxX {
			maxX = b[2]
		}
		if b[3] > maxY {
			maxY = b[3]
		}
		found = true
	}
	if !found {
		return nil
	}
	return []float64{minX, minY, maxX, maxY}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func pathID(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".json")
}

func Fetch(path string) ([]byte, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		client := &http.Client{Timeout: 60 * time.Second}
		req, err := http.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "geoparquet-tileserver/0.1")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(path)
}

func Resolve(base, href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	if strings.HasPrefix(href, "file://") {
		u, err := url.Parse(href)
		if err == nil {
			return u.Path
		}
		return strings.TrimPrefix(href, "file://")
	}
	if strings.Contains(href, "://") {
		return ""
	}
	if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
		b, err := url.Parse(base)
		if err != nil {
			return ""
		}
		r, err := url.Parse(href)
		if err != nil {
			return ""
		}
		return b.ResolveReference(r).String()
	}
	if filepath.IsAbs(href) {
		return href
	}
	return filepath.Join(filepath.Dir(base), href)
}

func (it Item) Bound() orb.Bound {
	if len(it.BBox) >= 4 {
		return orb.Bound{Min: orb.Point{it.BBox[0], it.BBox[1]}, Max: orb.Point{it.BBox[2], it.BBox[3]}}
	}
	return orb.Bound{Min: orb.Point{-180, -85}, Max: orb.Point{180, 85}}
}

func (it Item) ParquetAsset() (string, bool) {
	var best string
	for _, a := range it.Assets {
		if contains(a.Roles, "overview") || strings.Contains(a.Href, "*") {
			continue
		}
		if strings.Contains(a.Type, "parquet") || strings.HasSuffix(a.Href, ".parquet") {
			href := Resolve(it.Base, a.Href)
			if href == "" {
				continue
			}
			if best == "" || len(href) < len(best) {
				best = href
			}
		}
	}
	return best, best != ""
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
