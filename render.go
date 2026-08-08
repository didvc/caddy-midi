package caddymidi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/sinshu/go-meltysynth/meltysynth"
)

// ErrInvalidMIDI reports a file that is not a parseable Standard MIDI File.
// It is a client error, not a server error, so the handler maps it to 415.
var ErrInvalidMIDI = errors.New("invalid MIDI file")

const (
	wavChannels      = 2
	wavBitsPerSample = 16
	wavBytesPerFrame = wavChannels * wavBitsPerSample / 8
	wavHeaderSize    = 44

	// renderBlockFrames bounds memory: synthesis happens one block at a time
	// and each block is written out immediately, so a 30-second file and a
	// 30-minute file cost the same resident memory.
	renderBlockFrames = 4096
)

// Render synthesizes the Standard MIDI File in r through sf and writes a
// complete 16-bit stereo WAV file to w, returning the number of bytes written.
//
// tail is extra audio rendered past the final MIDI event, so release envelopes
// and reverb decay naturally instead of being chopped off mid-ring.
func Render(sf *meltysynth.SoundFont, r io.Reader, sampleRate int, tail time.Duration, w io.Writer) (int64, error) {
	midiFile, err := meltysynth.NewMidiFile(r)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrInvalidMIDI, err)
	}

	settings := meltysynth.NewSynthesizerSettings(int32(sampleRate))
	synth, err := meltysynth.NewSynthesizer(sf, settings)
	if err != nil {
		return 0, fmt.Errorf("creating synthesizer: %w", err)
	}

	seq := meltysynth.NewMidiFileSequencer(synth)
	seq.Play(midiFile, false)

	frames := frameCount(midiFile.GetLength()+tail, sampleRate)

	written, err := writeWAVHeader(w, frames, sampleRate)
	if err != nil {
		return written, err
	}

	left := make([]float32, renderBlockFrames)
	right := make([]float32, renderBlockFrames)
	pcm := make([]byte, renderBlockFrames*wavBytesPerFrame)

	for remaining := frames; remaining > 0; {
		n := int(min(int64(renderBlockFrames), remaining))

		seq.Render(left[:n], right[:n])
		for i := range n {
			binary.LittleEndian.PutUint16(pcm[wavBytesPerFrame*i:], uint16(clamp16(left[i])))
			binary.LittleEndian.PutUint16(pcm[wavBytesPerFrame*i+2:], uint16(clamp16(right[i])))
		}

		nw, err := w.Write(pcm[:n*wavBytesPerFrame])
		written += int64(nw)
		if err != nil {
			return written, err
		}
		remaining -= int64(n)
	}

	return written, nil
}

// WAVSize returns the exact byte length Render produces for a MIDI file of the
// given total duration. PCM is constant bitrate, so the size is knowable
// without rendering — which is what makes byte ranges and seeking work.
func WAVSize(d time.Duration, sampleRate int) int64 {
	return wavHeaderSize + frameCount(d, sampleRate)*wavBytesPerFrame
}

func frameCount(d time.Duration, sampleRate int) int64 {
	if d <= 0 {
		return 0
	}
	return int64(math.Ceil(d.Seconds() * float64(sampleRate)))
}

func writeWAVHeader(w io.Writer, frames int64, sampleRate int) (int64, error) {
	dataSize := frames * wavBytesPerFrame
	if dataSize > math.MaxUint32-wavHeaderSize {
		return 0, errors.New("rendered audio exceeds the 4 GiB limit of the WAV format")
	}

	h := make([]byte, 0, wavHeaderSize)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(wavHeaderSize-8+dataSize))
	h = append(h, "WAVE"...)
	h = append(h, "fmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16) // PCM fmt chunk size
	h = binary.LittleEndian.AppendUint16(h, 1)  // PCM, uncompressed
	h = binary.LittleEndian.AppendUint16(h, wavChannels)
	h = binary.LittleEndian.AppendUint32(h, uint32(sampleRate))
	h = binary.LittleEndian.AppendUint32(h, uint32(sampleRate*wavBytesPerFrame))
	h = binary.LittleEndian.AppendUint16(h, wavBytesPerFrame)
	h = binary.LittleEndian.AppendUint16(h, wavBitsPerSample)
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, uint32(dataSize))

	n, err := w.Write(h)
	return int64(n), err
}

// clamp16 hard-limits rather than normalizing. Peak normalization would need
// the whole rendered buffer in memory, and it would make loudness depend on
// the single loudest note in the file.
func clamp16(v float32) int16 {
	s := float64(v) * math.MaxInt16
	switch {
	case s > math.MaxInt16:
		return math.MaxInt16
	case s < math.MinInt16:
		return math.MinInt16
	}
	return int16(s)
}
