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

	// "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	silencePacketDetectionThreshold = 100 * time.Millisecond
	uploadRetryAttempts             = 3
	uploadRetryDelay                = 5 * time.Second
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

// bufferedPacket holds an RTP packet along with its arrival time
// legacy buffered recording types removed in favor of mixer-based recording

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
			return nil
		}

		fmt.Printf("adding writer for client %s, track %s", clientID, track.ID())

		if _, ok := session.tps[clientID]; !ok {
			session.tps[clientID] = make(map[string]*TrackProcessor)
		}

		// dedupe per-track
		if _, exists := session.tps[clientID][track.ID()]; exists {
			fmt.Printf("tp already exists for client %s, track %s", clientID, track.ID())
			return nil
		}

		// derive basic codec params if needed
		sampleRate := uint32(48000)
		_ = sampleRate

		// Use type switch to handle both Track and AudioTrack types
		var codecParams webrtc.RTPCodecParameters
		switch t := track.(type) {
		case *Track:
			codecParams = t.base.codec
		case *AudioTrack:
			codecParams = t.Track.base.codec
		default:
			r.sfu.log.Warnf("room: unknown track type: %T", track)
			return nil
		}

		if codecParams.ClockRate > 0 {
			sampleRate = uint32(codecParams.ClockRate)
		}

		// Wire mixer TrackProcessor for this audio track
		var remote IRemoteTrack
		switch t := track.(type) {
		case *AudioTrack:
			remote = t.RemoteTrack().Track()
		case *Track:
			if t.Kind() == webrtc.RTPCodecTypeAudio {
				remote = t.RemoteTrack().Track()
			} else {
				return nil
			}
		default:
			return nil
		}

		var tp *TrackProcessor
		var err error
		// Map client to channel (left/right/both)
		if channel == ChannelOne {
			tp, err = session.mixer.AddTrackProcessorForChannel(remote, 0)
		} else if channel == ChannelTwo {
			tp, err = session.mixer.AddTrackProcessorForChannel(remote, 1)
		} else {
			tp, err = session.mixer.AddTrackProcessorForChannel(remote, 2)
		}
		if err != nil {
			return err
		}
		session.tps[clientID][track.ID()] = tp

		fmt.Printf("added mixer processor for client %s, track %s", clientID, track.ID())

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

// legacy packet buffering functions removed

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

// mergeAndUpload mixes per-channel recordings, merges stereo, uploads to S3, and removes local files.
func (r *Room) mergeAndUpload(session *recordingSession) error {
	baseDir := filepath.Join(session.cfg.BasePath, session.id)

	logPath := filepath.Join(baseDir, "error.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("error creating log file %s: %v\n", logPath, err)
	}
	if logFile != nil {
		defer logFile.Close()
	}

	logError := func(format string, v ...interface{}) {
		msg := fmt.Sprintf(format, v...)
		fmt.Println(msg) // also print to stdout
		if logFile != nil {
			logFile.WriteString(time.Now().Format(time.RFC3339) + " " + msg + "\n")
		}
	}

	retry := func(attempts int, sleep time.Duration, fn func() error) error {
		var err error
		for i := 0; i < attempts; i++ {
			if i > 0 {
				logError("Retrying operation, attempt %d/%d...", i+1, attempts)
				time.Sleep(sleep)
			}
			err = fn()
			if err == nil {
				return nil
			}
			logError("Operation failed (attempt %d/%d): %v", i+1, attempts, err)
		}
		return fmt.Errorf("after %d attempts, last error: %w", attempts, err)
	}

	// runCmdWithRetry no longer used after mixer refactor

	// Mixer already produced final file; nothing to merge here.
	finalPath := filepath.Join(baseDir, session.id+".m4a")

	// Upload to S3
	mc, err := minio.New(session.cfg.S3.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(session.cfg.S3.AccessKey, session.cfg.S3.SecretKey, ""),
		Secure: session.cfg.S3.Secure,
	})
	if err != nil {
		logError("error creating minio client: %v", err)
		return err
	}
	dateStr := session.meta.StartTime.Format("02-01-2006")
	object := path.Join(session.cfg.S3.FilePrefix, dateStr, session.id+".m4a")
	ctx := context.Background()

	uploadFn := func() error {
		_, err := mc.FPutObject(ctx, session.cfg.S3.Bucket, object, finalPath, minio.PutObjectOptions{ContentType: "audio/mp4"})
		return err
	}

	if err := retry(uploadRetryAttempts, uploadRetryDelay, uploadFn); err != nil {
		logError("s3 upload failed after all retries: %v", err)
		return err
	}

	fmt.Printf("uploaded to s3: %s", object)
	fmt.Printf("removing local files: %s", baseDir)
	// Cleanup local files
	os.RemoveAll(baseDir)
	return nil
}

// legacy OGG helpers removed
