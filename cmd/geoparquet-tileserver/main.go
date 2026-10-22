package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/sync/singleflight"

	"github.com/adonm/geoparquet-tileserver/internal/catalog"
	"github.com/adonm/geoparquet-tileserver/internal/config"
	"github.com/adonm/geoparquet-tileserver/internal/tiles"
)

type server struct {
	cfg    *config.Config
	cat    *catalog.Catalog
	layers map[string]*tiles.Layer
	cache  *lru.Cache[string, []byte]
	group  singleflight.Group
}

type TileInput struct {
	Collection string `path:"collection" doc:"Collection / layer id"`
	Z          uint8  `path:"z" doc:"Zoom level"`
	X          uint32 `path:"x" doc:"Tile column"`
	Y          string `path:"y" doc:"Tile row, optionally with .mvt suffix"`
	MinCount   int    `query:"min_count" default:"0" doc:"Only include features with count >= this value"`
	Host       string `header:"Host"`
	Proto      string `header:"X-Forwarded-Proto"`
}

type TileOutput struct {
	ContentType  string `header:"Content-Type"`
	CacheControl string `header:"Cache-Control"`
	Body         []byte
}

type TileJSONOutput struct {
	Body *TileJSON
}

type TileJSON struct {
	TileJSON     string        `json:"tilejson"`
	Name         string        `json:"name"`
	Tiles        []string      `json:"tiles"`
	MinZoom      int           `json:"minzoom"`
	MaxZoom      int           `json:"maxzoom"`
	Bounds       []float64     `json:"bounds"`
	VectorLayers []VectorLayer `json:"vector_layers"`
}

type VectorLayer struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
}

type CollectionInfo struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	MinZoom int       `json:"minzoom"`
	MaxZoom int       `json:"maxzoom"`
	Bounds  []float64 `json:"bounds"`
}

type CollectionsOutput struct {
	Body []CollectionInfo
}

type HealthOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}

func (s *server) tile(ctx context.Context, in *TileInput) (*TileOutput, error) {
	layer, ok := s.layers[in.Collection]
	if !ok {
		return nil, huma.Error404NotFound("unknown collection: " + in.Collection)
	}
	if int(in.Z) < layer.MinZoom || int(in.Z) > layer.MaxZoom {
		return nil, huma.Error404NotFound("zoom out of range")
	}
	ys := strings.TrimSuffix(in.Y, ".mvt")
	y, err := strconv.ParseUint(ys, 10, 32)
	if err != nil {
		return nil, huma.Error400BadRequest("invalid y")
	}
	filter := in.MinCount
	var minPtr *int
	if filter > 0 {
		minPtr = &filter
	}
	key := fmt.Sprintf("%s/%d/%d/%d/%d", in.Collection, in.Z, in.X, y, filter)
	if b, ok := s.cache.Get(key); ok {
		return &TileOutput{
			ContentType:  "application/vnd.mapbox-vector-tile",
			CacheControl: "public, max-age=3600",
			Body:         b,
		}, nil
	}
	v, err, _ := s.group.Do(key, func() (any, error) {
		return layer.Render(ctx, in.Z, in.X, uint32(y), minPtr)
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	b := v.([]byte)
	s.cache.Add(key, b)
	return &TileOutput{
		ContentType:  "application/vnd.mapbox-vector-tile",
		CacheControl: "public, max-age=3600",
		Body:         b,
	}, nil
}

func (s *server) tileJSON(ctx context.Context, in *struct {
	Collection string `path:"collection"`
	Host       string `header:"Host"`
	Proto      string `header:"X-Forwarded-Proto"`
}) (*TileJSONOutput, error) {
	layer, ok := s.layers[in.Collection]
	if !ok {
		return nil, huma.Error404NotFound("unknown collection: " + in.Collection)
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	tileURL := "/collections/" + layer.Name + "/tiles/{z}/{x}/{y}.mvt"
	if base != "" {
		tileURL = base + tileURL
	}
	return &TileJSONOutput{Body: &TileJSON{
		TileJSON: "3.0.0",
		Name:     layer.Name,
		Tiles:    []string{tileURL},
		MinZoom:  layer.MinZoom,
		MaxZoom:  layer.MaxZoom,
		Bounds:   []float64{layer.Bounds.Min.X(), layer.Bounds.Min.Y(), layer.Bounds.Max.X(), layer.Bounds.Max.Y()},
		VectorLayers: []VectorLayer{{
			ID:     layer.Name,
			Fields: map[string]string{"count": "Number", "sum_value": "Number"},
		}},
	}}, nil
}

func (s *server) collections(context.Context, *struct{}) (*CollectionsOutput, error) {
	out := &CollectionsOutput{}
	for _, l := range s.layers {
		out.Body = append(out.Body, CollectionInfo{
			ID: l.Name, Name: l.Name, MinZoom: l.MinZoom, MaxZoom: l.MaxZoom,
			Bounds: []float64{l.Bounds.Min.X(), l.Bounds.Min.Y(), l.Bounds.Max.X(), l.Bounds.Max.Y()},
		})
	}
	return out, nil
}

func main() {
	var configPath string
	var listen string
	flag.StringVar(&configPath, "config", "config.yaml", "path to config.yaml")
	flag.StringVar(&listen, "listen", "", "override listen address")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	if listen != "" {
		cfg.Listen = listen
	}
	cat, err := catalog.Load(cfg.Catalog)
	if err != nil {
		slog.Error("catalog", "err", err)
		os.Exit(1)
	}
	s := &server{cfg: cfg, cat: cat, layers: map[string]*tiles.Layer{}}
	for _, lc := range cfg.Layers {
		items := cat.Filter(lc.Collection)
		l, err := tiles.New(tiles.Options{
			Name: lc.Name, Collection: lc.Collection, GeometryColumn: lc.GeometryColumn,
			MinZoom: lc.MinZoom, MaxZoom: lc.MaxZoom, Preload: cfg.Preload,
		}, items)
		if err != nil {
			slog.Error("layer", "name", lc.Name, "err", err)
			os.Exit(1)
		}
		s.layers[lc.Name] = l
	}
	s.cache, _ = lru.New[string, []byte](cfg.CacheTiles)

	router := chi.NewMux()
	humaCfg := huma.DefaultConfig("GeoParquet Tile Server", "0.1.0")
	humaCfg.Info.Description = "A small STAC + GeoParquet vector tile server."
	api := humachi.New(router, humaCfg)

	huma.Register(api, huma.Operation{
		OperationID: "get-tile", Method: http.MethodGet,
		Path:    "/collections/{collection}/tiles/{z}/{x}/{y}",
		Summary: "Get an MVT tile",
		Responses: map[string]*huma.Response{
			"200": {Description: "MVT tile", Content: map[string]*huma.MediaType{
				"application/vnd.mapbox-vector-tile": {},
			}},
		},
	}, s.tile)

	huma.Register(api, huma.Operation{
		OperationID: "get-tilejson", Method: http.MethodGet,
		Path: "/collections/{collection}/tilejson.json", Summary: "Get TileJSON",
	}, s.tileJSON)

	huma.Register(api, huma.Operation{
		OperationID: "list-collections", Method: http.MethodGet,
		Path: "/collections", Summary: "List collections",
	}, s.collections)

	huma.Register(api, huma.Operation{
		OperationID: "health", Method: http.MethodGet, Path: "/healthz", Summary: "Health",
	}, func(context.Context, *struct{}) (*HealthOutput, error) {
		out := &HealthOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	srv := &http.Server{Addr: cfg.Listen, Handler: router, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("listening", "addr", cfg.Listen, "collections", len(s.layers))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
