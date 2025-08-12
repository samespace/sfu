// recorder_sr_mixer.go
package sfu

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/pion/interceptor/pkg/jitterbuffer"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	opus "gopkg.in/hraban/opus.v2"
)

/////////////////////
// Configurable constants
/////////////////////

const (
	SampleRate       = 48000                                 // WebRTC default
	FrameDurationMS  = 20                                    // typical Opus frame 20ms
	FrameSamples     = (SampleRate * FrameDurationMS) / 1000 // 960
	ChannelsOut      = 2                                     // stereo output
	BytesPerSample   = 2                                     // int16
	DefaultBatchMS   = 100                                   // write to ffmpeg every 100ms
	DefaultSafetyMS  = 200                                   // wait this long before flushing (to allow late packets)
	DefaultBufferSec = 10                                    // sliding buffer size in seconds
)

/////////////////////
// Helper NTP <-> time helpers
/////////////////////

// NTP epoch starts at 1900; Unix epoch starts at 1970 -> offset seconds:
const ntpToUnixOffset = 2208988800 // seconds

// timeToNTP converts time.Time to RFC-1305 style NTP uint64 (seconds<<32 | fraction)
func timeToNTP(t time.Time) uint64 {
	secs := uint64(t.Unix() + ntpToUnixOffset)
	// fraction = (nsec * 2^32) / 1e9
	frac := (uint64(t.Nanosecond()) << 32) / 1_000_000_000
	return (secs << 32) | frac
}

// ntpToTime converts NTP uint64 to time.Time
func ntpToTime(ntp uint64) time.Time {
	secs := int64(ntp >> 32)
	frac := ntp & 0xffffffff
	nsec := int64((frac * 1_000_000_000) >> 32)
	unix := secs - ntpToUnixOffset
	return time.Unix(unix, nsec)
}

/////////////////////
// SR mapping storage
/////////////////////

type SRInfo struct {
	// mapping: RTPTime (uint32) at the instant of NTPTime (uint64)
	RTPTime uint32
	NTPTime uint64 // NTP fixed point (upper 32=seconds, lower 32=fraction)
	SeenAt  time.Time
	// Sticky base mapping captured from the very first SR for this SSRC
	baseRTP uint32
	baseNTP uint64
	baseSet bool
}

/////////////////////
// Decoded frame container
/////////////////////

type DecodedFrame struct {
	SSRC     uint32
	RTPTime  uint32
	Samples  []int16 // len == FrameSamples (mono)
	SamplesN int     // actual number of samples returned by decoder (<= FrameSamples)
	Channel  int     // assignment: 0=left,1=right,>=2=mix both
}

/////////////////////
// Track processor w/ jitter buffer
/////////////////////

type TrackProcessor struct {
	jb      *jitterbuffer.JitterBuffer
	decoder *opus.Decoder
	outCh   chan *DecodedFrame // send decoded frames to Mixer
	pool    *sync.Pool         // pool of []int16 slices
	quit    chan struct{}
	ssrc    uint32
	channel int
	// timeline state for generating synthetic silence aligned to RTP clock
	lastRTPTime uint32
	haveRTP     bool
}

func NewTrackProcessor(ssrc uint32, channel int, outCh chan *DecodedFrame, queueSize int) (*TrackProcessor, error) {
	dec, err := opus.NewDecoder(SampleRate, 1)
	if err != nil {
		return nil, fmt.Errorf("opus.NewDecoder: %w", err)
	}
	jb := jitterbuffer.New() // defaults; you can pass options (e.g., WithMinimumPacketCount)
	tp := &TrackProcessor{
		jb:      jb,
		decoder: dec,
		outCh:   outCh,
		ssrc:    ssrc,
		channel: channel,
		pool: &sync.Pool{
			New: func() interface{} {
				return make([]int16, FrameSamples)
			},
		},
		quit: make(chan struct{}),
	}
	go tp.playoutLoop()
	return tp, nil
}

func (t *TrackProcessor) Close() {
	close(t.quit)
	t.jb.Clear(true)
}

func (t *TrackProcessor) ReadCallback(pkt *rtp.Packet) {
	// push into jitter buffer; jitter buffer may drop if full
	t.jb.Push(pkt)
}

