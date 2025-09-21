package sfu

import (
	"strings"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"golang.org/x/exp/slices"
)

var (
	// Audio codecs optimized for voice communication
	audioCodecs = []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     "audio/red",
				ClockRate:    48000,
				Channels:     2,
				SDPFmtpLine:  "111/111",
				RTCPFeedback: nil,
			},
			PayloadType: 63,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeOpus,
				ClockRate:    48000,
				Channels:     2,
				SDPFmtpLine:  "minptime=10;useinbandfec=1",
				RTCPFeedback: nil,
			},
			PayloadType: 111,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeG722,
				ClockRate:    8000,
				Channels:     0,
				SDPFmtpLine:  "",
				RTCPFeedback: nil,
			},
			PayloadType: rtp.PayloadTypeG722,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypePCMU,
				ClockRate:    8000,
				Channels:     0,
				SDPFmtpLine:  "",
				RTCPFeedback: nil,
			},
			PayloadType: rtp.PayloadTypePCMU,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypePCMA,
				ClockRate:    8000,
				Channels:     0,
				SDPFmtpLine:  "",
				RTCPFeedback: nil,
			},
			PayloadType: rtp.PayloadTypePCMA,
		},
	}

	// Opus silence frame for muting/hold functionality
	OpusSilenceFrame = []byte{
		0xf8, 0xff, 0xfe, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

func RegisterCodecs(m *webrtc.MediaEngine, codecs []string) error {
	errors := []error{}

	// Only register audio codecs for voice-only SFU
	for _, codec := range audioCodecs {
		if slices.Contains(codecs, codec.MimeType) {
			if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
				errors = append(errors, err)
			}
		}
	}

	return FlattenErrors(errors)
}

func RegisterDefaultCodecs(m *webrtc.MediaEngine) error {
	// Register only audio codecs for voice-only SFU
	for _, codec := range audioCodecs {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	return nil
}

// Audio-only payloader for voice communication
func PayloaderForCodec(codec webrtc.RTPCodecCapability) (rtp.Payloader, error) {
	switch strings.ToLower(codec.MimeType) {
	case strings.ToLower(webrtc.MimeTypeOpus):
		return &codecs.OpusPayloader{}, nil
	case strings.ToLower(webrtc.MimeTypeG722):
		return &codecs.G722Payloader{}, nil
	case strings.ToLower(webrtc.MimeTypePCMU), strings.ToLower(webrtc.MimeTypePCMA):
		return &codecs.G711Payloader{}, nil
	default:
		return nil, webrtc.ErrNoPayloaderForCodec
	}
}

func SendMediaSamples(p rtp.Packetizer, sequencer rtp.Sequencer, localRTP *webrtc.TrackLocalStaticRTP, sample media.Sample) error {
	clockRate := localRTP.Codec().ClockRate

	// skip packets by the number of previously dropped packets
	for i := uint16(0); i < sample.PrevDroppedPackets; i++ {
		sequencer.NextSequenceNumber()
	}

	samples := uint32(sample.Duration.Seconds()) * clockRate
	if sample.PrevDroppedPackets > 0 {
		p.SkipSamples(samples * uint32(sample.PrevDroppedPackets))
	}

	packets := p.Packetize(sample.Data, samples)

	writeErrs := []error{}

	// Audio frame rate: 50 fps (20ms intervals) for optimal audio quality
	frameDuration := time.Duration(20) * time.Millisecond

	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	for _, p := range packets {
		if err := localRTP.WriteRTP(p); err != nil {
			writeErrs = append(writeErrs, err)
		}
		<-ticker.C
	}

	return FlattenErrors(writeErrs)
}

func getPayloadType(mimeType string) webrtc.PayloadType {
	for _, codec := range audioCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec.PayloadType
		}
	}
	return 0
}

func getRTPParameters(mimeType string) webrtc.RTPCodecParameters {
	for _, codec := range audioCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec
		}
	}
	return webrtc.RTPCodecParameters{}
}

func getCodecCapability(mimeType string) webrtc.RTPCodecCapability {
	for _, codec := range audioCodecs {
		if codec.RTPCodecCapability.MimeType == mimeType {
			return codec.RTPCodecCapability
		}
	}
	return webrtc.RTPCodecCapability{}
}
