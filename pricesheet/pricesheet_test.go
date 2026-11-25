package pricesheet

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/agent-sdk/config"
	"github.com/craggy-io/foundation-kit/eventlog"
)

func testKey(t *testing.T, id string) (eventlog.Signer, eventlog.Key) {
	t.Helper()
	k, err := eventlog.GenerateKey(id)
	if err != nil {
		t.Fatal(err)
	}
	s, err := k.Signer()
	if err != nil {
		t.Fatal(err)
	}
	return s, k.Key()
}

func sheetV(v int64) Sheet {
	s := Builtin()
	s.Version = v
	s.Published = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	return s
}

func TestBuiltin(t *testing.T) {
	s := Builtin()
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if s.Version != 0 {
		t.Fatalf("builtin version %d, want 0 so any published sheet wins", s.Version)
	}
	if s.Tier(Decision).ID == "" || s.Tier(Audio).ID != "x-ai/grok-voice-tts-1.0" || s.Tier(Audio).PriceUnit != "characters" {
		t.Fatalf("purpose-specific defaults: %+v %+v", s.Tier(Decision), s.Tier(Audio))
	}
}

func TestLegacySheetGetsNewDefaultsAfterVerification(t *testing.T) {
	signer, key := testKey(t, "legacy")
	legacy := sheetV(1)
	delete(legacy.Models, Decision)
	delete(legacy.Models, Audio)
	data, err := Sign(t.Context(), signer, legacy)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(data, []eventlog.Key{key})
	if err != nil {
		t.Fatal(err)
	}
	if got.Tier(Decision).ID != Builtin().Tier(Decision).ID || got.Tier(Audio).ID != Builtin().Tier(Audio).ID {
		t.Fatalf("legacy defaults: %+v", got.Models)
	}
}

func TestTrustedRoots(t *testing.T) {
	if keys := TrustedRoots(); len(keys) == 0 || keys[0].ID != "craggy-prices-1" {
		t.Fatalf("roots %+v", keys)
	}
}

func TestSignVerify(t *testing.T) {
	signer, key := testKey(t, "k1")
	data, err := Sign(context.Background(), signer, sheetV(3))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(data, []eventlog.Key{key})
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 || got.Tier(Med).ID == "" {
		t.Fatalf("got %+v", got)
	}

	_, other := testKey(t, "k1")
	if _, err := Verify(data, []eventlog.Key{other}); err == nil {
		t.Fatal("a sheet signed by another key with the same ID was trusted")
	}
	if _, err := Verify(data, nil); err == nil {
		t.Fatal("a sheet with no trusted roots was trusted")
	}
	tampered := bytes.Replace(data, []byte(`"version":3`), []byte(`"version":9`), 1)
	if _, err := Verify(tampered, []eventlog.Key{key}); err == nil {
		t.Fatal("a tampered sheet was trusted")
	}
}

func TestSignRefusesABadSheet(t *testing.T) {
	signer, _ := testKey(t, "k1")
	s := sheetV(1)
	s.Models = map[string]Model{Low: {ID: "x"}, "huge": {ID: "y"}}
	_, err := Sign(context.Background(), signer, s)
	if err == nil {
		t.Fatal("signed a sheet missing slots")
	}
	for _, want := range []string{"no model for med", "no model for embed", `unknown slot "huge"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}

func TestProfiles(t *testing.T) {
	s := sheetV(1)
	s.Models[High] = Model{ID: "anthropic/claude-opus-5.5", MaxOutput: 8000}
	c := config.Defaults()
	s.Profiles(&c)
	if c.Profiles["deep"].Model != "anthropic/claude-opus-5.5" || c.Profiles["deep"].MaxOutputTokens > 8000 {
		t.Fatalf("deep = %+v", c.Profiles["deep"])
	}
	if c.Profiles["default"].Model != s.Tier(Med).ID || c.Profiles["default"].Fallbacks != nil {
		t.Fatalf("default = %+v", c.Profiles["default"])
	}
	if c.Profiles["fast"].Model != s.Tier(Low).ID {
		t.Fatalf("fast = %+v", c.Profiles["fast"])
	}
}

type server struct {
	srv   *httptest.Server
	body  atomic.Value
	calls atomic.Int32
}

func serve(t *testing.T) *server {
	s := &server{}
	s.body.Store([]byte(nil))
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.calls.Add(1)
		b := s.body.Load().([]byte)
		if b == nil {
			http.NotFound(w, nil)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func TestReader(t *testing.T) {
	ctx := context.Background()
	signer, key := testKey(t, "k1")
	srv := serve(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	cache := filepath.Join(t.TempDir(), "sheet.json")
	r := &Reader{URL: srv.srv.URL, Roots: []eventlog.Key{key}, CacheFile: cache, Now: func() time.Time { return now }}

	// Nothing published: the built-in sheet, and the failure reported.
	if s := r.Current(ctx); s.Version != 0 || r.Status().Source != "built-in" || r.Status().Error == "" {
		t.Fatalf("got v%d %+v", s.Version, r.Status())
	}

	publish := func(v int64) {
		data, err := Sign(ctx, signer, sheetV(v))
		if err != nil {
			t.Fatal(err)
		}
		srv.body.Store(data)
	}
	publish(5)
	// Within MaxAge nothing is fetched.
	if s := r.Current(ctx); s.Version != 0 || srv.calls.Load() != 1 {
		t.Fatalf("fetched within MaxAge: v%d, %d calls", s.Version, srv.calls.Load())
	}
	now = now.Add(time.Hour)
	if s := r.Current(ctx); s.Version != 5 || r.Status().Source != "fetched" || r.Status().Error != "" {
		t.Fatalf("got v%d %+v", s.Version, r.Status())
	}

	// An older sheet is refused; the held one stays.
	publish(4)
	if s, st := r.Refresh(ctx); s.Version != 5 || !strings.Contains(st.Error, "older") {
		t.Fatalf("rolled back to v%d %+v", s.Version, st)
	}

	// A broken file is refused too.
	srv.body.Store([]byte(`{"signature":{},"sheet":{}}`))
	if s, st := r.Refresh(ctx); s.Version != 5 || st.Error == "" {
		t.Fatalf("got v%d %+v", s.Version, st)
	}

	// A new reader starts from the kept file, unreachable or not.
	srv.srv.Close()
	r2 := &Reader{URL: srv.srv.URL, Roots: []eventlog.Key{key}, CacheFile: cache, Now: func() time.Time { return now }}
	if s := r2.Current(ctx); s.Version != 5 || r2.Status().Source != "cache" {
		t.Fatalf("got v%d %+v", s.Version, r2.Status())
	}
}