func (t *TrackProcessor) playoutLoop() {
	ticker := time.NewTicker(time.Duration(FrameDurationMS) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-t.quit:
			return
		case <-ticker.C:
			// Pop one RTP packet that is ready for playout.
			pkt, err := t.jb.Pop()
			if err != nil {
				// If still buffering or underflow, only emit silence after first real RTP seen
				if !t.haveRTP {
					continue
				}
				buf := t.pool.Get().([]int16)
				for i := 0; i < FrameSamples; i++ {
					buf[i] = 0
				}
				df := &DecodedFrame{
					SSRC:     t.ssrc,
					RTPTime:  t.lastRTPTime,
					Samples:  buf,
					SamplesN: FrameSamples,
					Channel:  t.channel,
				}
				// advance expected RTP for next tick
				t.lastRTPTime += uint32(FrameSamples)
				// Non-blocking send to avoid wedging if mixer is slow
				select {
				case t.outCh <- df:
				default:
					t.pool.Put(buf)
				}
				continue
			}

			// decode pkt.Payload into pooled buffer
			buf := t.pool.Get().([]int16)
			n, err := t.decoder.Decode(pkt.Payload, buf)
			if err != nil {
				// decode error -> emit silence
				t.pool.Put(buf)
				sil := t.pool.Get().([]int16)
				for i := 0; i < FrameSamples; i++ {
					sil[i] = 0
				}
				df := &DecodedFrame{
					SSRC:     t.ssrc,
					RTPTime:  pkt.Timestamp,
					Samples:  sil,
					SamplesN: FrameSamples,
					Channel:  t.channel,
				}
				// initialize/advance RTP timeline reference
				t.haveRTP = true
				t.lastRTPTime = pkt.Timestamp + uint32(FrameSamples)
				select {
				case t.outCh <- df:
				default:
					t.pool.Put(sil)
				}
				continue
			}
			// pad if shorter than FrameSamples
			if n < FrameSamples {
				for i := n; i < FrameSamples; i++ {
					buf[i] = 0
				}
			}
			df := &DecodedFrame{
				SSRC:     t.ssrc,
				RTPTime:  pkt.Timestamp,
				Samples:  buf,
				SamplesN: FrameSamples,
				Channel:  t.channel,
			}
			// initialize/advance RTP timeline reference
			t.haveRTP = true
			t.lastRTPTime = pkt.Timestamp + uint32(FrameSamples)
			select {
			case t.outCh <- df:
			default:
				// mixer is backed up; drop frame and return buffer
				t.pool.Put(buf)
			}
		}
	}
}

/////////////////////
// Mixer with SR alignment and write batching
/////////////////////

type Mixer struct {
	// mixing timeline buffer (interleaved stereo int16)
	buffer        []int16
	bufferSamples int64 // per-channel sample count (length = bufferSamples * ChannelsOut)
	bufferStart   int64 // absolute sample index corresponding to buffer[0]/channels  (mixStart sample index)
	writeCursor   int64 // next sample index to flush to ffmpeg
	bufMu         sync.Mutex

	// SR mappings
	srMu sync.RWMutex
	sr   map[uint32]*SRInfo

	// incoming decoded frames
	incoming chan *DecodedFrame

	// ffmpeg stdin
	ffIn  io.WriteCloser
	ffCmd *exec.Cmd

	// Pools
	bytePool *sync.Pool

	// control
	ctx    context.Context
	cancel context.CancelFunc

	// params
	batchSamples   int64
	safetySamples  int64
	sampleRate     uint64
	bufferDuration time.Duration
	outFilename    string

	// NTP origin for absolute timeline mapping and highest mixed index
	originMu  sync.Mutex
	originNTP uint64
	originSet bool
	mixEnd    int64
}

