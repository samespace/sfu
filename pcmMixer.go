package sfu

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
	"gopkg.in/hraban/opus.v2"
)

type rtpPacket struct {
	pkt   *rtp.Packet
	seq   uint16
	index int
}

type packetHeap []*rtpPacket

func (ph packetHeap) Len() int { return len(ph) }
func (ph packetHeap) Less(i, j int) bool {
	return int16(ph[i].seq-ph[j].seq) < 0
}
func (ph packetHeap) Swap(i, j int) {
	ph[i], ph[j] = ph[j], ph[i]
	ph[i].index = i
	ph[j].index = j
}
func (ph *packetHeap) Push(x interface{}) {
	n := len(*ph)
	pkt := x.(*rtpPacket)
	pkt.index = n
	*ph = append(*ph, pkt)
}
func (ph *packetHeap) Pop() interface{} {
	old := *ph
	n := len(old)
	pkt := old[n-1]
	old[n-1] = nil
	pkt.index = -1
	*ph = old[0 : n-1]
	return pkt
}

type tinyJitterBuffer struct {
	pq          packetHeap
	maxDelay    int
	expectedSeq uint16
	started     bool
}

func newTinyJitterBuffer(maxDelayPackets int) *tinyJitterBuffer {
	return &tinyJitterBuffer{
		pq:       make(packetHeap, 0, maxDelayPackets*2),
		maxDelay: maxDelayPackets,
	}
}

func (jb *tinyJitterBuffer) push(pkt *rtp.Packet) {
	heap.Push(&jb.pq, &rtpPacket{pkt: pkt, seq: pkt.SequenceNumber})
}

func (jb *tinyJitterBuffer) popNext() (out *rtp.Packet, gap bool) {
	if !jb.started {
		if jb.pq.Len() >= jb.maxDelay {
			first := heap.Pop(&jb.pq).(*rtpPacket)
			jb.expectedSeq = first.seq + 1
			jb.started = true
			return first.pkt, false
		}
		return nil, false
	}

	if jb.pq.Len() == 0 {
		return nil, false
	}

	next := jb.pq[0]
	if next.seq == jb.expectedSeq {
		heap.Pop(&jb.pq)
		jb.expectedSeq++
		return next.pkt, false
	}

	// gap — insert silence
	jb.expectedSeq++
	return nil, true
}

// Constants - tuned for WebRTC typical values
const (
	SampleRate      = 48000
	Channels        = 1
	FrameMs         = 20                          // mixing frame length in ms
	SamplesPerFrame = SampleRate * FrameMs / 1000 // 960 samples for 20ms @ 48k
)

type PCMFrame = []int16

// Source represents an incoming Opus source
type Source struct {
	ID string

	// channel receives decoded PCM frames (length SamplesPerFrame)
	pcmCh chan PCMFrame

	// stop control
	cancel context.CancelFunc

	// low-latency jitter buffer / sequence based reading can be added here
	jb *tinyJitterBuffer

	// decoder
	dec *opus.Decoder
}

// Mixer mixes multiple sources
type Mixer struct {
	mu      sync.RWMutex
	sources map[string]*Source
	pool    *sync.Pool // []int16 buffers
	tick    *time.Ticker
	running bool
	ctx     context.Context
	cancel  context.CancelFunc
	file    *os.File

	// callback for mixed PCM frame
	OnMixed func(pcm PCMFrame)
}

// NewMixer creates a mixer
func NewMixer(filepath string) (*Mixer, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mixer{
		sources: make(map[string]*Source),
		pool: &sync.Pool{
			New: func() any {
				buf := make([]int16, SamplesPerFrame)
				return &buf
			},
		},
		tick:   time.NewTicker(FrameMs * time.Millisecond),
		ctx:    ctx,
		cancel: cancel,
	}

	f, err := os.Create(filepath)
	if err != nil {
		return nil, err
	}
	m.file = f

	m.OnMixed = func(pcm PCMFrame) {
		// PCM is []int16 little-endian
		// Convert to byte slice without extra alloc
		buf := make([]byte, len(pcm)*2)
		for i, v := range pcm {
			buf[2*i] = byte(v)        // low byte
			buf[2*i+1] = byte(v >> 8) // high byte
		}
		if _, err := f.Write(buf); err != nil {
			log.Printf("PCM write error: %v", err)
		}
		m.putBuf(pcm) // return buffer to pool
	}

	return m, nil
}

func (m *Mixer) GetSource(id string) *Source {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sources[id]
}

