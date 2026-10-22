package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen     string  `yaml:"listen"`
	BaseURL    string  `yaml:"base_url"`
	Catalog    string  `yaml:"catalog"`
	CacheTiles int     `yaml:"cache_tiles"`
	Preload    bool    `yaml:"preload"`
	Layers     []Layer `yaml:"layers"`
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
	if c.CacheTiles == 0 {
		c.CacheTiles = 4096
	}
	if c.Catalog == "" {
		return nil, fmt.Errorf("catalog is required")
	}
	if len(c.Layers) == 0 {
		return nil, fmt.Errorf("at least one layer is required")
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