func NewMixer(outputFile string, batchMS int, safetyMS int, bufferSec int) (*Mixer, error) {
	m := &Mixer{
		sr:             make(map[uint32]*SRInfo),
		incoming:       make(chan *DecodedFrame, 2048),
		sampleRate:     uint64(SampleRate),
		outFilename:    outputFile,
		batchSamples:   int64((SampleRate * batchMS) / 1000),
		safetySamples:  int64((SampleRate * safetyMS) / 1000),
		bufferSamples:  int64(SampleRate * bufferSec),
		bufferDuration: time.Duration(bufferSec) * time.Second,
	}

	// allocate interleaved buffer (stereo)
	m.buffer = make([]int16, m.bufferSamples*int64(ChannelsOut))
	m.bufferStart = 0
	m.writeCursor = 0

	// byte pool for conversion int16->[]byte
	m.bytePool = &sync.Pool{
		New: func() interface{} {
			// batch bytes for stereo: batchSamples * ChannelsOut * 2
			size := int(m.batchSamples) * ChannelsOut * BytesPerSample
			return make([]byte, size)
		},
	}

	// start ffmpeg
	cmd := exec.Command("ffmpeg",
		"-y",
		"-f", "s16le",
		"-ar", fmt.Sprintf("%d", SampleRate),
		"-ac", fmt.Sprintf("%d", ChannelsOut),
		"-i", "pipe:0",
		"-c:a", "aac",
		"-b:a", "64k",
		outputFile,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdin pipe: %w", err)
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	m.ffIn = stdin
	m.ffCmd = cmd

	m.ctx, m.cancel = context.WithCancel(context.Background())

	// start workers
	go m.frameProcessor()
	go m.flushLoop()
	return m, nil
}

func (m *Mixer) Close() error {
	fmt.Printf("Mixer closing: total samples written = %d, duration = %.3f seconds", m.writeCursor, float64(m.writeCursor)/float64(SampleRate))
	m.cancel()
	// close ffmpeg stdin so it can finalize file
	if m.ffIn != nil {
		_ = m.ffIn.Close()
	}
	// wait for ffmpeg exit
	if m.ffCmd != nil {
		_ = m.ffCmd.Wait()
	}
	return nil
}

/////////////////////
// SR update / lookup
/////////////////////

func (m *Mixer) UpdateSR(sr *rtcp.SenderReport) {
	m.srMu.Lock()
	defer m.srMu.Unlock()
	if existing, ok := m.sr[sr.SSRC]; ok {
		// Set base mapping once; subsequent SRs only update liveness
		if !existing.baseSet {
			existing.baseRTP = sr.RTPTime
			existing.baseNTP = sr.NTPTime
			existing.baseSet = true
			existing.RTPTime = sr.RTPTime
			existing.NTPTime = sr.NTPTime
			existing.SeenAt = time.Now()
			fmt.Printf("UpdateSR: SSRC=%d, RTPTime=%d, NTPTime=%d (base)\n", sr.SSRC, sr.RTPTime, sr.NTPTime)
		} else {
			existing.SeenAt = time.Now()
			fmt.Printf("UpdateSR: SSRC=%d (ignored, base mapping already set)\n", sr.SSRC)
		}
		return
	}
	info := &SRInfo{
		RTPTime: sr.RTPTime,
		NTPTime: sr.NTPTime,
		SeenAt:  time.Now(),
		baseRTP: sr.RTPTime,
		baseNTP: sr.NTPTime,
		baseSet: true,
	}
	fmt.Printf("UpdateSR: SSRC=%d, RTPTime=%d, NTPTime=%d (base)\n", sr.SSRC, sr.RTPTime, sr.NTPTime)
	m.sr[sr.SSRC] = info
}

func (m *Mixer) getSR(ssrc uint32) (*SRInfo, bool) {
	m.srMu.RLock()
	defer m.srMu.RUnlock()
	info, ok := m.sr[ssrc]
	return info, ok
}

/////////////////////
// Convert packet RTP timestamp -> pktNTP (uint64 fixed-point) using SR mapping
// pktNTP = sr.NTP + ((pktRTP - sr.RTP) << 32) / sampleRate
/////////////////////

func (m *Mixer) rtpToNTPUsingSR(ssrc uint32, pktRTP uint32) (uint64, bool) {
	info, ok := m.getSR(ssrc)
	if !ok || !info.baseSet {
		// Fallback: use current time (less accurate but prevents dropping frames)
		fmt.Printf("WARNING: Using fallback time for SSRC %d", ssrc)
		return timeToNTP(time.Now()), true
	}
	// Use sticky base mapping to avoid drift when new SRs arrive
	delta := uint32(pktRTP - info.baseRTP)
	deltaNTP := (uint64(delta) << 32) / m.sampleRate
	return info.baseNTP + deltaNTP, true
}

/////////////////////
// frame processing and placement
/////////////////////

// frameProcessor consumes decoded frames and places them into the timeline buffer.
func (m *Mixer) frameProcessor() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case df := <-m.incoming:
			// compute pktNTP using SR mapping
			pktNTP, ok := m.rtpToNTPUsingSR(df.SSRC, df.RTPTime)
			if !ok {
				// no SR mapping for this SSRC - dropping frame (for sample-accurate mode).
				fmt.Printf("WARNING: No SR mapping for SSRC %d, dropping frame (RTPTime: %d)", df.SSRC, df.RTPTime)
				// You could fallback to time.Now() mapping if you want approximate placement.
				// return buffer to pool (decoded frame owner should manage pool; here we just discard)
				// we assume TrackProcessor returns slices to pool itself when it detects drop. But
				// for safety, if df.Samples belongs to a track pool, calling close/return is necessary.
				continue
			}

			m.placeFrameAtNTP(df, pktNTP)
			// note: the df.Samples should be returned to the original pool by its owner.
			// In this design TrackProcessor gave ownership of the slice to Mixer; after copying
			// into our internal buffer we return it. We'll put it back into a generic pool.
			// To keep things simple the TrackProcessor and Mixer can share the same pool reference.
			// (In code above TrackProcessor used its own pool; in production you'd pass a common pool.)
			// We'll simply let GC handle it if pools are not shared; it's a tradeoff.
		}
	}
}

