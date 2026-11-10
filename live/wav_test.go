package live

import (
	stdcontext "context"
	"encoding/binary"
	"errors"
	"net/http"
	"testing"
)

// TestPCMToWAVLayout pins every byte of the canonical header: RIFF, fmt
// chunk, rate, channels, bits, data chunk, then the samples verbatim.
func TestPCMToWAVLayout(t *testing.T) {
	pcm := []byte{0x01, 0x00, 0xFF, 0x7F, 0x00, 0x80, 0x34, 0x12} // 2 stereo frames
	wav, err := PCMToWAV(pcm, 16000, 2)
	if err != nil {
		t.Fatalf("PCMToWAV: %v", err)
	}
	if len(wav) != 44+len(pcm) {
		t.Fatalf("len = %d, want %d", len(wav), 44+len(pcm))
	}
	str := func(off, n int) string { return string(wav[off : off+n]) }
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(wav[off:]) }
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(wav[off:]) }

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"RIFF tag", str(0, 4), "RIFF"},
		{"RIFF size", u32(4), uint32(36 + len(pcm))},
		{"WAVE tag", str(8, 4), "WAVE"},
		{"fmt tag", str(12, 4), "fmt "},
		{"fmt chunk size", u32(16), uint32(16)},
		{"format tag (PCM)", u16(20), uint16(1)},
		{"channels", u16(22), uint16(2)},
		{"sample rate", u32(24), uint32(16000)},
		{"byte rate", u32(28), uint32(16000 * 2 * 2)},
		{"block align", u16(32), uint16(4)},
		{"bits per sample", u16(34), uint16(16)},
		{"data tag", str(36, 4), "data"},
		{"data size", u32(40), uint32(len(pcm))},
		{"samples", string(wav[44:]), string(pcm)},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

func TestPCMToWAVEmptyClip(t *testing.T) {
	wav, err := PCMToWAV(nil, 24000, 1)
	if err != nil {
		t.Fatalf("PCMToWAV(empty): %v", err)
	}
	if len(wav) != 44 || binary.LittleEndian.Uint32(wav[40:]) != 0 || binary.LittleEndian.Uint32(wav[4:]) != 36 {
		t.Fatalf("empty clip header = % x", wav)
	}
}

func TestPCMToWAVRejectsBadInput(t *testing.T) {
	tests := []struct {
		name     string
		pcm      []byte
		rate     int
		channels int
	}{
		{"zero rate", []byte{0, 0}, 0, 1},
		{"negative rate", []byte{0, 0}, -1, 1},
		{"zero channels", []byte{0, 0}, 16000, 0},
		{"too many channels", []byte{0, 0}, 16000, 70000},
		{"odd byte count mono", []byte{0, 0, 0}, 16000, 1},
		{"partial stereo frame", []byte{0, 0, 0, 0, 0, 0}, 16000, 2},
		{"byte rate overflow", make([]byte, 2), 1 << 30, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PCMToWAV(tc.pcm, tc.rate, tc.channels); !errors.Is(err, ErrInvalidPCM) {
				t.Fatalf("err = %v, want ErrInvalidPCM", err)
			}
		})
	}
}

func TestRawPCMFormat(t *testing.T) {
	tests := []struct {
		mime           string
		defRate, defCh int
		wantRate       int
		wantCh         int
		wantOK         bool
	}{
		{"audio/pcm", 0, 0, 24000, 1, true},
		{"audio/PCM", 0, 0, 24000, 1, true},
		{"audio/pcm", 8000, 2, 8000, 2, true},
		{"audio/L16;rate=16000;channels=1", 0, 0, 16000, 1, true},
		{"audio/L16; rate=44100; channels=2", 8000, 1, 44100, 2, true},
		{"audio/L16;rate=bogus", 12000, 1, 12000, 1, true},
		{"audio/L16;rate=-5;channels=0", 0, 0, 24000, 1, true},
		{"audio/pcm;rate=16000;;", 0, 0, 24000, 1, true}, // malformed parameters: still raw PCM, defaults apply
		{"audio/wav", 0, 0, 0, 0, false},
		{"audio/mpeg", 0, 0, 0, 0, false},
		{"", 0, 0, 0, 0, false},
	}
	for _, tc := range tests {
		rate, ch, ok := rawPCMFormat(tc.mime, tc.defRate, tc.defCh)
		if ok != tc.wantOK || rate != tc.wantRate || ch != tc.wantCh {
			t.Errorf("rawPCMFormat(%q, %d, %d) = (%d, %d, %v), want (%d, %d, %v)",
				tc.mime, tc.defRate, tc.defCh, rate, ch, ok, tc.wantRate, tc.wantCh, tc.wantOK)
		}
	}
}

// TestOpenAITranscriberWrapsRawPCM: raw PCM goes up as a playable WAV named
// speech.wav, with rate and channels from the mime parameters or the
// transcriber's fallbacks; other media types pass through untouched.
func TestOpenAITranscriberWrapsRawPCM(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	tests := []struct {
		name     string
		tr       OpenAITranscriber
		mime     string
		wantRate uint32
		wantCh   uint16
	}{
		{"mime parameters", OpenAITranscriber{}, "audio/L16;rate=16000;channels=2", 16000, 2},
		{"fields fallback", OpenAITranscriber{PCMSampleRate: 8000, PCMChannels: 2}, "audio/pcm", 8000, 2},
		{"package defaults", OpenAITranscriber{}, "audio/pcm", 24000, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newTranscriptionFake(t, http.StatusOK, "ok")
			tr := tc.tr
			tr.APIKey, tr.BaseURL = "k", fake.srv.URL
			if _, err := tr.Transcribe(stdcontext.Background(), pcm, tc.mime); err != nil {
				t.Fatalf("transcribe: %v", err)
			}
			if fake.gotFilename != "speech.wav" || fake.gotFileType != "audio/wav" {
				t.Fatalf("upload = %q (%s), want speech.wav (audio/wav)", fake.gotFilename, fake.gotFileType)
			}
			wav := fake.gotAudio
			if len(wav) != 44+len(pcm) || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
				t.Fatalf("upload is not a WAV: % x", wav)
			}
			if got := binary.LittleEndian.Uint32(wav[24:]); got != tc.wantRate {
				t.Errorf("sample rate = %d, want %d", got, tc.wantRate)
			}
			if got := binary.LittleEndian.Uint16(wav[22:]); got != tc.wantCh {
				t.Errorf("channels = %d, want %d", got, tc.wantCh)
			}
			if string(wav[44:]) != string(pcm) {
				t.Errorf("samples altered: % x", wav[44:])
			}
		})
	}
}

func TestOpenAITranscriberRawPCMBadFrames(t *testing.T) {
	fake := newTranscriptionFake(t, http.StatusOK, "ok")
	tr := &OpenAITranscriber{APIKey: "k", BaseURL: fake.srv.URL}
	_, err := tr.Transcribe(stdcontext.Background(), []byte{1, 2, 3}, "audio/pcm")
	if !errors.Is(err, ErrInvalidPCM) {
		t.Fatalf("err = %v, want ErrInvalidPCM", err)
	}
	if fake.gotAudio != nil {
		t.Fatal("a request went out despite the invalid clip")
	}
}
