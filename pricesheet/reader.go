package pricesheet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/craggy-io/foundation-kit/eventlog"
	"github.com/craggy-io/foundation-kit/fsx"
)

// maxFile caps a download: a sheet is a few kilobytes.
const maxFile = 1 << 20

// Reader holds the newest trusted sheet it has seen. The zero value reads URL
// with TrustedRoots, keeps nothing on disk and re-fetches hourly.
type Reader struct {
	URL   string         // default URL
	Roots []eventlog.Key // default TrustedRoots()
	// CacheFile, when set, keeps the last good file on disk, read at start-up.
	CacheFile string
	MaxAge    time.Duration // default one hour
	// Timeout bounds one fetch; default five seconds.
	Timeout time.Duration
	HTTP    *http.Client
	Now     func() time.Time

	mu      sync.Mutex
	loaded  bool
	sheet   Sheet
	checked time.Time
	status  Status
}

// Status says where the held sheet came from, for a status line.
type Status struct {
	// Source is "built-in", "cache" or "fetched".
	Source    string    `json:"source"`
	Version   int64     `json:"version"`
	Published time.Time `json:"published"`
	// Checked is the last fetch attempt; Error its failure, if it failed.
	Checked time.Time `json:"checked"`
	Error   string    `json:"error,omitempty"`
}

// Current returns the newest trusted sheet, fetching first when the held one
// was checked more than MaxAge ago. A failed fetch keeps what is held and is
// reported by Status, never returned: a service always has a sheet.
func (r *Reader) Current(ctx context.Context) Sheet {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadLocked()
	if r.now().Sub(r.checked) >= or(r.MaxAge, time.Hour) {
		r.refreshLocked(ctx)
	}
	return r.sheet
}

// Refresh fetches now, whatever the sheet's age, and returns the result.
func (r *Reader) Refresh(ctx context.Context) (Sheet, Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadLocked()
	r.refreshLocked(ctx)
	return r.sheet, r.status
}

// Status reports the held sheet.
func (r *Reader) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadLocked()
	return r.status
}

func (r *Reader) loadLocked() {
	if r.loaded {
		return
	}
	r.loaded = true
	r.hold(Builtin(), "built-in")
	if r.CacheFile == "" {
		return
	}
	if data, err := os.ReadFile(r.CacheFile); err == nil {
		if s, err := Verify(data, r.roots()); err == nil {
			r.hold(s, "cache")
		}
	}
}

func (r *Reader) refreshLocked(ctx context.Context) {
	r.checked = r.now()
	r.status.Checked = r.checked
	data, err := r.fetch(ctx)
	var s Sheet
	if err == nil {
		s, err = Verify(data, r.roots())
	}
	if err == nil && s.Version < r.sheet.Version {
		err = fmt.Errorf("pricesheet: the published sheet is version %d, older than the %d held", s.Version, r.sheet.Version)
	}
	if err != nil {
		r.status.Error = err.Error()
		return
	}
	r.status.Error = ""
	r.hold(s, "fetched")
	if r.CacheFile != "" {
		if err := fsx.WriteFile(r.CacheFile, data, 0o644); err != nil {
			r.status.Error = "pricesheet: keeping the sheet: " + err.Error()
		}
	}
}

func (r *Reader) hold(s Sheet, source string) {
	r.sheet = s
	r.status.Source, r.status.Version, r.status.Published = source, s.Version, s.Published
}

func (r *Reader) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, or(r.Timeout, 5*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, or(r.URL, URL), nil)
	if err != nil {
		return nil, err
	}
	client := r.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricesheet: fetching: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricesheet: fetching: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("pricesheet: fetching: %w", err)
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("pricesheet: the sheet is over %d bytes", maxFile)
	}
	return data, nil
}

func (r *Reader) roots() []eventlog.Key {
	if r.Roots != nil {
		return r.Roots
	}
	return TrustedRoots()
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func or[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}
