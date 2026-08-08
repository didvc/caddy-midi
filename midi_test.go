package caddymidi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// newTestSite writes a SoundFont and a MIDI file into a temp dir, makes that
// dir the working directory (so the site root resolves to "."), and returns a
// provisioned handler.
func newTestSite(t *testing.T, configure func(*MIDI)) *MIDI {
	t.Helper()

	dir := t.TempDir()
	sf2 := filepath.Join(dir, "test.sf2")
	if err := os.WriteFile(sf2, buildSoundFont(), 0o600); err != nil {
		t.Fatalf("writing soundfont: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "song.mid"), buildMIDI(69, 1), 0o600); err != nil {
		t.Fatalf("writing midi: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("writing text file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.mid"), []byte("not a midi file"), 0o600); err != nil {
		t.Fatalf("writing broken midi: %v", err)
	}
	t.Chdir(dir)

	m := &MIDI{
		SoundFont:  sf2,
		SampleRate: 22050,
		Tail:       caddy.Duration(500 * time.Millisecond),
		CacheDir:   filepath.Join(dir, "cache"),
	}
	if configure != nil {
		configure(m)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)

	if err := m.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return m
}

// serve runs one request through the handler, recording whether the request
// fell through to the next handler.
func serve(t *testing.T, m *MIDI, req *http.Request) (*httptest.ResponseRecorder, bool, error) {
	t.Helper()

	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
	rec := httptest.NewRecorder()

	var fellThrough bool
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		fellThrough = true
		w.WriteHeader(http.StatusNotFound)
		return nil
	})

	err := m.ServeHTTP(rec, req, next)
	return rec, fellThrough, err
}

func TestServeHTTPRendersMIDI(t *testing.T) {
	m := newTestSite(t, nil)

	rec, fellThrough, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if fellThrough {
		t.Fatal("request was passed to the next handler instead of being rendered")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", ct)
	}
	if ar := rec.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}

	body := rec.Body.Bytes()
	if string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("body is not a WAV file: % x", body[:min(44, len(body))])
	}
	// One quarter note at 120 BPM plus a 500ms tail.
	if want := WAVSize(time.Second, 22050); int64(len(body)) != want {
		t.Errorf("body is %d bytes, want %d", len(body), want)
	}
}

func TestServeHTTPSupportsRangeRequests(t *testing.T) {
	m := newTestSite(t, nil)

	full, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil))
	if err != nil {
		t.Fatalf("full request: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/song.mid", nil)
	req.Header.Set("Range", "bytes=44-1043")
	rec, _, err := serve(t, m, req)
	if err != nil {
		t.Fatalf("range request: %v", err)
	}

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got, want := rec.Body.Len(), 1000; got != want {
		t.Errorf("partial body is %d bytes, want %d", got, want)
	}
	// Seeking is only useful if the bytes line up with the full rendering.
	if !bytes.Equal(rec.Body.Bytes(), full.Body.Bytes()[44:1044]) {
		t.Error("ranged bytes do not match the same span of the full response")
	}

	if cr := rec.Header().Get("Content-Range"); cr != fmt.Sprintf("bytes 44-1043/%d", full.Body.Len()) {
		t.Errorf("Content-Range = %q", cr)
	}
}

func TestServeHTTPHeadRequest(t *testing.T) {
	m := newTestSite(t, nil)

	get, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	rec, _, err := serve(t, m, httptest.NewRequest(http.MethodHead, "/song.mid", nil))
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d bytes of body, want 0", rec.Body.Len())
	}
	// A HEAD must advertise the length a GET would deliver, or players cannot
	// show a duration or seek before downloading.
	if got, want := rec.Header().Get("Content-Length"), fmt.Sprint(get.Body.Len()); got != want {
		t.Errorf("Content-Length = %q, want %q", got, want)
	}
}

func TestServeHTTPPassesThroughUnclaimedPaths(t *testing.T) {
	m := newTestSite(t, nil)

	for _, path := range []string{"/notes.txt", "/missing.mid", "/"} {
		rec, fellThrough, err := serve(t, m, httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !fellThrough {
			t.Errorf("%s: handled by the midi module, want pass-through (status %d)", path, rec.Code)
		}
	}
}

func TestServeHTTPRejectsUnparseableMIDI(t *testing.T) {
	m := newTestSite(t, nil)

	_, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/broken.mid", nil))
	if err == nil {
		t.Fatal("expected an error for an unparseable .mid")
	}

	var handlerErr caddyhttp.HandlerError
	if !errors.As(err, &handlerErr) {
		t.Fatalf("error = %v, want a caddyhttp.HandlerError", err)
	}
	if handlerErr.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", handlerErr.StatusCode)
	}
}

