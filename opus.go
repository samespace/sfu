package sfu

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

type OpusRecorder struct {
	oggwriter   *oggwriter.OggWriter
	track       ITrack
	cancelCtx   context.Context
	cancelFn    context.CancelFunc
	isRecording atomic.Bool
	isPaused    atomic.Bool
	packetChan  chan *rtp.Packet
}

func NewOpusRecorder(track ITrack, writer *ChunkWriter) (Recorder, error) {
	ogg, err := oggwriter.NewWith(writer, 48000, 2) // Use a common sample rate for Opus
	if err != nil {
		return nil, fmt.Errorf("failed to create OggWriter: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := &OpusRecorder{
		track:       track,
		cancelCtx:   ctx,
		cancelFn:    cancel,
		isRecording: atomic.Bool{},
		isPaused:    atomic.Bool{},
		packetChan:  make(chan *rtp.Packet, 100), // Buffered channel to prevent blocking
		oggwriter:   ogg,
	}
	return rec, nil
}

func (r *OpusRecorder) Start() error {
	if !r.isRecording.CompareAndSwap(false, true) {
		return nil
	}
	go r.recordMedia()
	return nil
}

func (r *OpusRecorder) recordMedia() {
	defer r.Stop()

	r.track.OnRead(func(p *rtp.Packet, _ QualityLevel) {
		packetCopy := *p // Create a copy of the packet
		select {
		case r.packetChan <- &packetCopy:
		default:
			fmt.Println("packet channel is full, dropping packet")
		}
	})

	for {
		select {
		case <-r.cancelCtx.Done():
			return
		case packet := <-r.packetChan:
			if r.isPaused.Load() {
				blankPacket := r.createBlankOpusPacket(packet)
				if err := r.oggwriter.WriteRTP(blankPacket); err != nil {
					fmt.Println("error writing blank packet: ", err)
				}
			} else {
				if err := r.oggwriter.WriteRTP(packet); err != nil {
					fmt.Println("error recording packet: ", err)
				}
			}
		}
	}
}

func (r *OpusRecorder) Stop() error {
	if !r.isRecording.CompareAndSwap(true, false) {
		return nil
	}
	r.cancelFn()
	close(r.packetChan)
	return nil
}

func (r *OpusRecorder) Pause() error {
	if !r.isRecording.Load() {
		return fmt.Errorf("recorder is not running")
	}
	r.isPaused.Store(true)
	return nil
}

func (r *OpusRecorder) Continue() error {
	if !r.isRecording.Load() {
		return fmt.Errorf("recorder is not running")
	}
	r.isPaused.Store(false)
	return nil
}

func (r *OpusRecorder) Close() error {
	r.Stop()
	if err := r.oggwriter.Close(); err != nil {
		return fmt.Errorf("failed to close OggWriter: %w", err)
	}
	return nil
}

func (r *OpusRecorder) createBlankOpusPacket(original *rtp.Packet) *rtp.Packet {
	blankPacket := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Extension:      false,
			Marker:         false,
			PayloadType:    original.PayloadType,
			SequenceNumber: original.SequenceNumber,
			Timestamp:      original.Timestamp,
			SSRC:           original.SSRC,
		},
		Payload: []byte{0xF8, 0xFF, 0xFE}, // Opus silence payload
	}
	return blankPacket
}
