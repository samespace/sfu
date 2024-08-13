package sfu

import (
	"bytes"
	"sync"
	"time"
)

type ChunkWriter struct {
	delay     time.Duration
	writeLock sync.RWMutex
	chunk     *bytes.Buffer
	onChunkFn onChunkFn
	ticker    *time.Ticker
	stopChan  chan struct{}
}

type onChunkFn func(Chunk)

type Chunk []byte

func NewChunkWriter(delay time.Duration, onChunk onChunkFn) *ChunkWriter {
	cw := &ChunkWriter{
		delay:     delay,
		chunk:     &bytes.Buffer{},
		onChunkFn: onChunk,
		stopChan:  make(chan struct{}),
	}

	cw.ticker = time.NewTicker(delay)
	go cw.start()

	return cw
}

func (cw *ChunkWriter) start() {
	for {
		select {
		case <-cw.ticker.C:
			cw.flush()
		case <-cw.stopChan:
			cw.flush()
			cw.ticker.Stop()
			return
		}
	}
}

func (cw *ChunkWriter) Write(data []byte) (int, error) {
	cw.writeLock.Lock()
	defer cw.writeLock.Unlock()
	n, err := cw.chunk.Write(data)
	return n, err
}

func (cw *ChunkWriter) flush() {
	cw.writeLock.Lock()
	defer cw.writeLock.Unlock()

	if cw.chunk.Len() > 0 {
		chunkCopy := make(Chunk, cw.chunk.Len())
		copy(chunkCopy, cw.chunk.Bytes())
		cw.onChunkFn(chunkCopy)
		cw.chunk.Reset()
	}
}

func (cw *ChunkWriter) Close() {
	close(cw.stopChan)
}
