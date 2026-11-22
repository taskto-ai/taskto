package dictation

import (
	"encoding/binary"
	"fmt"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

// SampleRate is the only rate the API takes: what every engine wants, and what the web app
// resamples to.
const SampleRate = 16000

// MaxDuration is the longest dictation. The web app stops recording there; the server
// allows a few seconds more for the time the recorder takes to stop.
const MaxDuration = 10 * time.Minute

// maxMinutes is MaxDuration in minutes, for messages.
const maxMinutes = int(MaxDuration / time.Minute)

const maxSamples = SampleRate * int((MaxDuration+5*time.Second)/time.Second)

// Audio is mono 16 kHz signed 16-bit PCM.
type Audio struct {
	Samples []int16
}

// Duration is how long a lasts.
func (a Audio) Duration() time.Duration {
	return time.Duration(len(a.Samples)) * time.Second / SampleRate
}

// errAudio is a refused recording; its message is safe to show, and code is the API's.
type errAudio struct {
	code   apierr.Code
	msg    string
	params apierr.Params
}

func (e *errAudio) Error() string { return e.msg }

var (
	errNotWAV    = &errAudio{code: apierr.CodeAudioInvalid, msg: "audio must be a WAV file: 16-bit PCM, mono, 16 kHz"}
	errTooLong   = &errAudio{code: apierr.CodeAudioTooLong, msg: fmt.Sprintf("audio is longer than %d minutes", maxMinutes), params: apierr.Params{"minutes": maxMinutes}}
	errTruncated = &errAudio{code: apierr.CodeAudioInvalid, msg: "the WAV file is truncated"}
	errEmpty     = &errAudio{code: apierr.CodeAudioEmpty, msg: "the recording is empty"}
)

// ParseWAV reads a RIFF WAVE file of 16-bit PCM, one channel, 16 kHz. Chunks other than
// fmt and data are skipped.
func ParseWAV(b []byte) (Audio, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return Audio{}, errNotWAV
	}
	var format bool
	for p := 12; ; {
		if p+8 > len(b) {
			return Audio{}, errTruncated
		}
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		p += 8
		body := len(b) - p
		switch {
		case id == "data":
			if !format {
				return Audio{}, errNotWAV
			}
			// A recorder that streams may write the placeholder 0xFFFFFFFF: take what is
			// there. Any other size past the end is a cut file.
			if size > body {
				if size != 0xFFFFFFFF {
					return Audio{}, errTruncated
				}
				size = body
			}
			if size%2 != 0 {
				size--
			}
			if size/2 > maxSamples {
				return Audio{}, errTooLong
			}
			samples := make([]int16, size/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(b[p+2*i:])) //nolint:gosec // PCM is two's complement
			}
			return Audio{Samples: samples}, nil
		case size > body:
			return Audio{}, errTruncated
		case id == "fmt ":
			if size < 16 {
				return Audio{}, errNotWAV
			}
			f := b[p : p+16]
			tag, channels := binary.LittleEndian.Uint16(f[0:2]), binary.LittleEndian.Uint16(f[2:4])
			rate, bits := binary.LittleEndian.Uint32(f[4:8]), binary.LittleEndian.Uint16(f[14:16])
			// 0xFFFE is WAVE_FORMAT_EXTENSIBLE; with 16 bits and one channel it is plain PCM.
			if (tag != 1 && tag != 0xFFFE) || channels != 1 || rate != SampleRate || bits != 16 {
				return Audio{}, errNotWAV
			}
			format = true
		}
		p += size + size%2 // chunks are padded to an even size
	}
}

// WAV encodes a as a WAV file.
func (a Audio) WAV() []byte {
	n := 2 * len(a.Samples)
	b := make([]byte, 44+n)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+n)) //nolint:gosec // at most MaxDuration of samples
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // mono
	binary.LittleEndian.PutUint32(b[24:], SampleRate)
	binary.LittleEndian.PutUint32(b[28:], SampleRate*2) // bytes per second
	binary.LittleEndian.PutUint16(b[32:], 2)            // bytes per frame
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n)) //nolint:gosec // as above
	for i, s := range a.Samples {
		binary.LittleEndian.PutUint16(b[44+2*i:], uint16(s)) //nolint:gosec // two's complement
	}
	return b
}

// frame is the step at which Split measures loudness: 20 ms.
const frame = SampleRate / 50

// Split cuts a into pieces of at most maxLen, for engines that take a limited length at a
// time. Each cut is at the quietest 20 ms in the last third of the piece, so it rarely
// falls inside a word.
func (a Audio) Split(maxLen time.Duration) []Audio {
	maxN := int(maxLen * SampleRate / time.Second)
	if maxN < 3*frame {
		maxN = 3 * frame
	}
	var out []Audio
	s := a.Samples
	for len(s) > maxN {
		cut := maxN
		var quietest int64 = -1
		for at := maxN * 2 / 3; at+frame <= maxN; at += frame {
			var energy int64
			for _, v := range s[at : at+frame] {
				energy += int64(v) * int64(v)
			}
			if quietest < 0 || energy < quietest {
				quietest, cut = energy, at+frame/2
			}
		}
		out = append(out, Audio{Samples: s[:cut]})
		s = s[cut:]
	}
	return append(out, Audio{Samples: s})
}
