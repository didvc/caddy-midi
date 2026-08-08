// Package caddymidi provides a Caddy HTTP handler that serves Standard MIDI
// Files as synthesized audio.
//
// A request for a .mid under the site root is rendered through a SoundFont and
// returned as a WAV stream, so a browser can play it with a plain <audio> tag
// and no JavaScript. Synthesis uses github.com/sinshu/go-meltysynth, which is
// pure Go — an xcaddy build with this module still produces a single static
// binary with no cgo and no audio hardware required.
package caddymidi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/sinshu/go-meltysynth/meltysynth"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(MIDI{})
	httpcaddyfile.RegisterHandlerDirective("midi", parseCaddyfile)
	// Claim MIDI requests before file_server would serve the raw bytes.
	httpcaddyfile.RegisterDirectiveOrder("midi", httpcaddyfile.Before, "file_server")
}

const (
	defaultSampleRate = 44100
	defaultTail       = 2 * time.Second
	defaultMaxSize    = 8 << 20

	// cacheKeyVersion is mixed into every cache key. Bump it whenever a change
	// to rendering would make previously cached WAVs wrong.
	cacheKeyVersion = "1"
)

// MIDI is the caddyhttp handler module. Requests whose path matches one of
// Extensions and resolves to a file under the site root are synthesized;
// everything else is passed to the next handler untouched.
type MIDI struct {
	// SoundFont is the path to the .sf2 file used for synthesis. Required.
	SoundFont string `json:"soundfont,omitempty"`

	// SampleRate of the rendered audio, in Hz. Default 44100. The synthesizer
	// accepts 16000 through 192000.
	SampleRate int `json:"sample_rate,omitempty"`

	// Tail is how much audio to render past the last MIDI event, so notes and
	// reverb are not cut off mid-decay. Default 2s.
	Tail caddy.Duration `json:"tail,omitempty"`

	// CacheDir holds rendered WAV files. Empty means a directory under Caddy's
	// app data dir. The literal "off" disables reuse and re-renders every
	// request, which is only sensible for testing.
	CacheDir string `json:"cache_dir,omitempty"`

	// Extensions this handler claims. Default [".mid", ".midi"]. Matching is
	// case-insensitive.
	Extensions []string `json:"extensions,omitempty"`

	// MaxSize is the largest MIDI file, in bytes, that will be synthesized.
	// Default 8 MiB. Files are read into memory to be hashed and rendered, and
	// a small MIDI file can expand into a very large WAV, so this bounds both.
	MaxSize int64 `json:"max_size,omitempty"`

	soundFont *meltysynth.SoundFont
	// soundFontID identifies the loaded SoundFont's content for cache keying.
	soundFontID string
	cache       *cache
	logger      *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (MIDI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.midi",
		New: func() caddy.Module { return new(MIDI) },
	}
}

// Provision loads the SoundFont once at config load, rather than per request.
func (m *MIDI) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger()

	if m.SampleRate == 0 {
		m.SampleRate = defaultSampleRate
	}
	if m.Tail == 0 {
		m.Tail = caddy.Duration(defaultTail)
	}
	if m.MaxSize == 0 {
		m.MaxSize = defaultMaxSize
	}
	if len(m.Extensions) == 0 {
		m.Extensions = []string{".mid", ".midi"}
	}
	for i, ext := range m.Extensions {
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		m.Extensions[i] = strings.ToLower(ext)
	}

	if m.SoundFont == "" {
		return errors.New("a soundfont is required")
	}
	if err := m.loadSoundFont(); err != nil {
		return fmt.Errorf("loading soundfont %s: %w", m.SoundFont, err)
	}

	if !strings.EqualFold(m.CacheDir, "off") {
		dir, err := m.prepareCacheDir()
		if err != nil {
			return err
		}
		m.cache = &cache{dir: dir}
	}

	cacheDir := "off"
	if m.cache != nil {
		cacheDir = m.cache.dir
	}
	m.logger.Info("midi handler ready",
		zap.String("soundfont", m.SoundFont),
		zap.Int("sample_rate", m.SampleRate),
		zap.Int("presets", len(m.soundFont.Presets)),
		zap.String("cache_dir", cacheDir))

	return nil
}

// prepareCacheDir creates the cache directory, returning the one in use.
//
// An explicitly configured path that cannot be created is a configuration
// error and fails provisioning. The default location is only a preference, so
// when it is unwritable — a read-only or absent HOME, common when running
// ad hoc or in a container — fall back to the temp dir rather than refusing to
// start over a directory of regenerable files.
func (m *MIDI) prepareCacheDir() (string, error) {
	if m.CacheDir != "" {
		if err := os.MkdirAll(m.CacheDir, 0o700); err != nil {
			return "", fmt.Errorf("preparing cache dir %s: %w", m.CacheDir, err)
		}
		return m.CacheDir, nil
	}

	dir := filepath.Join(caddy.AppDataDir(), "midi-cache")
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		return dir, nil
	}

	fallback := filepath.Join(os.TempDir(), "caddy-midi-cache")
	if ferr := os.MkdirAll(fallback, 0o700); ferr != nil {
		return "", fmt.Errorf("preparing cache dir %s: %w", dir, err)
	}

	m.logger.Warn("default cache dir unavailable, falling back to temp dir",
		zap.String("preferred", dir), zap.String("using", fallback), zap.Error(err))
	return fallback, nil
}

// loadSoundFont reads the .sf2 fully into memory before parsing. Beyond being
// simpler, this sidesteps the parser's assumption that a single Read fills the
// buffer, which does not hold for every io.Reader.
func (m *MIDI) loadSoundFont() error {
	data, err := os.ReadFile(m.SoundFont)
	if err != nil {
		return err
	}

	sf, err := meltysynth.NewSoundFont(newSliceReader(data))
	if err != nil {
		return err
	}

	sum := sha256.Sum256(data)
	m.soundFont = sf
	m.soundFontID = hex.EncodeToString(sum[:])
	return nil
}