func clampInt32ToInt16(v int32) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

// placeFrameAtNTP places decoded mono frame into interleaved buffer at the exact sample offset
// corresponding to pktNTP (this NTP corresponds to the RTP timestamp of the first decoded sample).
func (m *Mixer) placeFrameAtNTP(df *DecodedFrame, pktNTP uint64) {
	// Lazy-init origin using first placed NTP
	m.originMu.Lock()
	if !m.originSet {
		m.originNTP = pktNTP
		m.originSet = true
	}
	origin := m.originNTP
	m.originMu.Unlock()

	// Compute absolute sample offset from origin using fixed-point arithmetic
	var sampleOffset int64
	if pktNTP >= origin {
		delta := pktNTP - origin
		sampleOffset = int64((delta * m.sampleRate) >> 32)
	} else {
		delta := origin - pktNTP
		sampleOffset = -int64((delta * m.sampleRate) >> 32)
	}

	m.bufMu.Lock()
	defer m.bufMu.Unlock()

	if m.writeCursor == 0 && m.bufferStart == 0 && m.mixEnd == 0 {
		m.bufferStart = 0
		m.mixEnd = 0
	}

	relStart := sampleOffset - m.bufferStart
	frameLen := int64(df.SamplesN)

	// Clip head if needed
	startInFrame := int64(0)
	if relStart < 0 {
		skip := -relStart
		if skip >= frameLen {
			return
		}
		startInFrame = skip
		relStart = 0
		// We consumed 'skip' samples from the frame head, so reduce the writable length accordingly
		frameLen -= skip
	}

	// Clip tail if exceeds buffer capacity
	if relStart+frameLen > m.bufferSamples {
		writable := m.bufferSamples - relStart
		if writable <= 0 {
			return
		}
		frameLen = writable
	}

	baseIdx := relStart * int64(ChannelsOut)
	endInFrame := startInFrame + frameLen
	// Ensure we never read past the provided sample slice
	maxSamples := int64(df.SamplesN)
	if endInFrame > maxSamples {
		endInFrame = maxSamples
	}
	chanMode := df.Channel

	for si, bi := startInFrame, baseIdx; si < endInFrame; si, bi = si+1, bi+int64(ChannelsOut) {
		sample := df.Samples[si]
		switch chanMode {
		case 0: // left only
			vL := int32(m.buffer[bi]) + int32(sample)
			m.buffer[bi] = clampInt32ToInt16(vL)
		case 1: // right only
			vR := int32(m.buffer[bi+1]) + int32(sample)
			m.buffer[bi+1] = clampInt32ToInt16(vR)
		default: // both
			vL := int32(m.buffer[bi]) + int32(sample)
			vR := int32(m.buffer[bi+1]) + int32(sample)
			m.buffer[bi] = clampInt32ToInt16(vL)
			m.buffer[bi+1] = clampInt32ToInt16(vR)
		}
	}

	absEnd := sampleOffset + int64(df.SamplesN)
	if absEnd > m.mixEnd {
		m.mixEnd = absEnd
	}
	fmt.Printf("Placed frame for SSRC %d: sampleOffset=%d, frameLen=%d, mixEnd=%d", df.SSRC, sampleOffset, frameLen, m.mixEnd)
}

