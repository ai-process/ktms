// Package audio transcodes the raw PCM emitted by Gemini TTS into the OGG/Opus
// container chat apps such as Telegram render as a voice message.
//
// It shells out to ffmpeg: there is no production-quality pure-Go Opus encoder,
// and CGO would break static builds.
package audio

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

type ffmpegMissingError struct{}

func (ffmpegMissingError) Error() string {
	return "ffmpeg binary not found on PATH"
}

// SkipToNextModel allows the router to fall back to alternatives (e.g. OpenAI TTS)
// when ffmpeg is missing in the current environment.
func (ffmpegMissingError) SkipToNextModel() bool {
	return true
}

// ErrFFmpegMissing is returned when the ffmpeg binary is not available on PATH.
var ErrFFmpegMissing error = ffmpegMissingError{}

// MinClipMillis is the minimum clip duration: some chat platforms reject sub-~1s clips
// as audio messages, so short clips are
// padded with trailing silence up to this length.
const MinClipMillis = 1500

// Available reports whether the ffmpeg binary can be found on PATH.
func Available() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// EncodePCMToOggOpus converts 16-bit little-endian signed mono PCM at the given
// sample rate into an OGG/Opus file suitable for voice-message playback.
func EncodePCMToOggOpus(ctx context.Context, pcm []byte, sampleRate int) ([]byte, error) {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le", "-ar", strconv.Itoa(sampleRate), "-ac", "1",
		"-i", "pipe:0",
		// Pad up to MinClipMillis (some platforms reject sub-1s audio); never truncates longer clips.
		"-af", fmt.Sprintf("apad=whole_dur=%.3f", float64(MinClipMillis)/1000.0),
		"-c:a", "libopus", "-b:a", "32k", "-vbr", "on", "-application", "voip",
		"-f", "ogg",
		"pipe:1",
	}
	out, err := run(ctx, args, pcm)
	if err != nil {
		return nil, fmt.Errorf("encode pcm to ogg/opus: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("encode pcm to ogg/opus: empty output")
	}
	return out, nil
}

// TripleOggWithGap returns an OGG/Opus that plays the input clip three times
// separated by gapMillis of silence, padded to MinClipMillis. Lengthens stored
// short clips (a single word/sound) so strict platforms accept them, without re-TTS.
func TripleOggWithGap(ctx context.Context, in []byte, gapMillis int) ([]byte, error) {
	if !Available() {
		return nil, ErrFFmpegMissing
	}
	if gapMillis < 0 {
		gapMillis = 0
	}
	// Split clip into 3 and gap into 2, concat clip,gap,clip,gap,clip; apad guards
	// pathologically short inputs.
	filter := fmt.Sprintf(
		"[0:a]aresample=48000,aformat=sample_fmts=s16:channel_layouts=mono,asplit=3[a0][a1][a2];"+
			"[1:a]aformat=sample_fmts=s16:channel_layouts=mono,asplit=2[s0][s1];"+
			"[a0][s0][a1][s1][a2]concat=n=5:v=0:a=1,apad=whole_dur=%.3f[o]",
		float64(MinClipMillis)/1000.0)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-f", "lavfi", "-t", fmt.Sprintf("%.3f", float64(gapMillis)/1000.0), "-i", "anullsrc=r=48000:cl=mono",
		"-filter_complex", filter,
		"-map", "[o]",
		"-c:a", "libopus", "-b:a", "32k", "-vbr", "on", "-application", "voip",
		"-f", "ogg",
		"pipe:1",
	}
	out, err := run(ctx, args, in)
	if err != nil {
		return nil, fmt.Errorf("triple ogg: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("triple ogg: empty output")
	}
	return out, nil
}

// PCMDurationMs returns the duration of 16-bit mono PCM at the given rate.
func PCMDurationMs(pcm []byte, sampleRate int) int {
	if sampleRate <= 0 {
		return 0
	}
	samples := len(pcm) / 2
	return int(int64(samples) * 1000 / int64(sampleRate))
}

func run(ctx context.Context, args []string, input []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, ErrFFmpegMissing
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}
