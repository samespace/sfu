package sfu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/inlivedev/sfu/proto/speechanalysis"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// SpeechAnalysisConfig holds per-recording speech analysis settings.
type SpeechAnalysisConfig struct {
	Enable      bool
	CallbackURL string
}

// readPCMFile reads raw little-endian int16 PCM samples from a file.
func readPCMFile(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	samples := make([]int16, len(data)/2)
	for i := 0; i < len(samples); i++ {
		samples[i] = int16(data[2*i]) | int16(data[2*i+1])<<8
	}
	return samples, nil
}

// resample48kTo16k downsamples 48kHz mono PCM to 16kHz by averaging every 3 consecutive samples.
func resample48kTo16k(samples []int16) []int16 {
	const ratio = 3
	outLen := len(samples) / ratio
	out := make([]int16, outLen)
	for i := 0; i < outLen; i++ {
		sum := int32(samples[i*ratio]) + int32(samples[i*ratio+1]) + int32(samples[i*ratio+2])
		out[i] = int16(sum / ratio)
	}
	return out
}

// interleaveStereo merges two mono channels into interleaved stereo (L, R, L, R, ...).
// Zero-pads the shorter channel if lengths differ.
func interleaveStereo(left, right []int16) []int16 {
	maxLen := len(left)
	if len(right) > maxLen {
		maxLen = len(right)
	}
	stereo := make([]int16, maxLen*2)
	for i := 0; i < maxLen; i++ {
		if i < len(left) {
			stereo[2*i] = left[i]
		}
		if i < len(right) {
			stereo[2*i+1] = right[i]
		}
	}
	return stereo
}

// samplesToBytes converts int16 samples to a little-endian byte slice.
func samplesToBytes(samples []int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, v := range samples {
		buf[2*i] = byte(v)
		buf[2*i+1] = byte(v >> 8)
	}
	return buf
}

// streamSpeechAnalysis reads PCM files, resamples to 16kHz stereo, and streams to gRPC.
func (r *Room) streamSpeechAnalysis(basePath, id string, oneExists, twoExists bool, callbackURL string) {
	grpcAddr := r.sfu.speechAnalysisAddr
	if grpcAddr == "" {
		return
	}

	var left, right []int16

	if oneExists {
		raw, err := readPCMFile(filepath.Join(basePath, id, "one.pcm"))
		if err != nil {
			fmt.Printf("speech analysis: error reading one.pcm: %v\n", err)
			return
		}
		left = resample48kTo16k(raw)
	}

	if twoExists {
		raw, err := readPCMFile(filepath.Join(basePath, id, "two.pcm"))
		if err != nil {
			fmt.Printf("speech analysis: error reading two.pcm: %v\n", err)
			return
		}
		right = resample48kTo16k(raw)
	}

	if left == nil && right == nil {
		fmt.Printf("speech analysis: no audio channels for recording %s\n", id)
		return
	}

	// Duplicate single channel to both sides if only one exists
	if left == nil {
		left = right
	}
	if right == nil {
		right = left
	}

	stereo := interleaveStereo(left, right)
	stereoBytes := samplesToBytes(stereo)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Printf("speech analysis: failed to connect to gRPC server %s: %v\n", grpcAddr, err)
		return
	}
	defer conn.Close()

	client := speechanalysis.NewSpeechAnalysisClient(conn)
	stream, err := client.StreamSentiment(ctx)
	if err != nil {
		fmt.Printf("speech analysis: failed to create stream: %v\n", err)
		return
	}

	const chunkSize = 32 * 1024
	chunkNumber := int32(0)
	totalBytes := len(stereoBytes)

	for offset := 0; offset < totalBytes; offset += chunkSize {
		end := offset + chunkSize
		if end > totalBytes {
			end = totalBytes
		}

		chunk := &speechanalysis.AudioChunk{
			AudioData:   stereoBytes[offset:end],
			ChunkNumber: chunkNumber,
			IsFinal:     end >= totalBytes,
		}

		if chunkNumber == 0 {
			chunk.CallbackUrl = callbackURL
		}

		if err := stream.Send(chunk); err != nil {
			fmt.Printf("speech analysis: error sending chunk %d: %v\n", chunkNumber, err)
			return
		}
		chunkNumber++
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		fmt.Printf("speech analysis: error receiving response: %v\n", err)
		return
	}

	fmt.Printf("speech analysis: completed for recording %s, status=%s, message=%s\n", id, resp.Status, resp.Message)
}
