package dictation

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func tone(n int, quiet func(i int) bool) Audio {
	s := make([]int16, n)
	for i := range s {
		if quiet != nil && quiet(i) {
			continue
		}
		if i/20%2 == 0 {
			s[i] = 8000
		} else {
			s[i] = -8000
		}
	}
	return Audio{Samples: s}
}

func TestWAVRoundTrip(t *testing.T) {
	a := Audio{Samples: []int16{0, 1, -1, 32767, -32768}}
	got, err := ParseWAV(a.WAV())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != len(a.Samples) {
		t.Fatalf("samples = %v", got.Samples)
	}
	for i := range a.Samples {
		if got.Samples[i] != a.Samples[i] {
			t.Fatalf("samples = %v, want %v", got.Samples, a.Samples)
		}
	}
	if d := tone(SampleRate*3/2, nil).Duration(); d != 1500*time.Millisecond {
		t.Fatalf("duration = %v", d)
	}
}

func TestParseWAVSkipsOtherChunks(t *testing.T) {
	wav := Audio{Samples: []int16{5, 6}}.WAV()
	const list = "LIST\x03\x00\x00\x00abc\x00"
	b := append(append(append([]byte{}, wav[:36]...), list...), wav[36:]...)
	binary.LittleEndian.PutUint32(b[4:], binary.LittleEndian.Uint32(wav[4:])+uint32(len(list)))
	got, err := ParseWAV(b)
	if err != nil || len(got.Samples) != 2 || got.Samples[1] != 6 {
		t.Fatalf("got %v, %v", got.Samples, err)
	}
}

func TestParseWAVRefuses(t *testing.T) {
	good := Audio{Samples: []int16{1, 2, 3}}.WAV()
	with := func(at int, v any) []byte {
		b := append([]byte{}, good...)
		if _, err := binary.Encode(b[at:], binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, tc := range map[string]struct {
		b    []byte
		want error
	}{
		"not RIFF":   {[]byte("hello world, not a wav"), errNotWAV},
		"stereo":     {with(22, uint16(2)), errNotWAV},
		"44.1 kHz":   {with(24, uint32(44100)), errNotWAV},
		"8-bit":      {with(34, uint16(8)), errNotWAV},
		"float":      {with(20, uint16(3)), errNotWAV},
		"no fmt":     {append([]byte("RIFF\x00\x00\x00\x00WAVE"), good[36:]...), errNotWAV},
		"truncated":  {good[:30], errTruncated},
		"no data":    {good[:36], errTruncated},
		"cut data":   {good[:len(good)-2], errTruncated},
		"too long":   {tone(maxSamples+1, nil).WAV(), errTooLong},
		"empty file": {nil, errNotWAV},
	} {
		if _, err := ParseWAV(tc.b); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	streamed := with(40, uint32(0xFFFFFFFF))
	if a, err := ParseWAV(streamed); err != nil || len(a.Samples) != 3 {
		t.Fatalf("placeholder size: %v, %v", a.Samples, err)
	}
	if _, err := ParseWAV(tone(SampleRate*601, nil).WAV()); err != nil {
		t.Fatalf("ten minutes: %v", err)
	}
}

func TestSplitCutsAtTheQuietestMoment(t *testing.T) {
	pause := func(at float64) func(int) bool {
		return func(i int) bool {
			s := float64(i) / SampleRate
			return s >= at && s < at+0.1
		}
	}
	p1, p2 := pause(25), pause(52)
	a := tone(SampleRate*70, func(i int) bool { return p1(i) || p2(i) })
	parts := a.Split(30 * time.Second)
	if len(parts) != 3 {
		t.Fatalf("%d parts", len(parts))
	}
	total := 0
	for i, p := range parts {
		total += len(p.Samples)
		if p.Duration() > 30*time.Second {
			t.Fatalf("part %d lasts %v", i, p.Duration())
		}
	}
	if total != len(a.Samples) {
		t.Fatalf("parts hold %d samples, want %d", total, len(a.Samples))
	}
	if d := parts[0].Duration(); d < 25*time.Second || d > 25100*time.Millisecond {
		t.Fatalf("first cut at %v, want in the pause at 25 s", d)
	}
	if d := parts[0].Duration() + parts[1].Duration(); d < 52*time.Second || d > 52100*time.Millisecond {
		t.Fatalf("second cut at %v, want in the pause at 52 s", d)
	}
	if got := tone(100, nil).Split(30 * time.Second); len(got) != 1 {
		t.Fatalf("short audio split in %d", len(got))
	}
}
