package sfu

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"time"
)

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
	_, cancel := context.WithCancel(m.ctx)
	s := &Source{
		ID:     id,
		pcmCh:  make(chan PCMFrame, 4), // small ring
		cancel: cancel,
	}
	m.sources[id] = s

	return s, nil
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
		m.file.Close()
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
