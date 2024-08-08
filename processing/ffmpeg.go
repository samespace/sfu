package processing

import (
	"fmt"

	ffmpeg "github.com/u2takey/ffmpeg-go"
)

// Convert Opus to PCM16 format
func opusToPcm(src, dst string) error {
	err := ffmpeg.Input(src).
		Output(dst, ffmpeg.KwArgs{
			"acodec": "pcm_s16le", // Audio codec for PCM 16-bit little-endian
			"ar":     "48000",     // Sample rate 48kHz
			"ac":     "2",         // Number of audio channels (stereo)
		}).
		Run()
	if err != nil {
		return fmt.Errorf("failed to convert Opus to PCM16: %w", err)
	}
	return nil
}

// Convert μ-law or A-law to PCM16 format
func pcmToLPcm(audioType audioType, src, dst string) error {
	var format string
	switch audioType {
	case audioTypeULaw:
		format = "mulaw" // Input format is μ-law
	case audioTypeALaw:
		format = "alaw" // Input format is a-law
	default:
		return fmt.Errorf("unsupported audio type: %s", audioType)
	}

	err := ffmpeg.Input(src, ffmpeg.KwArgs{
		"f":  format, // Input format (μ-law or a-law)
		"ar": "8000", // Sample rate 8000 Hz
		"ac": "1",    // Number of audio channels (mono)
	}).
		Output(dst, ffmpeg.KwArgs{
			"acodec": "pcm_s16le", // Output codec is PCM 16-bit little-endian
		}).
		Run()
	if err != nil {
		return fmt.Errorf("failed to convert %s to PCM16: %w", audioType, err)
	}
	return nil
}

// Create a silence audio file of the specified duration (in seconds)
func createSilence(filePath string, duration float64) error {
	err := ffmpeg.Input("anullsrc=r=48000:cl=stereo", ffmpeg.KwArgs{
		"t": duration, // Duration of silence
		"f": "lavfi",
	}).
		Output(filePath, ffmpeg.KwArgs{
			"acodec": "pcm_s16le", // Audio codec for PCM 16-bit little-endian
			"ar":     "48000",     // Sample rate 48kHz
			"ac":     "2",         // Number of audio channels (stereo)
		}).
		Run()
	if err != nil {
		return fmt.Errorf("failed to create silence file %s: %w", filePath, err)
	}
	return nil
}
