package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"

	// "reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type ChannelType int

const (
	ChannelUnknown ChannelType = iota
	ChannelOne
	ChannelTwo
)

type S3Config struct {
	Secure     bool
	Endpoint   string
	AccessKey  string
	SecretKey  string
	Bucket     string
	FilePrefix string
}

type RecordingConfig struct {
	BasePath       string
	ChannelMapping map[string]ChannelType
	S3             S3Config
}

type recordingSession struct {
	id      string
	cfg     RecordingConfig
	mixer   *Mixer
	tps     map[string]map[string]*TrackProcessor // clientID -> trackID -> processor
	mu      sync.Mutex
	paused  bool
	stopped bool
	meta    struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
	}
}

// StartRecording begins recording audio tracks using the SR-aligned mixer according to the provided config.
func (r *Room) StartRecording(cfg RecordingConfig) (string, error) {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession != nil {
		return "", fmt.Errorf("recording already in progress")
	}
	startTime := time.Now()
	id := fmt.Sprintf("%d_%s", startTime.Unix(), uuid.New().String())
	session := &recordingSession{
		id:  id,
		cfg: cfg,
		tps: make(map[string]map[string]*TrackProcessor),
	}
	session.meta.StartTime = startTime

	baseDir := filepath.Join(cfg.BasePath, id)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", err
	}

	// Record client join/leave events
	r.OnClientJoined(func(c *Client) {
		session.mu.Lock()
		session.meta.Events = append(session.meta.Events, Event{
			Type: "client_join",
			Time: time.Now(),
			Data: map[string]interface{}{"client_id": c.ID()},
		})
		session.mu.Unlock()
	})
	r.OnClientLeft(func(c *Client) {
		session.mu.Lock()
		session.meta.Events = append(session.meta.Events, Event{
			Type: "client_leave",
			Time: time.Now(),
			Data: map[string]interface{}{"client_id": c.ID()},
		})
		session.mu.Unlock()
	})

	// Create mixed output path now
	finalDir := filepath.Join(cfg.BasePath, id)
	mixedPath := filepath.Join(finalDir, id+".m4a")

	// Create mixer (writes raw to a temp aac/m4a file directly)
	mx, err := NewMixer(mixedPath, DefaultBatchMS, DefaultSafetyMS, DefaultBufferSec)
	if err != nil {
		return "", err
	}
	session.mixer = mx

	// Attach SR readers for current peer connections (all clients in this room)
	for _, c := range r.SFU().clients.GetClients() {
		mx.AttachPeerConnection(c.PeerConnection().PC())
	}

	// Helper to add a track processor for a given client and audio track
	addTrackProcessor := func(clientID string, track ITrack) error {
		session.mu.Lock()
		defer session.mu.Unlock()
		channel := cfg.ChannelMapping[clientID]
		if channel == ChannelUnknown {
			fmt.Printf("No channel mapping for client %s, skipping track %s", clientID, track.ID())
			return nil
		}

		fmt.Printf("adding writer for client %s, track %s, channel: %d", clientID, track.ID(), channel)

		if _, ok := session.tps[clientID]; !ok {
			session.tps[clientID] = make(map[string]*TrackProcessor)
		}

		// dedupe per-track
		if _, exists := session.tps[clientID][track.ID()]; exists {
			fmt.Printf("tp already exists for client %s, track %s", clientID, track.ID())
			return nil
		}

		// Wire mixer TrackProcessor for this audio track
		var ssrc uint32
		switch t := track.(type) {
		case *AudioTrack:
			ssrc = uint32(t.RemoteTrack().Track().SSRC())
		case *Track:
			if t.Kind() == webrtc.RTPCodecTypeAudio {
				ssrc = uint32(t.RemoteTrack().Track().SSRC())
			} else {
				return nil
			}
		default:
			return nil
		}

		var tp *TrackProcessor
		var err error
		// Map client to channel (left/right/both)
		switch channel {
		case ChannelOne:
			tp, err = session.mixer.AddTrackProcessor(ssrc, 0)
		case ChannelTwo:
			tp, err = session.mixer.AddTrackProcessor(ssrc, 1)
		default:
			err = fmt.Errorf("invalid channel: %d", channel)
		}

		if err != nil {
			return err
		}
		session.tps[clientID][track.ID()] = tp

		// add a hook for read callback
		track.OnRead(func(attributes interceptor.Attributes, packet *rtp.Packet, qualityLevel QualityLevel) {
			fmt.Printf("RTP packet received for client %s, track %s, SSRC %d, timestamp %d", clientID, track.ID(), packet.SSRC, packet.Timestamp)
			tp.ReadCallback(packet)
		})

		fmt.Printf("added mixer processor for client %s, track %s, ssrc: %d", clientID, track.ID(), ssrc)

		return nil
	}

	// Subscribe existing clients' tracks
	for clientID, client := range r.SFU().clients.GetClients() {
		fmt.Printf("Client Loop: %s", clientID)
		for _, track := range client.Tracks() {
			// Remove goroutine to avoid race condition
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				if err := addTrackProcessor(clientID, track); err != nil {
					fmt.Printf("error adding writer for client %s, track %s: %v", clientID, track.ID(), err)
				}
			}
		}

		// add a hook for add track too
		client.OnTracksReady(func(tracks []ITrack) {
			for _, track := range tracks {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addTrackProcessor(clientID, track)
				}
			}
		})
	}

	// Hook future client additions
	r.OnClientJoined(func(c *Client) {
		fmt.Printf("Client Joined: %s", c.ID())
		for _, track := range c.Tracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				_ = addTrackProcessor(c.ID(), track)
			}
		}

		// add a hook for add track too
		c.OnTracksReady(func(tracks []ITrack) {
			for _, track := range tracks {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addTrackProcessor(c.ID(), track)
				}
			}
		})
	})

	r.recordingSession = session
	return id, nil
}

