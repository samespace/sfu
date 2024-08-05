package sfu

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/pion/rtp"
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
