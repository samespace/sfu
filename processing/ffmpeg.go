package processing

import (
	"fmt"
	"os/exec"
)

func opusToPcm(src, dst string) error {
	cmd := exec.Command(
		"ffmpeg",
		"-i", src,
		"-acodec", "pcm_s16le",
		"-ar", "48000",
		"-ac", "2",
		dst,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to convert Opus to PCM16: %w, output: %s", err, output)
	}
	return nil
}

func pcmToLPcm(audioType audioType, src, dst string) error {
	var format string
	switch audioType {
	case "pcmu":
		format = "mulaw"
	case "pcma":
		format = "alaw"
	default:
		return fmt.Errorf("unsupported audio type: %s", audioType)
	}

	cmd := exec.Command(
		"ffmpeg",
		"-f", format,
		"-ar", "8000",
		"-ac", "1",
		"-i", src,
		"-acodec", "pcm_s16le",
		dst,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("error running ffmpeg: %v, output: %s", err, output)
	}

	return nil
}

func addAudioSilence(in, out string, duration float32) error {
	dur := fmt.Sprintf("%.2f", duration/1000)
	var cmd *exec.Cmd

	if dur == "0.00" {
		cmd = exec.Command(
			"ffmpeg",
			"-i", in,
			out,
		)
	} else {
		cmd = exec.Command(
			"ffmpeg",
			"-f", "lavfi",
			"-t", dur,
			"-i", "anullsrc=r=8000:cl=mono",
			"-i", in,
			"-filter_complex", "[0][1]concat=n=2:v=0:a=1[a]",
			"-map", "[a]",
			out,
		)
	}

	output, err := cmd.CombinedOutput()

	if err != nil {
		fmt.Println(cmd)
		fmt.Println(string(output))
	}

	return err
}

func mixAudio(inputs []string, output string) error {
	var ffmpegArgs []string
	for _, input := range inputs {
		ffmpegArgs = append(ffmpegArgs, "-i", input)
	}

	ffmpegArgs = append(ffmpegArgs,
		"-filter_complex", fmt.Sprintf("amix=inputs=%d:duration=longest:dropout_transition=3", len(inputs)),
		output,
	)

	cmd := exec.Command("ffmpeg", ffmpegArgs...)

	_, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	return nil
}
