package sfu

import (
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// OpusMixer mixes Opus RTP coming from a primary source with a fallback
// silent generator. This is useful when the primary source is using
// Opus DTX (does not transmit anything during silence). The mixer
// guarantees that a continuous stream of Opus RTP packets is produced
// by filling the gaps with Opus DTX frames.
//
// Typical usage:
//
//     mixer := NewOpusMixer(200) // 200-packet buffer
//
//     // Feed incoming RTP from the WebRTC track
//     go func() {
//         for pkt := range rtpFromTrack {
//             mixer.Push(pkt.Clone())
//         }
//     }()
//
//     // Consume the mixed stream – forward to writer, peer connection, etc.
//     for pkt := range mixer.Out() {
//         _ = handlePacket(pkt)
//     }
//
// Calling Close() will stop the internal goroutine and close the output channel.
//
// The mixer always produces 20 ms Opus frames (960 samples at 48 kHz).
// Sequence numbers, timestamps and SSRC are regenerated so the output
// stream is self-contained and gap-free.

type OpusMixer struct {
	clockRate        uint32
	samplesPerPacket uint32

	mu   sync.Mutex
	ssrc uint32
	seq  uint16
	ts   uint32

	// incoming actual RTP packets
	in chan *rtp.Packet
	// outgoing mixed RTP stream
	out chan *rtp.Packet

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewOpusMixer creates a mixer with the given buffer size for incoming packets.
//
// buffer should be big enough to absorb jitter from the network.
// If buffer <= 0 a default size of 200 is used.
func NewOpusMixer(buffer int) *OpusMixer {
	if buffer <= 0 {
		buffer = 200
	}
	m := &OpusMixer{
		clockRate:        48000,
		samplesPerPacket: 960, // 20 ms @ 48 kHz
		ssrc:             uint32(rand.IntN(1 << 32)),
		seq:              uint16(rand.IntN(1 << 16)),
		ts:               uint32(rand.IntN(1 << 32)),
		in:               make(chan *rtp.Packet, buffer),
		out:              make(chan *rtp.Packet, buffer),
		stop:             make(chan struct{}),
	}
	m.wg.Add(1)
	go m.run()
	return m
}

// Push adds an RTP packet coming from the primary Opus source.
// The caller MUST clone the packet first if it intends to retain ownership.
func (m *OpusMixer) Push(pkt *rtp.Packet) {
	select {
	case m.in <- pkt:
	default:
		// Drop if buffer is full.
	}
}

// Out returns a read-only channel that delivers the mixed RTP packets.
func (m *OpusMixer) Out() <-chan *rtp.Packet {
	return m.out
}

// Close stops the mixer and closes the Out channel.
func (m *OpusMixer) Close() {
	close(m.stop)
	m.wg.Wait()
	close(m.out)
}

func (m *OpusMixer) nextHeader() rtp.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	hdr := rtp.Header{
		Version:        2,
		PayloadType:    111, // Dynamic PT for Opus used by WebRTC
		SequenceNumber: m.seq,
		Timestamp:      m.ts,
		SSRC:           m.ssrc,
	}
	m.seq++
	m.ts += m.samplesPerPacket
	return hdr
}

func (m *OpusMixer) run() {
	defer m.wg.Done()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			var payload []byte

			// Prefer an actual audio packet if available in this 20 ms slot.
			select {
			case pkt := <-m.in:
				payload = pkt.Payload
			default:
				// Opus DTX silence frame (RFC 6716 §3.1.3)
				payload = []byte{0xF8, 0xFF, 0xFE}
			}

			mixedPkt := &rtp.Packet{
				Header:  m.nextHeader(),
				Payload: payload,
			}

			// Non-blocking send – drop if downstream is congested.
			select {
			case m.out <- mixedPkt:
			default:
			}
		}
	}
}
