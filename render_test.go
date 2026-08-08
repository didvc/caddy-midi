package caddymidi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/sinshu/go-meltysynth/meltysynth"
)

func testSoundFont(t *testing.T) *meltysynth.SoundFont {
	t.Helper()
	sf, err := meltysynth.NewSoundFont(newSliceReader(buildSoundFont()))
	if err != nil {
		t.Fatalf("building test soundfont: %v", err)
	}
	return sf
}

func TestRenderProducesPlayableWAV(t *testing.T) {
	const (
		rate = 22050
		tail = time.Second
	)

	var buf bytes.Buffer
	n, err := Render(testSoundFont(t), bytes.NewReader(buildMIDI(69, 2)), rate, tail, &buf)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Errorf("Render reported %d bytes but wrote %d", n, buf.Len())
	}

	// Two quarter notes at 120 BPM is 1s, plus the 1s tail.
	if want := WAVSize(2*time.Second, rate); n != want {
		t.Errorf("rendered %d bytes, want %d", n, want)
	}

	got := buf.Bytes()
	if string(got[0:4]) != "RIFF" || string(got[8:12]) != "WAVE" || string(got[36:40]) != "data" {
		t.Fatalf("output is not a WAV file: % x", got[:44])
	}
	if v := binary.LittleEndian.Uint16(got[22:]); v != wavChannels {
		t.Errorf("channels = %d, want %d", v, wavChannels)
	}
	if v := binary.LittleEndian.Uint32(got[24:]); v != rate {
		t.Errorf("sample rate = %d, want %d", v, rate)
	}
	if v := binary.LittleEndian.Uint16(got[34:]); v != wavBitsPerSample {
		t.Errorf("bits per sample = %d, want %d", v, wavBitsPerSample)
	}
	if v := binary.LittleEndian.Uint32(got[4:]); int64(v) != n-8 {
		t.Errorf("RIFF size field = %d, want %d", v, n-8)
	}
	if v := binary.LittleEndian.Uint32(got[40:]); int64(v) != n-wavHeaderSize {
		t.Errorf("data size field = %d, want %d", v, n-wavHeaderSize)
	}

	// The note must actually sound: check that the first quarter second of
	// audio has real amplitude rather than silence.
	var peak int16
	for i := wavHeaderSize; i+1 < wavHeaderSize+rate/4*wavBytesPerFrame; i += 2 {
		if s := int16(binary.LittleEndian.Uint16(got[i:])); s > peak {
			peak = s
		}
	}
	if peak < 1000 {
		t.Errorf("rendered audio is silent or near-silent: peak amplitude %d", peak)
	}
}

func TestRenderReleasesNoteIntoTail(t *testing.T) {
	const rate = 22050

	var withTail, withoutTail bytes.Buffer
	sf := testSoundFont(t)

	if _, err := Render(sf, bytes.NewReader(buildMIDI(69, 1)), rate, 0, &withoutTail); err != nil {
		t.Fatalf("Render without tail: %v", err)
	}
	if _, err := Render(sf, bytes.NewReader(buildMIDI(69, 1)), rate, 3*time.Second, &withTail); err != nil {
		t.Fatalf("Render with tail: %v", err)
	}

	extra := int64(withTail.Len() - withoutTail.Len())
	if want := int64(3 * rate * wavBytesPerFrame); extra != want {
		t.Errorf("tail added %d bytes, want %d", extra, want)
	}
}

func TestRenderRejectsNonMIDI(t *testing.T) {
	var buf bytes.Buffer
	_, err := Render(testSoundFont(t), bytes.NewReader([]byte("this is not a midi file")), 44100, 0, &buf)
	if !errors.Is(err, ErrInvalidMIDI) {
		t.Fatalf("error = %v, want it to wrap ErrInvalidMIDI", err)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	sf := testSoundFont(t)
	midi := buildMIDI(60, 1)

	var first, second bytes.Buffer
	if _, err := Render(sf, bytes.NewReader(midi), 22050, time.Second, &first); err != nil {
		t.Fatalf("first render: %v", err)
	}
	if _, err := Render(sf, bytes.NewReader(midi), 22050, time.Second, &second); err != nil {
		t.Fatalf("second render: %v", err)
	}

	// Caching the output is only sound if rendering is a pure function of its
	// inputs, including across the reuse of one SoundFont by many requests.
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("two renders of the same input produced different audio")
	}
}

func TestWAVSize(t *testing.T) {
	tests := []struct {
		d    time.Duration
		rate int
		want int64
	}{
		{0, 44100, wavHeaderSize},
		{-time.Second, 44100, wavHeaderSize},
		{time.Second, 44100, wavHeaderSize + 44100*4},
		{time.Second / 2, 8000, wavHeaderSize + 4000*4},
	}

	for _, tt := range tests {
		if got := WAVSize(tt.d, tt.rate); got != tt.want {
			t.Errorf("WAVSize(%v, %d) = %d, want %d", tt.d, tt.rate, got, tt.want)
		}
	}
}

func TestClamp16(t *testing.T) {
	tests := []struct {
		in   float32
		want int16
	}{
		{0, 0},
		{1, 32767},
		{-1, -32767},
		{5, 32767},   // clipped, not wrapped
		{-5, -32768}, // clipped, not wrapped
	}

	for _, tt := range tests {
		if got := clamp16(tt.in); got != tt.want {
			t.Errorf("clamp16(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