func TestCacheServesSecondRequestFromDisk(t *testing.T) {
	m := newTestSite(t, nil)

	first, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil))
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	second, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}

	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("cached response differs from the freshly rendered one")
	}

	entries, err := os.ReadDir(m.cache.dir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("cache holds %d entries %v, want exactly 1", len(entries), names)
	}
}

func TestCacheKeyTracksMIDIContent(t *testing.T) {
	m := newTestSite(t, nil)

	if _, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil)); err != nil {
		t.Fatalf("first request: %v", err)
	}

	// Rewrite the file with different music; the cache must not serve the old
	// audio back.
	if err := os.WriteFile("song.mid", buildMIDI(48, 4), 0o600); err != nil {
		t.Fatalf("rewriting midi: %v", err)
	}
	rec, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil))
	if err != nil {
		t.Fatalf("second request: %v", err)
	}

	if want := WAVSize(2500*time.Millisecond, 22050); int64(rec.Body.Len()) != want {
		t.Errorf("body is %d bytes, want %d — stale cache entry served", rec.Body.Len(), want)
	}
}

func TestCacheSharesIdenticalMIDIFiles(t *testing.T) {
	m := newTestSite(t, nil)

	if err := os.WriteFile("copy.mid", buildMIDI(69, 1), 0o600); err != nil {
		t.Fatalf("writing duplicate midi: %v", err)
	}

	for _, path := range []string{"/song.mid", "/copy.mid"} {
		if _, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, path, nil)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}

	// Keying on content rather than path means the same music under two names
	// is rendered once.
	entries, err := os.ReadDir(m.cache.dir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("cache holds %d entries, want 1", len(entries))
	}
}

func TestCacheHandlesConcurrentFirstRequests(t *testing.T) {
	m := newTestSite(t, nil)

	const concurrent = 8
	bodies := make([][]byte, concurrent)
	errs := make([]error, concurrent)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // pile up on the same cold cache entry

			req := httptest.NewRequest(http.MethodGet, "/song.mid", nil)
			req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
			rec := httptest.NewRecorder()
			next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })

			errs[i] = m.ServeHTTP(rec, req, next)
			bodies[i] = rec.Body.Bytes()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		// A half-written cache file served as a hit would show up here as a
		// short or mismatched body.
		if !bytes.Equal(bodies[i], bodies[0]) {
			t.Errorf("request %d returned %d bytes, want %d matching bytes",
				i, len(bodies[i]), len(bodies[0]))
		}
	}
	if len(bodies[0]) == 0 {
		t.Fatal("no audio was returned")
	}
}

func TestServeHTTPRejectsOversizeFile(t *testing.T) {
	m := newTestSite(t, func(m *MIDI) { m.MaxSize = 16 })

	_, _, err := serve(t, m, httptest.NewRequest(http.MethodGet, "/song.mid", nil))
	if err == nil {
		t.Fatal("expected an error for a file over max_size")
	}

	var handlerErr caddyhttp.HandlerError
	if !errors.As(err, &handlerErr) {
		t.Fatalf("error = %v, want a caddyhttp.HandlerError", err)
	}
	if handlerErr.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", handlerErr.StatusCode)
	}
}

func TestCacheDirFallbackAndFailure(t *testing.T) {
	// An unwritable path that was explicitly asked for is a config error.
	unwritable := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(unwritable, 0o500); err != nil {
		t.Fatalf("preparing unwritable dir: %v", err)
	}

	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	sf2 := filepath.Join(t.TempDir(), "test.sf2")
	if err := os.WriteFile(sf2, buildSoundFont(), 0o600); err != nil {
		t.Fatalf("writing soundfont: %v", err)
	}

	m := &MIDI{SoundFont: sf2, CacheDir: filepath.Join(unwritable, "cache")}
	if err := m.Provision(ctx); err == nil {
		t.Error("Provision should fail when an explicit cache dir cannot be created")
	}

	// With no cache dir configured, provisioning must still succeed.
	m = &MIDI{SoundFont: sf2}
	if err := m.Provision(ctx); err != nil {
		t.Fatalf("Provision with the default cache dir: %v", err)
	}
	if m.cache == nil || m.cache.dir == "" {
		t.Error("expected a usable default cache dir")
	}
}

