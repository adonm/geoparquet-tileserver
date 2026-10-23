// Package artifact stores small derived artifacts in an object store that
// speaks the pgvs3 plain-HTTP/S3 interface.
package artifact

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Store struct {
	base   *url.URL
	bucket string
	client *http.Client
}

func New(baseURL, bucket string) (*Store, error) {
	if baseURL == "" || bucket == "" {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("object store URL must be an absolute http(s) URL")
	}
	return &Store{base: u, bucket: bucket, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (s *Store) url(key string) string {
	parts := []string{s.bucket}
	for _, part := range strings.Split(strings.TrimPrefix(key, "/"), "/") {
		if part != "" {
			parts = append(parts, url.PathEscape(part))
		}
	}
	u := *s.base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.Join(parts, "/")
	return u.String()
}

// Get returns an artifact and whether it exists.
func (s *Store) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(key), nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("object store GET %s: %s", key, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// Put writes an immutable artifact. An existing object is treated as success.
func (s *Store) Put(ctx context.Context, key, contentType string, body []byte) error {
	if s == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.url(key), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("If-None-Match", "*")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusPreconditionFailed:
		return nil
	default:
		return fmt.Errorf("object store PUT %s: %s", key, resp.Status)
	}
}
