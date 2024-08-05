package sfu

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/sopro-dev/sopro-core/audio"
	"github.com/sopro-dev/sopro-core/audio/formats/pcm"

	mulaw "github.com/sopro-dev/sopro-core/audio/formats/ulaw"
	"github.com/sopro-dev/sopro-core/audio/utils"
)

type PCMLaw string

const (
	PCMULaw PCMLaw = "ulaw"
	PCMALaw PCMLaw = "alaw"
)

type PCMRecorder struct {
	writer      io.Writer
	track       ITrack
	cancelCtx   context.Context
	cancelFn    func()
	isRecording atomic.Bool
	packetChan  chan *rtp.Packet
}

func NewPCMRecorder(payloadType PCMLaw, track ITrack, writer io.Writer) (Recorder, error) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &PCMRecorder{
		writer:      writer,
		track:       track,
		cancelCtx:   ctx,
		cancelFn:    cancel,
		isRecording: atomic.Bool{},
		packetChan:  make(chan *rtp.Packet),
	}

	return rec, nil
}

func (r *PCMRecorder) Start() error {
	if r.isRecording.Load() {
		return nil
	}
	r.isRecording.Store(true)
	go r.recordMedia()
	return nil
}

func (r *PCMRecorder) recordMedia() {
	t := r.track

	t.OnRead(func(p *rtp.Packet, _ QualityLevel) {
		packetCopy := *p // Create a copy of the packet
		r.packetChan <- &packetCopy
	})

	defer func() {
		r.Stop()
	}()

recordLoop:
	for {
		select {
		case <-r.cancelCtx.Done():
			break recordLoop
		case packet := <-r.packetChan:
			if _, err := r.writer.Write(packet.Payload); err != nil {
				fmt.Println("error recording packet: ", err)
			}
		}
	}
}

func (r *PCMRecorder) Stop() error {
	if !r.isRecording.Load() {
		return nil
	}
	r.cancelFn()
	return nil
}

func (r *PCMRecorder) Pause() error {
	return fmt.Errorf("not implemented")
}

func (r *PCMRecorder) Continue() error {
	return fmt.Errorf("not implemented")
}

func (r *PCMRecorder) Close() error {
	r.cancelFn()
	return nil
}

func main() {
	// read file on "internal/samples/sample.ul"
	f, err := os.Open("internal/samples/sample.ul")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	// copy all to an array
	inputData, err := io.ReadAll(f)
	if err != nil {
		panic(err)
	}

	audioInfo := audio.AudioInfo{
		SampleRate:  8000,
		Channels:    1,
		BitDepth:    8,
		FloatFormat: false,
		Verbose:     false,
	}

	transcoder := audio.NewTranscoder(&mulaw.MuLawFormat{}, &pcm.PCMFormat{})
	outputData, err := transcoder.Transcode(inputData, audioInfo)
	if err != nil {
		panic(err)
	}

	// Store the output data to a file...
	f, err = os.Create("output_converted.wav")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	// create headers
	headers := utils.GenerateWavHeadersWithConfig(&utils.WavHeader{
		Length:     uint32(len(outputData) + 44),
		WaveFormat: utils.WAVE_FORMAT_PCM,
		Channels:   1,
		SampleRate: 8000,
		BitDepth:   16,
		Verbose:    audioInfo.Verbose,
	})

	f.Write(headers)
	f.Seek(44, 0)
	f.Write(outputData)
}
