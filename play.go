package sfu

import (
	"fmt"

	"github.com/pion/webrtc/v4"
	"github.com/samespace/sfu/player"
)

func (c *Client) GetPlayer(mimeType string) (*player.AudioPlayer, error) {
	t, err := webrtc.NewTrackLocalStaticSample(
		getCodecCapability(mimeType),
		"audio",
		"audio-player",
	)
	if err != nil {
		return nil, fmt.Errorf("error creating rtp track : %w", err)
	}

	if _, err = c.peerConnection.AddTrack(t); err != nil {
		return nil, fmt.Errorf("error adding track to peer connection : %w", err)
	}

	return player.NewAudioPlayer(t), nil
}
