# caddy-midi

A Caddy HTTP handler that serves Standard MIDI Files as synthesized audio.

A request for `/song.mid` is rendered through a SoundFont and returned as a WAV
stream, so a browser plays it with a plain `<audio>` tag and no JavaScript:

```html
<audio controls src="/hymns/nearer-my-god.mid"></audio>
```

Synthesis uses [go-meltysynth](https://github.com/sinshu/go-meltysynth), which
is pure Go. An `xcaddy` build with this module is still a single static binary:
no cgo, no libfluidsynth, no ALSA, and no sound card on the server.

## Install

```sh
xcaddy build --with github.com/didvc/caddy-midi
```

## Configure

```caddyfile
midi.example.com {
	root * /srv/midi

	midi {
		soundfont /srv/soundfonts/GeneralUser-GS.sf2
		sample_rate 44100
		tail 3s
		cache /var/cache/caddy-midi
	}

	file_server
}
```

The directive registers itself to run before `file_server`, so no global
`order` line is needed. Requests that do not match a claimed extension, or that
do not resolve to a file, fall through untouched.

| Subdirective  | Default              | Meaning                                                           |
| ------------- | -------------------- | ----------------------------------------------------------------- |
| `soundfont`   | *(required)*         | Path to the `.sf2` used for synthesis.                            |
| `sample_rate` | `44100`              | Output rate in Hz. Accepts 16000–192000.                          |
| `tail`        | `2s`                 | Audio rendered past the last MIDI event so notes and reverb decay.|
| `cache`       | Caddy's app data dir | Directory for rendered WAVs, or `off` to re-render every request. |
| `max_size`    | `8388608`            | Largest MIDI file, in bytes, that will be synthesized.            |
| `extensions`  | `.mid .midi`         | Extensions this handler claims.                                   |

`soundfont` may also be given inline: `midi /srv/gm.sf2`.

## Running it ad hoc

There is no `caddy midi-server` to match `caddy file-server`, because the
built-in subcommands are compiled into Caddy itself and cannot be extended by a
plugin. The equivalent one-liner pipes a Caddyfile in on stdin:

```sh
caddy run --config - --adapter caddyfile <<'EOF'
{
	admin off
	auto_https off
	persist_config off
}
:29061 {
	root * .
	midi {
		soundfont /path/to/GeneralUser-GS.sf2
	}
	file_server browse
}
EOF
```

That is `caddy file-server --listen :29061 --root . --browse` with MIDI
rendering added. Keep it in a shell function if you use it often.

If `$HOME` is unwritable or unset, the default cache location cannot be
created; the handler logs a warning and uses a directory under `$TMPDIR`
instead of refusing to start. A `cache` path you set yourself is treated as
deliberate and fails loudly if it cannot be created.

## Design notes

### Why the output is WAV

The SMF tempo map gives the exact duration before any audio is rendered, and
PCM is constant bitrate, so the response length is known up front and byte
offsets convert exactly to timestamps. That is what makes `Range` requests,
seeking and resume work — Caddy's `http.ServeContent` handles all of it. A
chunked, unknown-length encode would be smaller on the wire but would give up
seeking, which matters more for a music player.

The cost is size: 44.1 kHz stereo 16-bit is roughly 10 MB per minute, and PCM
barely compresses. Drop `sample_rate` to 22050 to halve it, or put this behind
a CDN.

### Caching

Rendering is a pure function of (MIDI bytes, SoundFont, sample rate, tail), so
results are cached on disk and reused. The key is a hash of the MIDI file's
*contents* rather than its path and mtime, because a file regenerated to the
same length within the filesystem's timestamp granularity would otherwise keep
serving stale audio. Hashing content also means the same music under two names
renders once.

Concurrent first requests for the same file collapse into one render via
`singleflight`, and each render lands via a temp file plus atomic rename, so an
interrupted render can never leave a truncated WAV to be served as a hit.

Entries are never evicted: every cached file is reproducible from inputs still
on disk, so the directory is a rebuildable artifact store rather than state.
Point `cache` at a tmpfs, or sweep it on a timer, if that matters.

### Memory and dynamics

Synthesis proceeds one 4096-frame block at a time, written straight out, so a
30-minute MIDI file costs the same resident memory as a 30-second one. MIDI
files themselves are read into memory, bounded by `max_size`.

Samples are hard-limited on the way to 16-bit. Peak normalizing instead would
require holding the whole rendered buffer in memory, and it would make a file's
overall loudness depend on its single loudest note.

## Limits

- No live MIDI hardware output. Caddy's model is request/response; a persistent
  ALSA connection outliving requests fights the config-reload lifecycle and
  would reintroduce cgo. Use a standalone daemon and `reverse_proxy` for that.
- SoundFont v2 only, 16-bit samples. 24-bit sample chunks are ignored, which is
  a limitation of the upstream synthesizer.
- One SoundFont per handler. Use separate matchers and `midi` blocks to serve
  different instrument sets from different paths.

## Tests

```sh
go test -race ./...
```

The suite builds its own SoundFont and MIDI file in memory — a one-preset `.sf2`
wrapping a looping sine sample, and a format-0 SMF — so there are no binary
fixtures and no external assets to fetch.
