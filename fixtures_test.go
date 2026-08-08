package caddymidi

import (
	"encoding/binary"
	"math"
)

// The tests build their own SoundFont and MIDI file instead of checking in
// binary fixtures, so the suite stays hermetic and the inputs stay readable.

// SF2 generator operators used below; see the SoundFont 2.04 spec, section 8.1.
const (
	genSampleModes = 54
	genSampleID    = 53
	genInstrument  = 41
)

const (
	// A 100-frame period at 44000 Hz is exactly 440 Hz, so the sample loops
	// seamlessly without any fractional-period click.
	sampleFrames     = 1000
	samplePeriod     = 100
	sampleRateHz     = 44000
	sampleTerminator = 46 // spec requires >= 46 zero frames after the sample
)

// buildSoundFont returns a minimal but valid .sf2: one preset on bank 0,
// program 0, backed by a single looping sine sample that sounds 440 Hz at MIDI
// key 69.
func buildSoundFont() []byte {
	samples := make([]int16, sampleFrames+sampleTerminator)
	for i := range sampleFrames {
		phase := 2 * math.Pi * float64(i) / samplePeriod
		samples[i] = int16(math.Round(math.Sin(phase) * 20000))
	}

	info := listChunk("INFO",
		concat(
			chunk("ifil", binary.LittleEndian.AppendUint32(nil, 2|1<<16)), // v2.1
			chunk("isng", fixedString("EMU8000", 8)),
			chunk("INAM", fixedString("caddy-midi test", 16)),
		))

	sdta := listChunk("sdta", chunk("smpl", int16LE(samples)))

	pdta := listChunk("pdta", concat(
		chunk("phdr", concat(
			presetRecord("sine", 0, 0, 0),
			presetRecord("EOP", 0, 0, 1), // terminator
		)),
		chunk("pbag", concat(zoneRecord(0, 0), zoneRecord(1, 0))),
		chunk("pmod", make([]byte, 10)), // terminator record only
		chunk("pgen", concat(
			generatorRecord(genInstrument, 0),
			generatorRecord(0, 0), // terminator
		)),
		chunk("inst", concat(
			instrumentRecord("sine", 0),
			instrumentRecord("EOI", 1), // terminator
		)),
		chunk("ibag", concat(zoneRecord(0, 0), zoneRecord(2, 0))),
		chunk("imod", make([]byte, 10)),
		chunk("igen", concat(
			generatorRecord(genSampleModes, 1), // loop continuously
			generatorRecord(genSampleID, 0),    // must be the last generator
			generatorRecord(0, 0),              // terminator
		)),
		chunk("shdr", concat(
			sampleRecord("sine", 0, sampleFrames),
			sampleRecord("EOS", 0, 0), // terminator
		)),
	))

	return riffChunk("sfbk", concat(info, sdta, pdta))
}

// buildMIDI returns a format-0 Standard MIDI File holding one note of the
// given duration in quarter notes, at 120 BPM.
func buildMIDI(key byte, quarters int) []byte {
	var track []byte
	track = append(track, 0x00, 0xFF, 0x51, 0x03, 0x07, 0xA1, 0x20) // tempo 500000us
	track = append(track, 0x00, 0x90, key, 0x64)                    // note on, velocity 100
	track = append(track, varLen(uint32(480*quarters))...)
	track = append(track, 0x80, key, 0x40) // note off
	track = append(track, 0x00, 0xFF, 0x2F, 0x00)

	header := []byte{0, 0 /*format 0*/, 0, 1 /*1 track*/, 0x01, 0xE0 /*480 ticks per quarter*/}

	return concat(
		chunkBE("MThd", header),
		chunkBE("MTrk", track),
	)
}

// varLen encodes a MIDI variable-length quantity.
func varLen(v uint32) []byte {
	buf := []byte{byte(v & 0x7F)}
	for v >>= 7; v > 0; v >>= 7 {
		buf = append([]byte{byte(v&0x7F | 0x80)}, buf...)
	}
	return buf
}

// chunk writes a little-endian RIFF chunk. Sizes are kept even by
// construction: the parser advances by the declared size and would desync on a
// pad byte it does not know about.
func chunk(id string, body []byte) []byte {
	if len(body)%2 != 0 {
		panic("caddy-midi test: RIFF chunk " + id + " must have an even length")
	}
	out := append([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(out, body...)
}

func listChunk(listType string, body []byte) []byte {
	return chunk("LIST", append([]byte(listType), body...))
}

func riffChunk(formType string, body []byte) []byte {
	return chunk("RIFF", append([]byte(formType), body...))
}

// chunkBE writes a big-endian chunk header, as used by SMF.
func chunkBE(id string, body []byte) []byte {
	out := append([]byte(id), binary.BigEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(out, body...)
}

func presetRecord(name string, program, bank, bagIndex uint16) []byte {
	out := fixedString(name, 20)
	out = binary.LittleEndian.AppendUint16(out, program)
	out = binary.LittleEndian.AppendUint16(out, bank)
	out = binary.LittleEndian.AppendUint16(out, bagIndex)
	return append(out, make([]byte, 12)...) // library, genre, morphology
}

func instrumentRecord(name string, bagIndex uint16) []byte {
	return binary.LittleEndian.AppendUint16(fixedString(name, 20), bagIndex)
}

func zoneRecord(genIndex, modIndex uint16) []byte {
	out := binary.LittleEndian.AppendUint16(nil, genIndex)
	return binary.LittleEndian.AppendUint16(out, modIndex)
}

func generatorRecord(op, value uint16) []byte {
	out := binary.LittleEndian.AppendUint16(nil, op)
	return binary.LittleEndian.AppendUint16(out, value)
}

func sampleRecord(name string, start, end uint32) []byte {
	out := fixedString(name, 20)
	out = binary.LittleEndian.AppendUint32(out, start)
	out = binary.LittleEndian.AppendUint32(out, end)
	out = binary.LittleEndian.AppendUint32(out, start) // loop start
	out = binary.LittleEndian.AppendUint32(out, end)   // loop end
	out = binary.LittleEndian.AppendUint32(out, sampleRateHz)
	out = append(out, 69, 0)                        // original key A4, no correction
	out = binary.LittleEndian.AppendUint16(out, 0)  // sample link
	return binary.LittleEndian.AppendUint16(out, 1) // monoSample
}

func fixedString(s string, length int) []byte {
	out := make([]byte, length)
	copy(out, s)
	return out
}

func int16LE(values []int16) []byte {
	out := make([]byte, 0, len(values)*2)
	for _, v := range values {
		out = binary.LittleEndian.AppendUint16(out, uint16(v))
	}
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
