package player

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

const oggPageDuration = time.Millisecond * 20

type OggSampler struct {
	reader io.ReadCloser
	ogg    *oggreader.OggReader
	track  *webrtc.TrackLocalStaticSample
}

func NewOggSampler(reader io.ReadCloser, track *webrtc.TrackLocalStaticSample) (Sampler, error) {
	ogg, _, oggErr := oggreader.NewWith(reader)
	if oggErr != nil {
		panic(oggErr)
	}

	sampler := &OggSampler{
		reader,
		ogg,
		track,
	}

	go sampler.startReader()

	return sampler, nil
}

func (r *OggSampler) startReader() {
	var lastGranule uint64
	ticker := time.NewTicker(oggPageDuration)
	defer ticker.Stop()
	for ; true; <-ticker.C {
		pageData, pageHeader, oggErr := r.ogg.ParseNextPage()
		if errors.Is(oggErr, io.EOF) {
			fmt.Printf("All audio pages parsed and sent")
			os.Exit(0)
		}

		if oggErr != nil {
			panic(oggErr)
		}

		// The amount of samples is the difference between the last and current timestamp
		sampleCount := float64(pageHeader.GranulePosition - lastGranule)
		lastGranule = pageHeader.GranulePosition
		sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

		if err := r.track.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); err != nil {
			fmt.Println("error writing sample to track : %w", err)
		}
	}

}
