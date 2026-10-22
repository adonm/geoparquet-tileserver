package tiles

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type source struct {
	ra     io.ReaderAt
	size   int64
	closer io.Closer
}

func openSource(path string) (*source, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		client := &http.Client{Timeout: 60 * time.Second}
		size, err := remoteSize(client, path)
		if err != nil {
			return nil, err
		}
		h := &httpReaderAt{url: path, client: client, chunks: map[int64][]byte{}}
		return &source{ra: h, size: size, closer: nil}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &source{ra: f, size: st.Size(), closer: f}, nil
}

func (s *source) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

func remoteSize(client *http.Client, url string) (int64, error) {
	if req, err := http.NewRequestWithContext(context.Background(), http.MethodHead, url, nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && n > 0 {
					return n, nil
				}
			}
		}
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range request to %s: unexpected status %s", url, resp.Status)
	}
	cr := resp.Header.Get("Content-Range")
	if i := strings.LastIndex(cr, "/"); i >= 0 {
		return strconv.ParseInt(cr[i+1:], 10, 64)
	}
	return 0, fmt.Errorf("range request to %s: missing Content-Range", url)
}

const httpChunkSize = 1 << 20
const httpCacheMax = 64 << 20

type httpReaderAt struct {
	url    string
	client *http.Client

	mu     sync.Mutex
	chunks map[int64][]byte
	bytes  int
}

func (h *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		abs := off + int64(n)
		start := abs - abs%httpChunkSize
		chunk, err := h.chunk(start)
		if err != nil {
			return n, err
		}
		rel := abs - start
		if rel >= int64(len(chunk)) {
			return n, io.EOF
		}
		c := copy(p[n:], chunk[rel:])
		n += c
		if c == 0 {
			return n, io.EOF
		}
	}
	return n, nil
}

func (h *httpReaderAt) chunk(start int64) ([]byte, error) {
	h.mu.Lock()
	if b, ok := h.chunks[start]; ok {
		h.mu.Unlock()
		return b, nil
	}
	h.mu.Unlock()
	b, err := h.fetch(start, httpChunkSize)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.chunks[start] = b
	h.bytes += len(b)
	if h.bytes > httpCacheMax {
		h.chunks = map[int64][]byte{start: b}
		h.bytes = len(b)
	}
	h.mu.Unlock()
	return b, nil
}

func (h *httpReaderAt) fetch(off int64, length int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusOK {
		return io.ReadAll(resp.Body)
	}
	return nil, fmt.Errorf("range request to %s: status %s", h.url, resp.Status)
}