/////////////////////
// Flush loop (write to ffmpeg)
/////////////////////

func (m *Mixer) flushLoop() {
	ticker := time.NewTicker(time.Duration(DefaultBatchMS) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// compute how many samples we can flush safely:
			// currentNTP := timeToNTP(time.Now())
			// latestAllowedSample := ((currentNTP - originNTP) * sampleRate >> 32) - safety
			// flush in batches of m.batchSamples
			m.tryFlush()
		}
	}
}

func (m *Mixer) tryFlush() {
	m.bufMu.Lock()
	defer m.bufMu.Unlock()

	for {
		pending := m.mixEnd - m.writeCursor
		if pending <= 0 {
			break
		}
		toFlush := m.batchSamples
		if toFlush > pending {
			toFlush = pending
		}
		if toFlush <= 0 {
			break
		}

		byteBuf := m.bytePool.Get().([]byte)
		requiredBytes := int(toFlush) * ChannelsOut * BytesPerSample
		if len(byteBuf) < requiredBytes {
			byteBuf = make([]byte, requiredBytes)
		}

		startIdx := (m.writeCursor - m.bufferStart) * int64(ChannelsOut)
		bytePos := 0
		for i := int64(0); i < toFlush; i++ {
			vL := m.buffer[startIdx+i*int64(ChannelsOut)]
			binary.LittleEndian.PutUint16(byteBuf[bytePos:bytePos+2], uint16(vL))
			bytePos += 2
			vR := m.buffer[startIdx+i*int64(ChannelsOut)+1]
			binary.LittleEndian.PutUint16(byteBuf[bytePos:bytePos+2], uint16(vR))
			bytePos += 2
		}

		if _, err := m.ffIn.Write(byteBuf[:requiredBytes]); err != nil {
			log.Printf("ffmpeg write err: %v", err)
			m.cancel()
			return
		}
		fmt.Printf("Wrote %d bytes to ffmpeg (samples: %d, writeCursor: %d)", requiredBytes, toFlush, m.writeCursor)
		m.bytePool.Put(byteBuf)
		m.writeCursor += toFlush

		// Slide buffer if we consumed > 50% of capacity
		consumed := m.writeCursor - m.bufferStart
		if consumed > m.bufferSamples/2 {
			remaining := (m.mixEnd - m.bufferStart) - consumed
			if remaining < 0 {
				remaining = 0
			}
			srcStart := consumed * int64(ChannelsOut)
			dstLen := remaining * int64(ChannelsOut)
			copy(m.buffer[:dstLen], m.buffer[srcStart:srcStart+dstLen])
			for i := dstLen; i < int64(len(m.buffer)); i++ {
				m.buffer[i] = 0
			}
			m.bufferStart += consumed
		}
	}
}

/////////////////////
// Public wiring helpers
/////////////////////

// AttachPeerConnection wires the mixer to a peer connection: it reads RTCP SRs and updates mappings.
// It does NOT call OnTrack for you; you should call AddTrackProcessor when tracks arrive.
func (m *Mixer) AttachPeerConnection(pc *webrtc.PeerConnection) {
	// Spawn RTCP readers for all current audio receivers to collect SRs
	startReceiver := func(recv *webrtc.RTPReceiver) {
		go func() {
			buf := make([]byte, 1500)
			for {
				n, attrs, err := recv.Read(buf)
				if err != nil {
					return
				}
				pkts, err := attrs.GetRTCPPackets(buf[:n])
				if err != nil {
					continue
				}
				for _, p := range pkts {
					if sr, ok := p.(*rtcp.SenderReport); ok {
						m.UpdateSR(sr)
					}
				}
			}
		}()
	}
	for _, r := range pc.GetReceivers() {
		if r.Track() != nil && r.Track().Kind() == webrtc.RTPCodecTypeAudio {
			startReceiver(r)
		}
	}
}

// AddTrackProcessorForChannel registers a processor for a given explicit channel
func (m *Mixer) AddTrackProcessor(ssrc uint32, channel int) (*TrackProcessor, error) {
	tp, err := NewTrackProcessor(ssrc, channel, m.incoming, 64)
	if err != nil {
		return nil, err
	}
	return tp, nil
}
