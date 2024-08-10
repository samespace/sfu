package player

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/pion/webrtc/v4"
)

type AudioPlayerState string
type PacketType string

const (
	PacketTypePCMA PacketType = "PCMA"
	PacketTypePCMU PacketType = "PCMU"
	PacketTypeOPUS PacketType = "OPUS"
)

const (
	AudioPlayerStatePlaying AudioPlayerState = "playing"
	AudioPlayerStatePaused  AudioPlayerState = "paused"
	AudioPlayerStateStopped AudioPlayerState = "stopped"
)

type AudioPlayer struct {
	track      *webrtc.TrackLocalStaticSample
	state      AudioPlayerState
	mu         sync.Mutex
	cancelFunc context.CancelFunc
	ctx        context.Context
	reader     Sampler
}

type Sampler interface{}

func NewAudioPlayer(t *webrtc.TrackLocalStaticSample) *AudioPlayer {
	ctx, cancle := context.WithCancel(context.Background())
	return &AudioPlayer{
		track:      t,
		state:      AudioPlayerStatePaused,
		mu:         sync.Mutex{},
		cancelFunc: cancle,
		ctx:        ctx,
	}
}

func (p *AudioPlayer) changeState(newState AudioPlayerState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = newState
}

func (p *AudioPlayer) PlayURL(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download audio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download audio, status code: %d", resp.StatusCode)
	}
	packetType, ok := getPacketType(url)
	if !ok {
		return fmt.Errorf("invalid file extension : %s", packetType)
	}
	return p.Play(resp.Body, packetType)
}

func (p *AudioPlayer) PlayFile(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	packetType, ok := getPacketType(filename)
	if !ok {
		return fmt.Errorf("invalid file extension : %s", packetType)
	}

	return p.Play(file, packetType)
}

func (p *AudioPlayer) Play(reader io.ReadCloser, packetType PacketType) error {
	fmt.Println("playing file : ", packetType)
	p.mu.Lock()
	if p.state == AudioPlayerStatePlaying {
		p.mu.Unlock()
		return fmt.Errorf("audio player is already playing")
	}
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelFunc = cancel
	p.ctx = ctx
	go func() {
		//defer reader.Close()
		err := p.playStream(reader, packetType)
		if err != nil {
			fmt.Printf("playStream error: %v\n", err)
		}
	}()
	return nil
}

func (p *AudioPlayer) Pause() {
	p.changeState(AudioPlayerStatePaused)
}

func (p *AudioPlayer) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != AudioPlayerStateStopped {
		if p.cancelFunc != nil {
			p.cancelFunc()
		}
		p.changeState(AudioPlayerStateStopped)
	}
}

func (p *AudioPlayer) playStream(reader io.ReadCloser, packetType PacketType) error {
	switch packetType {
	case PacketTypeOPUS:
		p.reader, _ = NewOggSampler(reader, p.track)
	}

	p.changeState(AudioPlayerStatePlaying)
	return nil
}

func getPacketType(url string) (PacketType, bool) {
	ext := filepath.Ext(url)
	switch ext {
	case ".opus":
		return PacketTypeOPUS, true
	case ".pcma":
		return PacketTypePCMA, true
	case ".pcmu":
		return PacketTypePCMU, true
	default:
		return PacketType(ext), false
	}
}
