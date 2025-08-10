package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
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

// bufferedPacket removed: we now write directly from OnRead

type trackWriter struct {
	writer    mkvcore.BlockWriteCloser
	lastPTS   int64
	mu        sync.Mutex
	clockRate uint32
	rtpBase   uint32
	ptsBaseMs int64
	rtpInited bool

	// Buffering fields
	recordingStartTime time.Time
}

// buildOpusHead returns a minimal OpusHead for Matroska/WebM CodecPrivate.
// Spec: https://wiki.xiph.org/OggOpus#ID_Header
func buildOpusHead(channels int, sampleRate int) []byte {
	// Minimal 19-byte OpusHead
	b := make([]byte, 19)
	copy(b[0:8], []byte("OpusHead"))
	b[8] = 1
	b[9] = byte(channels)
	// pre-skip 960 (20ms @ 48kHz) for safety
	preSkip := uint16(960)
	b[10] = byte(preSkip & 0xFF)
	b[11] = byte(preSkip >> 8)
	sr := uint32(sampleRate)
	b[12] = byte(sr & 0xFF)
	b[13] = byte((sr >> 8) & 0xFF)
	b[14] = byte((sr >> 16) & 0xFF)
	b[15] = byte((sr >> 24) & 0xFF)
	b[16] = 0 // gain
	b[17] = 0
	b[18] = 0 // channel mapping
	return b
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
		filePath := filepath.Join(trackDir, fmt.Sprintf("%s.webm", track.ID()))

		sampleRate := uint32(48000)
		channelCount := uint16(1)

		// Track type is already known to be audio; no codec params required for WebM/Opus

		// Opus in WebM is fixed 48kHz; ignore codec clockrate variations.
		f, err := os.Create(filePath)
		if err != nil {
			return err
		}
		// Build OpusHead (CodecPrivate) for Matroska/WebM Opus
		opusHead := buildOpusHead(int(channelCount), int(sampleRate))

		tracks := []webm.TrackEntry{{
			Name:         "audio",
			TrackNumber:  1,
			TrackUID:     uint64(time.Now().UnixNano()),
			CodecID:      "A_OPUS",
			CodecPrivate: opusHead,
			TrackType:    2,
			CodecDelay:   6500000,
			SeekPreRoll:  80000000,
			Audio: &webm.Audio{
				SamplingFrequency: float64(sampleRate),
				Channels:          uint64(channelCount),
			},
		}}
		writers, err := webm.NewSimpleBlockWriter(f, tracks, mkvcore.WithSeekHead(true))
		if err != nil || len(writers) != 1 {
			if err == nil {
				err = fmt.Errorf("unexpected writers count: %d", len(writers))
			}
			return err
		}

		// Create trackWriter
		tw := &trackWriter{
			writer:             writers[0],
			lastPTS:            -1,
			clockRate:          sampleRate,
			recordingStartTime: session.meta.StartTime,
		}

		session.writers[clientID][track.ID()] = tw

		// Anchor stream at t=0 with an Opus DTX frame so leading silence is preserved
		tw.mu.Lock()
		if tw.lastPTS < 0 {
			if _, err := tw.writer.Write(true, 0, []byte{0xF8, 0xFF, 0xFE}); err == nil {
				tw.lastPTS = 0
			}
		}
		tw.mu.Unlock()

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			if session.paused || session.stopped {
				return
			}

			// Direct write with explicit PTS; keep OnRead fast by minimizing critical section
			cloned := pkt.Clone()
			arrival := time.Now()
			tw.mu.Lock()
			// Compute PTS from both wall-clock and RTP timestamp; pick the larger to avoid compression
			ptsWall := arrival.Sub(tw.recordingStartTime).Milliseconds()
			if !tw.rtpInited {
				tw.rtpBase = cloned.Timestamp
				tw.ptsBaseMs = ptsWall
				tw.rtpInited = true
			}
			rtpDelta := uint32(cloned.Timestamp - tw.rtpBase)
			ptsRtp := tw.ptsBaseMs + int64(rtpDelta)*1000/int64(tw.clockRate)
			pts := ptsWall
			if ptsRtp > pts {
				pts = ptsRtp
			}
			if pts <= tw.lastPTS {
				pts = tw.lastPTS + 1
			}
			if _, err := tw.writer.Write(true, pts, cloned.Payload); err != nil {
				fmt.Printf("error writing webm block: %v", err)
			} else {
				tw.lastPTS = pts
			}
			tw.mu.Unlock()
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

	// Ensure duration covers StopTime by appending a final silent Opus frame per track
	for _, writerMap := range session.writers {
		for _, tw := range writerMap {
			tw.mu.Lock()
			targetPts := session.meta.StopTime.Sub(tw.recordingStartTime).Milliseconds()
			if targetPts > tw.lastPTS {
				silence := []byte{0xF8, 0xFF, 0xFE}
				pts := targetPts
				if pts <= tw.lastPTS {
					pts = tw.lastPTS + 1
				}
				if _, err := tw.writer.Write(true, pts, silence); err != nil {
					fmt.Printf("error writing final silence: %v", err)
				} else {
					tw.lastPTS = pts
				}
			}
			tw.mu.Unlock()
		}
	}

	fmt.Printf("closing writers: %s", session.id)

	// Close writers
	for _, m := range session.writers {
		for _, tw := range m {
			_ = tw.writer.Close()
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

	// Build single-pass ffmpeg to mix per-channel inputs to stereo and encode AAC
	filesByChannel := map[ChannelType][]string{}
	for clientID, writerMap := range session.writers {
		ch := session.cfg.ChannelMapping[clientID]
		for trackID := range writerMap {
			filesByChannel[ch] = append(filesByChannel[ch], filepath.Join(baseDir, clientID, fmt.Sprintf("%s.webm", trackID)))
		}
	}

	leftInputs := filesByChannel[ChannelOne]
	rightInputs := filesByChannel[ChannelTwo]
	logError("merge: leftInputs=%d rightInputs=%d", len(leftInputs), len(rightInputs))
	if len(leftInputs) == 0 && len(rightInputs) == 0 {
		err := fmt.Errorf("no audio to merge")
		logError(err.Error())
		return err
	}

	m4aPath := filepath.Join(baseDir, session.id+".m4a")
	args := []string{"-y", "-copyts", "-start_at_zero"}
	// Add inputs: left first, then right. Mark primary audio stream and ensure stereo layout on output.
	for _, in := range leftInputs {
		args = append(args, "-itsoffset", "0", "-i", in)
	}
	for _, in := range rightInputs {
		args = append(args, "-itsoffset", "0", "-i", in)
	}

	// Construct filter graph with per-branch resample to pad gaps from timestamps.
	var filter string
	nL := len(leftInputs)
	nR := len(rightInputs)
	// Left branch
	if nL >= 1 {
		if nL == 1 {
			filter += "[0:a]aresample=async=1:first_pts=0[aL];"
		} else {
			for i := 0; i < nL; i++ {
				filter += fmt.Sprintf("[%d:a]", i)
			}
			filter += fmt.Sprintf("amix=inputs=%d:duration=longest[Lm];[Lm]aresample=async=1:first_pts=0[aL];", nL)
		}
	}
	// Right branch
	if nR >= 1 {
		base := nL
		if nR == 1 {
			filter += fmt.Sprintf("[%d:a]aresample=async=1:first_pts=0[aR];", base)
		} else {
			for i := 0; i < nR; i++ {
				filter += fmt.Sprintf("[%d:a]", base+i)
			}
			filter += fmt.Sprintf("amix=inputs=%d:duration=longest[Rm];[Rm]aresample=async=1:first_pts=0[aR];", nR)
		}
	}
	if nL >= 1 && nR >= 1 {
		filter += "[aL][aR]amerge=inputs=2[aout]"
		args = append(args, "-filter_complex", filter, "-map", "[aout]", "-c:a", "aac", "-ac", "2", "-b:a", "32k", m4aPath)
	} else if nL >= 1 {
		args = append(args, "-filter_complex", filter, "-map", "[aL]", "-c:a", "aac", "-ac", "1", "-b:a", "32k", m4aPath)
	} else if nR >= 1 {
		args = append(args, "-filter_complex", filter, "-map", "[aR]", "-c:a", "aac", "-ac", "1", "-b:a", "32k", m4aPath)
	}

	logError("ffmpeg args: %v", args)
	if err := runCmdWithRetry("ffmpeg", args...); err != nil {
		err = fmt.Errorf("ffmpeg single-pass mix+encode failed: %w", err)
		logError(err.Error())
		return err
	}

	finalPath := m4aPath

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

// removed: ogg granule helper no longer needed
