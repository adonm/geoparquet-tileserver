package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen       string             `yaml:"listen"`
	BaseURL      string             `yaml:"base_url"`
	Catalog      string             `yaml:"catalog"`
	CacheTilesMB int                `yaml:"cache_tiles_mb"`
	MaxFeatures  int                `yaml:"max_features"`
	Preload      bool               `yaml:"preload"`
	Layers       []Layer            `yaml:"layers"`
	Catalogs     map[string]string  `yaml:"catalogs"`
	ObjectStore  *ObjectStoreConfig `yaml:"object_store"`
	ExcludeProps []string           `yaml:"exclude_properties"`
	IDColumn     string             `yaml:"id_column"`
	Simplify     float64            `yaml:"simplify"`
}

type ObjectStoreConfig struct {
	URL             string             `yaml:"url"`
	ArtifactsBucket string             `yaml:"artifacts_bucket"`
	Routes          []ObjectStoreRoute `yaml:"routes"`
}

type ObjectStoreRoute struct {
	Prefix string `yaml:"prefix"`
	Bucket string `yaml:"bucket"`
}

type Layer struct {
	Name           string `yaml:"name"`
	Collection     string `yaml:"collection"`
	GeometryColumn string `yaml:"geometry_column"`
	MinZoom        int    `yaml:"min_zoom"`
	MaxZoom        int    `yaml:"max_zoom"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.CacheTilesMB == 0 {
		c.CacheTilesMB = 256
	}
	if c.CacheTilesMB < 0 {
		return nil, fmt.Errorf("cache_tiles_mb must be positive")
	}
	if c.MaxFeatures == 0 {
		c.MaxFeatures = 100000
	}
	if c.MaxFeatures < 0 || c.MaxFeatures > 100000 {
		return nil, fmt.Errorf("max_features must be between 1 and 100000")
	}
	if c.Simplify == 0 {
		c.Simplify = 1.0
	}
	if c.Simplify < 0 {
		c.Simplify = 0
	}
	if c.ObjectStore != nil {
		if c.ObjectStore.URL == "" || c.ObjectStore.ArtifactsBucket == "" {
			return nil, fmt.Errorf("object_store requires url and artifacts_bucket")
		}
		for i, route := range c.ObjectStore.Routes {
			if route.Prefix == "" || route.Bucket == "" {
				return nil, fmt.Errorf("object_store.routes[%d] requires prefix and bucket", i)
			}
		}
	}
	if c.Catalog == "" {
		c.Catalog = "https://stac.overturemaps.org/catalog.json"
	}
	for i := range c.Layers {
		if c.Layers[i].Name == "" {
			return nil, fmt.Errorf("layer %d: name is required", i)
		}
		if c.Layers[i].GeometryColumn == "" {
			c.Layers[i].GeometryColumn = "geometry"
		}
		if c.Layers[i].MaxZoom == 0 {
			c.Layers[i].MaxZoom = 14
		}
		if c.Layers[i].MinZoom < 0 || c.Layers[i].MaxZoom > 24 || c.Layers[i].MinZoom > c.Layers[i].MaxZoom {
			return nil, fmt.Errorf("layer %q: invalid zoom range", c.Layers[i].Name)
		}
	}
	return &c, nil
}
