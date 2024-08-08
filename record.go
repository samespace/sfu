package sfu

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/samespace/sfu/processing"
)

type Recorder interface {
	Start() error
	Stop() error
	Pause() error
	Continue() error
	Close() error
}

const (
	bufferDuration = time.Second * 2
)

func (c *Client) getOrCreateTrackRecorder(trackID string) (Recorder, error) {
	if rec, ok := c.recorders.Load(trackID); ok {
		return rec, nil
	}

	track, err := c.tracks.Get(trackID)
	if err != nil {
		return nil, fmt.Errorf("track not found in client tracks: %w", err)
	}
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return nil, ErrOnlyAudioSupported
	}

	dir, err := c.createRecordingDirectory(trackID)
	if err != nil {
		return nil, err
	}

	file, err := os.Create(fmt.Sprintf("%s/audio.%s", dir, c.getTrackExtension(track.MimeType())))
	if err != nil {
		return nil, fmt.Errorf("error creating file: %v", err)
	}

	writer := NewChunkWriter(bufferDuration, func(chunk Chunk) {
		if _, err := file.Write(chunk); err != nil {
			fmt.Printf("error writing chunk: %v", err)
		}
	})

	recorder, err := c.createRecorderByMimeType(track, writer)
	if err != nil {
		return nil, fmt.Errorf("error creating recorder: %v", err)
	}
	c.recorders.Store(trackID, recorder)
	c.setupTrackRemovalHandler(trackID, track, recorder, writer)
	return recorder, nil
}

func (c *Client) createRecorderByMimeType(track ITrack, writer *ChunkWriter) (Recorder, error) {
	switch track.MimeType() {
	case webrtc.MimeTypeOpus:
		return NewOpusRecorder(track, writer)
	case webrtc.MimeTypePCMA:
		return NewPCMRecorder(PCMALaw, track, writer)
	case webrtc.MimeTypePCMU:
		return NewPCMRecorder(PCMULaw, track, writer)
	default:
		return nil, fmt.Errorf("unsupported track mime type: %s", track.MimeType())
	}
}

func (c *Client) createRecordingDirectory(trackID string) (string, error) {
	dir := fmt.Sprintf("recordings/%s/%s/%s", c.roomId, c.id, trackID)
	if err := ensureDir(dir); err != nil {
		return "", fmt.Errorf("unable to create recording directory: %w", err)
	}
	return dir, nil
}

func (c *Client) getTrackExtension(mimeType string) string {
	switch mimeType {
	case webrtc.MimeTypePCMA:
		return "pcma"
	case webrtc.MimeTypePCMU:
		return "pcmu"
	case webrtc.MimeTypeOpus:
		return "opus"
	default:
		return "unknown"
	}
}

func (c *Client) setupTrackRemovalHandler(trackID string, track ITrack, recorder Recorder, writer *ChunkWriter) {
	fn := func(sourceType string, removedTrack *webrtc.TrackLocalStaticRTP) {
		if track.ID() == removedTrack.ID() {
			if err := recorder.Close(); err != nil {
				c.log.Errorf("error closing the track recorder: %v", err)
			}
			c.recorders.Delete(trackID)
			writer.Close()
		}
	}

	c.OnTrackRemoved(fn)
}

func writeRecordingMetadata(dir string) error {
	file, err := os.OpenFile(dir, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer file.Close()
	meta := processing.RecordMetadata{
		StartTime: time.Now(),
	}
	j, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = file.Write(j)
	return err
}
