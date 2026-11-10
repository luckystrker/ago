package live

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"mime"
	"strconv"
)

// ErrInvalidPCM reports raw PCM that cannot be wrapped as WAV: a
// non-positive or oversized sample rate or channel count, a length that is
// not a whole number of frames, or more data than a WAV container holds.
var ErrInvalidPCM = errors.New("live: invalid raw PCM")

// Raw 16-bit PCM facts shared with OpenAI's "pcm" speech format: 24 kHz mono.
const (
	defaultPCMRate     = 24000
	defaultPCMChannels = 1
	pcmSampleWidth     = 2 // bytes; the pipeline carries 16-bit samples only
	wavHeaderLen       = 44
)

// PCMToWAV wraps raw little-endian 16-bit PCM in a canonical 44-byte-header
// WAV container: RIFF/WAVE, a PCM "fmt " chunk, then the "data" chunk (Python
// parity: stt.py voice_to_wav_buffer). Upload-style STT endpoints infer the
// codec from the container, so bare samples are unplayable to them.
//
// AGO-DEVIATION: a length that is not a whole number of frames is an
// ErrInvalidPCM here; Python's wave module writes the trailing partial frame
// and leaves a header that disagrees with the data.
func PCMToWAV(pcm []byte, sampleRate, channels int) ([]byte, error) {
	if sampleRate <= 0 || sampleRate > math.MaxInt32 {
		return nil, fmt.Errorf("%w: sample rate %d", ErrInvalidPCM, sampleRate)
	}
	if channels <= 0 || channels > math.MaxUint16 {
		return nil, fmt.Errorf("%w: channel count %d", ErrInvalidPCM, channels)
	}
	blockAlign := channels * pcmSampleWidth
	if len(pcm)%blockAlign != 0 {
		return nil, fmt.Errorf("%w: %d bytes is not a whole number of %d-byte frames", ErrInvalidPCM, len(pcm), blockAlign)
	}
	// The RIFF size field is a uint32 covering everything after itself.
	if uint64(len(pcm)) > math.MaxUint32-(wavHeaderLen-8) {
		return nil, fmt.Errorf("%w: %d bytes exceeds the WAV size limit", ErrInvalidPCM, len(pcm))
	}
	byteRate := uint64(sampleRate) * uint64(blockAlign)
	if byteRate > math.MaxUint32 {
		return nil, fmt.Errorf("%w: byte rate %d overflows", ErrInvalidPCM, byteRate)
	}

	out := make([]byte, wavHeaderLen, wavHeaderLen+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(wavHeaderLen-8+len(pcm)))
	copy(out[8:], "WAVE")
	copy(out[12:], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16) // fmt chunk size for plain PCM
	binary.LittleEndian.PutUint16(out[20:], 1)  // format tag: PCM
	binary.LittleEndian.PutUint16(out[22:], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(out[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:], pcmSampleWidth*8) // bits per sample
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	return append(out, pcm...), nil
}

// rawPCMFormat reports whether mimeType names headerless PCM ("audio/pcm",
// or RFC 3551's "audio/L16") and, if so, its sample rate and channel count
// from the "rate" and "channels" media-type parameters, falling back to
// defRate/defChannels (then 24 kHz mono) when a parameter is absent or
// malformed.
func rawPCMFormat(mimeType string, defRate, defChannels int) (rate, channels int, ok bool) {
	base, params, err := mime.ParseMediaType(mimeType)
	if err != nil {
		// An unparsable parameter list must not hide a bare "audio/pcm".
		base, params = baseMimeType(mimeType), nil
	}
	if base != "audio/pcm" && base != "audio/l16" {
		return 0, 0, false
	}
	rate, channels = defRate, defChannels
	if rate <= 0 {
		rate = defaultPCMRate
	}
	if channels <= 0 {
		channels = defaultPCMChannels
	}
	if n, err := strconv.Atoi(params["rate"]); err == nil && n > 0 {
		rate = n
	}
	if n, err := strconv.Atoi(params["channels"]); err == nil && n > 0 {
		channels = n
	}
	return rate, channels, true
}
