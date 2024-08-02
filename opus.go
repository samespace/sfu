package sfu

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

type OpusRecorder struct {
	fileName    string
	oggwriter   *oggwriter.OggWriter
	track       ITrack
	buff        bytes.Buffer
	cancelCtx   context.Context
	cancleFn    func()
	isRecording atomic.Bool
	packetChan  chan *rtp.Packet
}

func NewOpusRecorder(filePath string, track ITrack) (Recorder, error) {
	ctx, can := context.WithCancel(context.Background())
	rec := &OpusRecorder{
		fileName:    filePath,
		buff:        bytes.Buffer{},
		track:       track,
		cancelCtx:   ctx,
		cancleFn:    can,
		isRecording: atomic.Bool{},
		packetChan:  make(chan *rtp.Packet),
	}
	writer := bufio.NewWriter(&rec.buff)
	ogg, err := oggwriter.NewWith(writer, 8000, 2)
	if err != nil {
		return nil, err
	}
	rec.oggwriter = ogg
	return rec, nil
}

func (r *OpusRecorder) Start() error {
	if r.isRecording.Load() {
		return nil
	}
	r.isRecording.Store(true)
	go r.recordMedia()
	return nil
}

func (r *OpusRecorder) recordMedia() {
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
			fmt.Println("done")
			break recordLoop
		case packet := <-r.packetChan:
			fmt.Println(packet.SequenceNumber, packet.Header.Timestamp)
			if err := r.oggwriter.WriteRTP(packet); err != nil {
				fmt.Println("err recording packet: ", err)
			}
		}
	}
}

func (r *OpusRecorder) Stop() error {
	if !r.isRecording.Load() {
		return nil
	}
	r.cancleFn()
	return nil
}

func (r *OpusRecorder) Pause() error {
	return fmt.Errorf("not implemented")
}

func (r *OpusRecorder) Contiune() error {
	return fmt.Errorf("not implemented")
}

func (r *OpusRecorder) Close() error {
	r.cancleFn()
	return r.oggwriter.Close()
}