// PauseRecording pauses writing RTP packets to files.
func (r *Room) PauseRecording() error {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession == nil {
		return fmt.Errorf("no recording in progress")
	}
	r.recordingSession.mu.Lock()
	r.recordingSession.paused = true
	r.recordingSession.meta.Events = append(r.recordingSession.meta.Events, Event{Type: "pause", Time: time.Now(), Data: nil})
	r.recordingSession.mu.Unlock()
	return nil
}

// ResumeRecording resumes writing RTP packets to files.
func (r *Room) ResumeRecording() error {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession == nil {
		return fmt.Errorf("no recording in progress")
	}
	r.recordingSession.mu.Lock()
	r.recordingSession.paused = false
	r.recordingSession.meta.Events = append(r.recordingSession.meta.Events, Event{Type: "resume", Time: time.Now(), Data: nil})
	r.recordingSession.mu.Unlock()
	return nil
}

// StopRecording stops the recording session, closes files, and writes metadata.
func (r *Room) StopRecording() error {

	fmt.Printf("stopping recording: %s", r.recordingSession.id)

	r.recordingMu.Lock()
	session := r.recordingSession
	r.recordingMu.Unlock()
	if session == nil {
		return fmt.Errorf("no recording in progress")
	}
	session.mu.Lock()
	session.meta.StopTime = time.Now()
	session.stopped = true
	session.mu.Unlock()

	// Stop all track processors and close mixer to finalize file
	session.mu.Lock()
	for _, tpMap := range session.tps {
		for _, tp := range tpMap {
			tp.Close()
		}
	}
	mx := session.mixer
	session.mu.Unlock()

	if mx != nil {
		_ = mx.Close()
	}

	// Check if recording duration is too short
	duration := session.meta.StopTime.Sub(session.meta.StartTime)
	fmt.Printf("Recording duration: %v", duration)
	if duration < 1*time.Second {
		fmt.Printf("WARNING: Recording duration (%v) is very short, file may appear as 0 seconds", duration)
		// Optional: Wait a bit more to capture any remaining audio
		fmt.Printf("Waiting additional 1 second to capture any remaining audio...")
		time.Sleep(1 * time.Second)
		session.meta.StopTime = time.Now()
	}

	fmt.Printf("writing meta.json: %s", session.id)

	// Write meta.json
	metaFile := filepath.Join(session.cfg.BasePath, session.id, "meta.json")
	f, err := os.Create(metaFile)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(session.meta); err != nil {
		return err
	}

	fmt.Printf("merging and uploading: %s", session.id)

	// Upload to S3 the already-mixed file
	go func() {
		baseDir := filepath.Join(session.cfg.BasePath, session.id)
		finalPath := filepath.Join(baseDir, session.id+".m4a")
		// If mixer wrote a different extension, fallback to that
		if _, err := os.Stat(finalPath); err != nil {
			// try .aac
			alt := filepath.Join(baseDir, session.id+".aac")
			if _, err2 := os.Stat(alt); err2 == nil {
				finalPath = alt
			} else {
				return
			}
		}
		// perform upload (minimal wrapper)
		mc, err := minio.New(session.cfg.S3.Endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(session.cfg.S3.AccessKey, session.cfg.S3.SecretKey, ""),
			Secure: session.cfg.S3.Secure,
		})
		if err != nil {
			fmt.Printf("error creating minio client: %v", err)
			return
		}
		object := path.Join(session.cfg.S3.FilePrefix, session.meta.StartTime.Format("02-01-2006"), filepath.Base(finalPath))
		contentType := "audio/mp4"
		if filepath.Ext(finalPath) == ".aac" {
			contentType = "audio/aac"
		}
		if _, err := mc.FPutObject(context.Background(), session.cfg.S3.Bucket, object, finalPath, minio.PutObjectOptions{ContentType: contentType}); err != nil {
			fmt.Printf("s3 upload failed: %v", err)
			return
		}
		fmt.Printf("uploaded to s3: %s", object)
		os.RemoveAll(baseDir)
	}()

	r.recordingMu.Lock()
	r.recordingSession = nil
	r.recordingMu.Unlock()
	return nil
}