// AddSource: given a function that writes decoded PCM into the source's pcmCh.
// Typical caller: for each incoming RTP/TrackRemote you spawn a goroutine that
// decodes Opus frames to PCM and writes to the source's pcmCh.
func (m *Mixer) AddSource(id string) (*Source, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sources[id]; ok {
		return nil, errors.New("source exists")
	}
	sourceCtx, cancel := context.WithCancel(m.ctx)

	jb := newTinyJitterBuffer(4) // ~60 ms for 20ms frames
	dec, err := opus.NewDecoder(SampleRate, 1)
	if err != nil {
		cancel()
		return nil, err
	}

	s := &Source{
		ID:     id,
		pcmCh:  make(chan PCMFrame, 10), // small ring
		cancel: cancel,
		jb:     jb,
		dec:    dec,
	}
	m.sources[id] = s

	go m.startSource(sourceCtx, s, jb, dec)

	return s, nil
}

func (m *Mixer) startSource(ctx context.Context, src *Source, jb *tinyJitterBuffer, dec *opus.Decoder) {
	ticker := time.NewTicker(FrameMs * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rtpPkt, gap := jb.popNext()
			pcm := m.getBuf()
			if gap || rtpPkt == nil {
				zeroSlice(pcm)
			} else {
				fmt.Printf("source %s: %d\n", src.ID, len(pcm))
				n, err := dec.Decode(rtpPkt.Payload, pcm)
				if err != nil || n != SamplesPerFrame {
					fmt.Printf("error decoding %s: %v\n", src.ID, err)
					zeroSlice(pcm)
				}
			}
			src.pcmCh <- pcm
		}
	}
}

// RemoveSource
func (m *Mixer) RemoveSource(id string) {
	m.mu.Lock()
	s, ok := m.sources[id]
	if ok {
		delete(m.sources, id)
	}
	m.mu.Unlock()
	if ok {
		s.cancel()
		// Wait a bit for writers to stop, then close
		time.Sleep(10 * time.Millisecond)
		select {
		case <-s.pcmCh: // drain
		default:
		}
		close(s.pcmCh)
	}
}

// Start mixer main loop
func (m *Mixer) Start() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	go m.loop()
}

// Stop mixer
func (m *Mixer) Stop() {
	m.cancel()
	m.tick.Stop()
	m.mu.Lock()
	m.running = false
	if m.file != nil {
		if err := m.file.Close(); err != nil {
			log.Printf("Error closing mixer file: %v", err)
		}
		m.file = nil
	}
	m.mu.Unlock()
}

func (m *Mixer) loop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.tick.C:
			m.mixOnce()
		}
	}
}

func (m *Mixer) mixOnce() {
	// snapshot sources to avoid blocking registration during mixing
	m.mu.RLock()
	sources := make([]*Source, 0, len(m.sources))
	for _, s := range m.sources {
		sources = append(sources, s)
	}
	m.mu.RUnlock()

	if len(sources) == 0 {
		// nothing to mix - produce silence if needed
		outBuf := m.getBuf()
		zeroSlice(outBuf)
		if m.OnMixed != nil {
			m.OnMixed(outBuf)
		} else {
			m.putBuf(outBuf)
		}
		return
	}

	// accumulate in int32 to avoid overflow; then convert to int16 with clipping
	acc := make([]int32, SamplesPerFrame)

	// For each source, try to read a frame non-blocking; if none available -> silence
	for _, s := range sources {
		var frame PCMFrame
		select {
		case frame = <-s.pcmCh:
			// got a frame
		default:
			// no frame available -> pad silence
			frame = nil
		}

		if frame == nil {
			// silence: skip (acc remains unchanged)
			continue
		}

		// Mix: add into acc
		for i := 0; i < SamplesPerFrame; i++ {
			acc[i] += int32(frame[i])
		}
		// return buffer to pool
		m.putBuf(frame)
	}

	// Normalize / clip and return as int16 slice
	out := m.getBuf()
	// simple normalization: if N sources > 1, divide by count to avoid clipping. More complex AGC can be used.
	n := int32(len(sources))
	if n < 1 {
		n = 1
	}
	for i := 0; i < SamplesPerFrame; i++ {
		v := acc[i] / n
		// clip to int16
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}

	if m.OnMixed != nil {
		m.OnMixed(out)
	} else {
		m.putBuf(out)
	}
}

// helpers
func (m *Mixer) getBuf() PCMFrame {
	v := m.pool.Get().(*[]int16)
	return (*v)[:]
}
func (m *Mixer) putBuf(b PCMFrame) {
	// avoid holding onto large arrays longer than needed
	// zero it to prevent retaining previous PCM
	zeroSlice(b)
	m.pool.Put(&b)
}
func zeroSlice(b PCMFrame) {
	for i := range b {
		b[i] = 0
	}
}
