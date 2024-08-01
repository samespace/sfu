package sfu

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

type rtpRecorder struct {
	fileName    string
	trackId     string
	track       ITrack
	cancelCtx   context.Context
	isRecording atomic.Bool
	cancleFn    func()
	packetChan  chan *rtp.Packet
}

func newRTPRecorder(fname string, tId string, track ITrack) *rtpRecorder {
	ctx, can := context.WithCancel(context.Background())

	return &rtpRecorder{
		fileName:    fname,
		trackId:     tId,
		track:       track,
		cancelCtx:   ctx,
		cancleFn:    can,
		isRecording: atomic.Bool{},
		packetChan:  make(chan *rtp.Packet),
	}
}

func (r *rtpRecorder) startRecording() {
	if r.isRecording.Load() {
		return
	}
	r.isRecording.Store(true)
	go r.recordMedia()
}

func (r *rtpRecorder) stopRecording() {
	if !r.isRecording.Load() {
		return
	}
	r.cancleFn()
}

func (r *rtpRecorder) recordMedia() {
	t := r.track

	t.OnRead(func(p *rtp.Packet, _ QualityLevel) {
		packetCopy := *p // Create a copy of the packet
		r.packetChan <- &packetCopy
	})

	defer func() {
		r.stopRecording()
	}()

	switch t.MimeType() {
	case "audio/opus":
		r.recordOpus()
	}
}

func (r *rtpRecorder) recordOpus() {
	oggwriter, err := oggwriter.New(r.fileName, 48000, 2)
	if err != nil {
		fmt.Println(err)
		return
	}

	defer func() {
		oggwriter.Close()
	}()

recordLoop:
	for {
		select {
		case <-r.cancelCtx.Done():
			fmt.Println("done")
			break recordLoop
		case packet := <-r.packetChan:
			if err := oggwriter.WriteRTP(packet); err != nil {
				fmt.Println("err recording packet: ", err)
			}
		}
	}
}