func TestCacheOffStillSeekable(t *testing.T) {
	m := newTestSite(t, func(m *MIDI) { m.CacheDir = "off" })

	if m.cache != nil {
		t.Fatal("cache should be disabled")
	}

	req := httptest.NewRequest(http.MethodGet, "/song.mid", nil)
	req.Header.Set("Range", "bytes=44-143")
	rec, _, err := serve(t, m, req)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rec.Code != http.StatusPartialContent {
		t.Errorf("status = %d, want 206 even with the cache off", rec.Code)
	}
	if rec.Body.Len() != 100 {
		t.Errorf("body is %d bytes, want 100", rec.Body.Len())
	}
}

func TestPathTraversalIsContained(t *testing.T) {
	m := newTestSite(t, nil)

	// The soundfont sits beside the site root; a traversal attempt must not
	// reach anything outside it.
	req := httptest.NewRequest(http.MethodGet, "/../../etc/passwd.mid", nil)
	_, fellThrough, err := serve(t, m, req)
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if !fellThrough {
		t.Error("traversal path was served rather than passed through")
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  MIDI
	}{
		{
			name:  "inline soundfont",
			input: `midi /srv/gm.sf2`,
			want:  MIDI{SoundFont: "/srv/gm.sf2"},
		},
		{
			name: "full block",
			input: `midi {
				soundfont /srv/gm.sf2
				sample_rate 48000
				tail 3s
				cache /var/cache/midi
				max_size 1048576
				extensions .mid .midi .kar
			}`,
			want: MIDI{
				SoundFont:  "/srv/gm.sf2",
				SampleRate: 48000,
				Tail:       caddy.Duration(3 * time.Second),
				CacheDir:   "/var/cache/midi",
				MaxSize:    1 << 20,
				Extensions: []string{".mid", ".midi", ".kar"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m MIDI
			if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tt.input)); err != nil {
				t.Fatalf("UnmarshalCaddyfile: %v", err)
			}
			if m.SoundFont != tt.want.SoundFont || m.SampleRate != tt.want.SampleRate ||
				m.Tail != tt.want.Tail || m.CacheDir != tt.want.CacheDir || m.MaxSize != tt.want.MaxSize {
				t.Errorf("got %+v, want %+v", m, tt.want)
			}
			if len(m.Extensions) != len(tt.want.Extensions) {
				t.Fatalf("extensions = %v, want %v", m.Extensions, tt.want.Extensions)
			}
			for i := range m.Extensions {
				if m.Extensions[i] != tt.want.Extensions[i] {
					t.Errorf("extensions = %v, want %v", m.Extensions, tt.want.Extensions)
					break
				}
			}
		})
	}
}

func TestUnmarshalCaddyfileErrors(t *testing.T) {
	// Written multi-line, as real Caddyfiles are: on a single line the closing
	// brace is on the same line as the subdirective and would be collected as
	// an argument.
	for _, input := range []string{
		"midi /a.sf2 /b.sf2",
		"midi {\n\tsample_rate abc\n}",
		"midi {\n\ttail nope\n}",
		"midi {\n\tbogus x\n}",
		"midi {\n\tmax_size huge\n}",
		"midi {\n\textensions\n}",
		"midi {\n\tsoundfont\n}",
	} {
		var m MIDI
		if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err == nil {
			t.Errorf("%s: expected an error, got none", input)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		m    MIDI
		ok   bool
	}{
		{"default rate", MIDI{SampleRate: 44100}, true},
		{"too low", MIDI{SampleRate: 8000}, false},
		{"too high", MIDI{SampleRate: 384000}, false},
		{"negative tail", MIDI{SampleRate: 44100, Tail: caddy.Duration(-time.Second)}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if tt.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tt.ok && err == nil {
				t.Error("Validate() = nil, want an error")
			}
		})
	}
}

func TestProvisionRequiresSoundFont(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	m := &MIDI{}
	if err := m.Provision(ctx); err == nil {
		t.Error("Provision without a soundfont should fail")
	}

	m = &MIDI{SoundFont: filepath.Join(t.TempDir(), "nope.sf2")}
	if err := m.Provision(ctx); err == nil {
		t.Error("Provision with a missing soundfont should fail")
	}
}
