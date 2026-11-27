package dictation

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

const SampleRate = 16000

const MaxDuration = 10 * time.Minute

const maxMinutes = int(MaxDuration / time.Minute)

const maxSamples = SampleRate * int((MaxDuration+5*time.Second)/time.Second)

type Audio struct {
	Samples []int16
}

func (a Audio) Duration() time.Duration {
	return time.Duration(len(a.Samples)) * time.Second / SampleRate
}

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
			if _, err := binary.Decode(b[p:p+size], binary.LittleEndian, samples); err != nil {
				return Audio{}, errTruncated
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
			if (tag != 1 && tag != 0xFFFE) || channels != 1 || rate != SampleRate || bits != 16 {
				return Audio{}, errNotWAV
			}
			format = true
		}
		p += size + size%2
	}
}

func (a Audio) WAV() []byte {
	n := 2 * len(a.Samples)
	b := make([]byte, 44+n)
	riffSize, dataSize := uint32(math.MaxUint32), uint32(math.MaxUint32)
	if int64(n) <= math.MaxUint32-36 {
		riffSize, dataSize = uint32(36+n), uint32(n)
	}
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], riffSize)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], SampleRate)
	binary.LittleEndian.PutUint32(b[28:], SampleRate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], dataSize)
	_, _ = binary.Encode(b[44:], binary.LittleEndian, a.Samples)
	return b
}

const frame = SampleRate / 50

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
