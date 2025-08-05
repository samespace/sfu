package sfu

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// OpusMixer handles mixing of Opus packets with DTX support
type OpusMixer struct {
	mu                sync.RWMutex
	silentPacketGen   chan *rtp.Packet
	webrtcPackets     chan *rtp.Packet
	outputChan        chan *rtp.Packet
	ctx               context.Context
	cancel            context.CancelFunc
	lastSequenceNum   uint16
	lastTimestamp     uint32
	ssrc              uint32
	payloadType       uint8
	sampleRate        uint32
	frameDuration     time.Duration
	maxSilentDuration time.Duration
	lastPacketTime    time.Time
}

// NewOpusMixer creates a new Opus mixer
func NewOpusMixer(ssrc uint32, payloadType uint8) *OpusMixer {
	ctx, cancel := context.WithCancel(context.Background())

	return &OpusMixer{
		silentPacketGen:   make(chan *rtp.Packet, 100),
		webrtcPackets:     make(chan *rtp.Packet, 100),
		outputChan:        make(chan *rtp.Packet, 100),
		ctx:               ctx,
		cancel:            cancel,
		ssrc:              ssrc,
		payloadType:       payloadType,
		sampleRate:        48000,                  // Opus standard sample rate
		frameDuration:     20 * time.Millisecond,  // 20ms frames
		maxSilentDuration: 100 * time.Millisecond, // Max silent gap before inserting comfort noise
		lastPacketTime:    time.Now(),
	}
}

// Start begins the mixing pipeline
func (om *OpusMixer) Start() {
	go om.silentPacketGenerator()
	go om.mixingLoop()
}

// Stop stops the mixing pipeline
func (om *OpusMixer) Stop() {
	om.cancel()
	close(om.silentPacketGen)
	close(om.webrtcPackets)
	close(om.outputChan)
}

// AddWebRTCPacket adds a packet from WebRTC peer
func (om *OpusMixer) AddWebRTCPacket(packet *rtp.Packet) {
	select {
	case om.webrtcPackets <- packet:
	case <-om.ctx.Done():
		return
	default:
		// Drop packet if channel is full
		log.Println("Warning: Dropping WebRTC packet due to full buffer")
	}
}

// GetOutputChan returns the output channel for mixed packets
func (om *OpusMixer) GetOutputChan() <-chan *rtp.Packet {
	return om.outputChan
}

// silentPacketGenerator generates silent Opus packets for DTX handling
func (om *OpusMixer) silentPacketGenerator() {
	ticker := time.NewTicker(om.frameDuration)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			om.mu.Lock()
			timeSinceLastPacket := time.Since(om.lastPacketTime)

			// Only generate silent packets if we haven't received real packets recently
			if timeSinceLastPacket > om.maxSilentDuration {
				silentPacket := om.createSilentPacket()
				select {
				case om.silentPacketGen <- silentPacket:
				default:
					// Drop if channel is full
				}
			}
			om.mu.Unlock()

		case <-om.ctx.Done():
			return
		}
	}
}

// createSilentPacket creates an Opus silent packet (DTX packet)
func (om *OpusMixer) createSilentPacket() *rtp.Packet {
	om.lastSequenceNum++
	om.lastTimestamp += uint32(float64(om.sampleRate) * om.frameDuration.Seconds())

	// Opus DTX packet - typically just a few bytes indicating silence
	// This is a simplified DTX packet - in practice, you might want to use
	// proper Opus comfort noise generation
	dtxPayload := []byte{0xF8, 0xFF, 0xFE} // Opus DTX packet format

	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Extension:      false,
			Marker:         false,
			PayloadType:    om.payloadType,
			SequenceNumber: om.lastSequenceNum,
			Timestamp:      om.lastTimestamp,
			SSRC:           om.ssrc,
		},
		Payload: dtxPayload,
	}
}

