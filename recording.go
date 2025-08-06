package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

const (
	uploadRetryAttempts = 3
	uploadRetryDelay    = 5 * time.Second
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
	writers map[string]map[string]*trackWriter // clientID -> trackID -> writer
	mu      sync.Mutex
	paused  bool
	stopped bool
	meta    struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
	}
}

type trackWriter struct {
	sequenceNumber uint16
	timestamp      uint32
	ssrc           uint32
	audioWriter    *oggwriter.OggWriter
	clockRate      uint32

	mu sync.Mutex
}

// StartRecording begins recording audio tracks in the room according to the provided config.
func (r *Room) StartRecording(cfg RecordingConfig) (string, error) {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession != nil {
		return "", fmt.Errorf("recording already in progress")
	}
	startTime := time.Now()
	id := fmt.Sprintf("%d_%s", startTime.Unix(), uuid.New().String())
	session := &recordingSession{
		id:      id,
		cfg:     cfg,
		writers: make(map[string]map[string]*trackWriter),
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

	// Helper to add a track writer for a given client and track
	addWriter := func(clientID string, track ITrack) error {

		session.mu.Lock()
		defer session.mu.Unlock()
		channel := cfg.ChannelMapping[clientID]
		if channel == ChannelUnknown {
			return nil
		}

		fmt.Printf("adding writer for client %s, track %s", clientID, track.ID())

		if _, ok := session.writers[clientID]; !ok {
			session.writers[clientID] = make(map[string]*trackWriter)
		}

		// Check if this specific track already has a writer
		if _, exists := session.writers[clientID][track.ID()]; exists {
			fmt.Printf("writer already exists for client %s, track %s", clientID, track.ID())
			return nil
		}

		trackDir := filepath.Join(baseDir, clientID)
		if err := os.MkdirAll(trackDir, 0755); err != nil {
			return err
		}
		filePath := filepath.Join(trackDir, fmt.Sprintf("%s.ogg", track.ID()))

		sampleRate := uint32(48000) // Default for Opus
		channelCount := uint16(1)   // Default for Opus

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

		ow, err := oggwriter.New(filePath, sampleRate, channelCount)
		if err != nil {
			return err
		}

		// Create trackWriter
		tw := &trackWriter{
			audioWriter: ow,
			clockRate:   sampleRate,
		}

		session.writers[clientID][track.ID()] = tw

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			tw.mu.Lock()
			defer tw.mu.Unlock()

			tw.sequenceNumber = pkt.SequenceNumber
			tw.timestamp = pkt.Timestamp
			tw.ssrc = pkt.SSRC

			if session.paused || session.stopped {
				// add the silence packet here
				pkt.Payload = []byte{0xF8, 0xFF, 0xFE}
			}
			tw.audioWriter.WriteRTP(pkt)
		})

		fmt.Printf("added writer for client %s, track %s", clientID, track.ID())

		return nil
	}

	// Subscribe existing clients' tracks
	for clientID, client := range r.SFU().clients.GetClients() {
		fmt.Printf("Client Loop: %s", clientID)
		for _, track := range client.Tracks() {
			// Remove goroutine to avoid race condition
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				if err := addWriter(clientID, track); err != nil {
					fmt.Printf("error adding writer for client %s, track %s: %v", clientID, track.ID(), err)
				}
			}
		}

		// add a hook for add track too
		client.OnTracksReady(func(tracks []ITrack) {
			for _, track := range tracks {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addWriter(clientID, track)
				}
			}
		})
	}

	// Hook future client additions
	r.OnClientJoined(func(c *Client) {
		fmt.Printf("Client Joined: %s", c.ID())
		for _, track := range c.Tracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				_ = addWriter(c.ID(), track)
			}
		}

		// add a hook for add track too
		c.OnTracksReady(func(tracks []ITrack) {
			for _, track := range tracks {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addWriter(c.ID(), track)
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

	fmt.Printf("closing writers: %s", session.id)

	// Close writers
	for _, m := range session.writers {
		for _, tw := range m {
			tw.mu.Lock()

			// before closing write one silent packet
			tw.audioWriter.WriteRTP(&rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    111,
					Marker:         true,
					SequenceNumber: tw.sequenceNumber + 1,
					Timestamp:      tw.timestamp + 960,
					SSRC:           tw.ssrc,
				},
				Payload: []byte{0xF8, 0xFF, 0xFE},
			})
			tw.audioWriter.Close()

			tw.mu.Unlock()
		}
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

	// Merge channels and upload to S3
	go func() {
		if err := r.mergeAndUpload(session); err != nil {
			fmt.Printf("error merging and uploading: %v", err)
		}
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

	runCmdWithRetry := func(name string, args ...string) error {
		return retry(uploadRetryAttempts, uploadRetryDelay, func() error {
			cmd := exec.Command(name, args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("command '%s %v' failed: %w, output: %s", name, args, err, string(out))
			}
			return nil
		})
	}

	// Group track files by channel
	filesByChannel := map[ChannelType][]string{}
	for clientID, writerMap := range session.writers {
		ch := session.cfg.ChannelMapping[clientID]
		for trackID := range writerMap {
			filesByChannel[ch] = append(filesByChannel[ch], filepath.Join(baseDir, clientID, fmt.Sprintf("%s.ogg", trackID)))
		}
	}
	// Create mono mixes per channel
	monoFiles := map[ChannelType]string{}
	for _, ch := range []ChannelType{ChannelOne, ChannelTwo} {
		inputs := filesByChannel[ch]
		if len(inputs) == 0 {
			continue
		}
		monoPath := filepath.Join(baseDir, fmt.Sprintf("mono_%d.ogg", ch))
		if len(inputs) == 1 {
			src := inputs[0]
			inF, err := os.Open(src)
			if err != nil {
				err = fmt.Errorf("copy file for channel %d failed: %v", ch, err)
				logError(err.Error())
				return err
			}
			defer inF.Close()
			outF, err := os.Create(monoPath)
			if err != nil {
				err = fmt.Errorf("copy file for channel %d failed: %v", ch, err)
				logError(err.Error())
				return err
			}
			defer outF.Close()
			if _, err := io.Copy(outF, inF); err != nil {
				err = fmt.Errorf("copy file for channel %d failed: %v", ch, err)
				logError(err.Error())
				return err
			}
			monoFiles[ch] = monoPath
			continue
		}
		args := []string{"-y"}
		for _, in := range inputs {
			args = append(args, "-i", in)
		}
		filter := fmt.Sprintf("amix=inputs=%d:duration=longest", len(inputs))
		args = append(args, "-filter_complex", filter, "-ac", "1", monoPath)
		if err := runCmdWithRetry("ffmpeg", args...); err != nil {
			err = fmt.Errorf("ffmpeg mix channel %d failed: %w", ch, err)
			logError(err.Error())
			return err
		}
		monoFiles[ch] = monoPath
	}
	// Merge to stereo
	finalPath := filepath.Join(baseDir, session.id+".ogg")
	left, hasLeft := monoFiles[ChannelOne]
	right, hasRight := monoFiles[ChannelTwo]
	if hasLeft && hasRight {
		args := []string{"-y", "-i", left, "-i", right, "-filter_complex", "amerge=inputs=2", "-ac", "2", finalPath}
		if err := runCmdWithRetry("ffmpeg", args...); err != nil {
			err = fmt.Errorf("ffmpeg merge stereo failed: %w", err)
			logError(err.Error())
			return err
		}
	} else if hasLeft || hasRight {
		src := left
		if !hasLeft {
			src = right
		}
		if err := os.Rename(src, finalPath); err != nil {
			logError("failed to rename mono file: %v", err)
			return err
		}
	} else {
		err := fmt.Errorf("no audio to merge")
		logError(err.Error())
		return err
	}
	// Convert merged .ogg to .m4a
	m4aPath := filepath.Join(baseDir, session.id+".m4a")
	args := []string{"-y", "-i", finalPath, "-c:a", "aac", "-b:a", "32k", m4aPath}
	if err := runCmdWithRetry("ffmpeg", args...); err != nil {
		err = fmt.Errorf("ffmpeg convert to m4a failed: %w", err)
		logError(err.Error())
		return err
	}

	if err := os.Remove(finalPath); err != nil {
		logError("warning: failed to remove merged ogg: %v", err)
	}
	finalPath = m4aPath

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