// Validate checks the configuration independently of provisioning.
func (m *MIDI) Validate() error {
	if m.SampleRate < 16000 || m.SampleRate > 192000 {
		return fmt.Errorf("sample_rate must be between 16000 and 192000, got %d", m.SampleRate)
	}
	if m.Tail < 0 {
		return errors.New("tail must not be negative")
	}
	return nil
}

func (m MIDI) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if !m.claims(r.URL.Path) {
		return next.ServeHTTP(w, r)
	}

	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	root := repl.ReplaceAll("{http.vars.root}", ".")
	filename := caddyhttp.SanitizedPathJoin(root, r.URL.Path)

	info, err := os.Stat(filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Not ours to serve; let file_server or the error routes answer.
			return next.ServeHTTP(w, r)
		}
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	if info.IsDir() {
		return next.ServeHTTP(w, r)
	}
	if info.Size() > m.MaxSize {
		return caddyhttp.Error(http.StatusRequestEntityTooLarge,
			fmt.Errorf("%s is %d bytes, over the %d byte limit", filename, info.Size(), m.MaxSize))
	}

	audio, err := m.audio(filename)
	if err != nil {
		if errors.Is(err, ErrInvalidMIDI) {
			m.logger.Debug("rejecting unparseable midi",
				zap.String("file", filename), zap.Error(err))
			return caddyhttp.Error(http.StatusUnsupportedMediaType, err)
		}
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	defer audio.Close()

	w.Header().Set("Content-Type", "audio/wav")
	// ServeContent handles Range, HEAD, If-Modified-Since and Accept-Ranges.
	// The modtime is the MIDI file's, so conditional requests stay correct
	// even when the cached WAV was written at some unrelated later time.
	http.ServeContent(w, r, path.Base(r.URL.Path)+".wav", info.ModTime(), audio)
	return nil
}

func (m MIDI) claims(urlPath string) bool {
	ext := strings.ToLower(path.Ext(urlPath))
	for _, want := range m.Extensions {
		if ext == want {
			return true
		}
	}
	return false
}

// audio returns a seekable handle to the synthesized WAV for the given file.
//
// The MIDI file is read into memory rather than streamed: it is bounded by
// MaxSize, and having the bytes in hand is what lets the cache key hash actual
// content instead of trusting the mtime.
func (m MIDI) audio(filename string) (*os.File, error) {
	midi, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	render := func(w io.Writer) error {
		_, err := Render(m.soundFont, newSliceReader(midi), m.SampleRate, time.Duration(m.Tail), w)
		return err
	}

	if m.cache == nil {
		return renderToTemp(render)
	}
	return m.cache.open(m.cacheKey(midi), render)
}

// cacheKey covers every input that changes the rendered bytes, so editing a
// MIDI file, swapping the SoundFont or changing the sample rate all invalidate
// naturally, with no explicit purge step.
//
// The MIDI is keyed by content hash rather than path and mtime. A file
// rewritten within the filesystem's timestamp granularity — or regenerated to
// the same length — would otherwise keep serving stale audio. Hashing also
// means two identical MIDI files share one rendering.
func (m MIDI) cacheKey(midi []byte) string {
	content := sha256.Sum256(midi)

	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%d",
		cacheKeyVersion, hex.EncodeToString(content[:]),
		m.soundFontID, m.SampleRate, m.Tail)
	return hex.EncodeToString(h.Sum(nil))
}

// UnmarshalCaddyfile parses the midi directive:
//
//	midi [<matcher>] [<soundfont>] {
//	    soundfont   <path>
//	    sample_rate <hz>
//	    tail        <duration>
//	    cache       <dir>|off
//	    extensions  <ext...>
//	}
func (m *MIDI) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	if d.NextArg() {
		m.SoundFont = d.Val()
	}
	if d.NextArg() {
		return d.ArgErr()
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "soundfont":
			if !d.AllArgs(&m.SoundFont) {
				return d.ArgErr()
			}
		case "sample_rate":
			var raw string
			if !d.AllArgs(&raw) {
				return d.ArgErr()
			}
			rate, err := strconv.Atoi(raw)
			if err != nil {
				return d.Errf("parsing sample_rate: %v", err)
			}
			m.SampleRate = rate
		case "tail":
			var raw string
			if !d.AllArgs(&raw) {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(raw)
			if err != nil {
				return d.Errf("parsing tail: %v", err)
			}
			m.Tail = caddy.Duration(dur)
		case "cache":
			if !d.AllArgs(&m.CacheDir) {
				return d.ArgErr()
			}
		case "max_size":
			var raw string
			if !d.AllArgs(&raw) {
				return d.ArgErr()
			}
			size, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return d.Errf("parsing max_size: %v", err)
			}
			m.MaxSize = size
		case "extensions":
			m.Extensions = d.RemainingArgs()
			if len(m.Extensions) == 0 {
				return d.ArgErr()
			}
		default:
			return d.Errf("unrecognized subdirective %q", d.Val())
		}
	}

	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m MIDI
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

// newSliceReader returns a reader over data whose Read always fills the given
// buffer when enough bytes remain.
func newSliceReader(data []byte) io.Reader { return &sliceReader{data: data} }

type sliceReader struct {
	data []byte
	pos  int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.pos:])
	s.pos += n
	return n, nil
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*MIDI)(nil)
	_ caddy.Validator             = (*MIDI)(nil)
	_ caddyhttp.MiddlewareHandler = (*MIDI)(nil)
	_ caddyfile.Unmarshaler       = (*MIDI)(nil)
)