// mixingLoop handles the main mixing logic
func (om *OpusMixer) mixingLoop() {
	for {
		select {
		case webrtcPacket := <-om.webrtcPackets:
			if webrtcPacket != nil {
				om.handleWebRTCPacket(webrtcPacket)
			}

		case silentPacket := <-om.silentPacketGen:
			if silentPacket != nil {
				om.handleSilentPacket(silentPacket)
			}

		case <-om.ctx.Done():
			return
		}
	}
}

// handleWebRTCPacket processes packets from WebRTC peer
func (om *OpusMixer) handleWebRTCPacket(packet *rtp.Packet) {
	om.mu.Lock()
	defer om.mu.Unlock()

	// Update tracking information
	om.lastSequenceNum = packet.SequenceNumber
	om.lastTimestamp = packet.Timestamp
	om.lastPacketTime = time.Now()

	// Check if this is a DTX packet from the peer
	if om.isOpusDTXPacket(packet.Payload) {
		log.Println("Received DTX packet from peer")
		// You might want to handle peer DTX differently
		return
	}

	// Forward the real audio packet
	select {
	case om.outputChan <- packet:
	case <-om.ctx.Done():
		return
	default:
		log.Println("Warning: Dropping output packet due to full buffer")
	}
}

// handleSilentPacket processes generated silent packets
func (om *OpusMixer) handleSilentPacket(packet *rtp.Packet) {
	om.mu.Lock()
	defer om.mu.Unlock()

	// Only send silent packets if we haven't sent anything recently
	timeSinceLastPacket := time.Since(om.lastPacketTime)
	if timeSinceLastPacket > om.maxSilentDuration {
		select {
		case om.outputChan <- packet:
			// Update lastPacketTime when we successfully send a silent packet
			// This prevents infinite generation of silent packets
			om.lastPacketTime = time.Now()
		case <-om.ctx.Done():
			return
		default:
			// Drop if output is full
		}
	}
}

// isOpusDTXPacket checks if the payload is an Opus DTX packet
func (om *OpusMixer) isOpusDTXPacket(payload []byte) bool {
	if len(payload) == 0 {
		return true // Empty payload is considered DTX
	}

	// Check for Opus DTX patterns
	// Opus DTX packets typically have specific TOC (Table of Contents) byte patterns
	if len(payload) >= 1 {
		toc := payload[0]
		// Check if it's a DTX packet (config 0-3 with no frames)
		return (toc&0xFC) == 0xF8 || len(payload) <= 3
	}

	return false
}

// WebRTCPeerManager manages the WebRTC peer connection
type WebRTCPeerManager struct {
	peerConnection *webrtc.PeerConnection
	mixer          *OpusMixer
}

// NewWebRTCPeerManager creates a new WebRTC peer manager
func NewWebRTCPeerManager(mixer *OpusMixer) (*WebRTCPeerManager, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create peer connection: %v", err)
	}

	manager := &WebRTCPeerManager{
		peerConnection: peerConnection,
		mixer:          mixer,
	}

	// Set up audio track handling
	if err := manager.setupAudioHandling(); err != nil {
		return nil, fmt.Errorf("failed to setup audio handling: %v", err)
	}

	return manager, nil
}

// setupAudioHandling configures audio track handling
func (wpm *WebRTCPeerManager) setupAudioHandling() error {
	// Add a transceiver for audio
	_, err := wpm.peerConnection.AddTransceiverFromKind(
		webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to add audio transceiver: %v", err)
	}

	// Handle incoming RTP packets
	wpm.peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Received track: %s, codec: %s", track.ID(), track.Codec().MimeType)

		if track.Codec().MimeType == "audio/opus" {
			go func() {
				for {
					packet, _, err := track.ReadRTP()
					if err != nil {
						log.Printf("Error reading RTP packet: %v", err)
						return
					}

					// Forward packet to mixer
					wpm.mixer.AddWebRTCPacket(packet)
				}
			}()
		}
	})

	return nil
}
