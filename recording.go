package sfu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// --- public/config types (kept similar to original) ---

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

// recordingSession keeps lightweight session state
type recordingSession struct {
	id      string
	cfg     RecordingConfig
	mu      sync.Mutex
	paused  bool
	stopped bool
	meta    struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
	}
	baseDir   string
	recorders map[string]*trackRecorder
	// completed recordings split by channel
	completedFiles struct {
		left  []string
		right []string
	}
}

// Track-level recorder using packet-level Opus concatenation into Ogg/Opus
// This avoids decoding/re-encoding and keeps CPU low.
type trackRecorder struct {
	mu       sync.Mutex
	ogg      *oggwriter.OggWriter
	filePath string
	channel  ChannelType
	closed   bool

	// optional bookkeeping
	hasReceivedPacket bool
	lastRTPTimestamp  uint32
	lastSeqNumber     uint16
}

// newTrackRecorder creates an Ogg Opus writer for a single track file
func newTrackRecorder(path string, ch ChannelType) (*trackRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// create the underlying file via oggwriter, which will open/create the file
	w, err := oggwriter.New(path, 48000, 1)
	if err != nil {
		return nil, err
	}
	return &trackRecorder{
		ogg:      w,
		filePath: path,
		channel:  ch,
	}, nil
}

// writeRTP writes the RTP packet payload into the Ogg Opus container using pion/oggwriter
// This is packet-level concatenation: we preserve RTP timestamps and let the opus decoder
// (clients that play the file) do PLC for missing frames.
func (t *trackRecorder) writeRTP(pkt *rtp.Packet) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}

	// mark first packet
	if !t.hasReceivedPacket {
		fmt.Printf("First RTP packet for %s timestamp=%d seq=%d payload=%d\n", t.filePath, pkt.Timestamp, pkt.SequenceNumber, len(pkt.Payload))
		t.hasReceivedPacket = true
	}

	// let oggwriter handle framing; it provides WriteRTP
	if err := t.ogg.WriteRTP(pkt); err != nil {
		return fmt.Errorf("write RTP to ogg: %w", err)
	}

	// update bookkeeping
	t.lastRTPTimestamp = pkt.Timestamp
	t.lastSeqNumber = pkt.SequenceNumber
	return nil
}

func (t *trackRecorder) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	w := t.ogg
	// unlock before potentially blocking Close()
	t.mu.Unlock()
	if w == nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("ogg close: %w", err)
	}
	return nil
}

// ----------------- Room recording orchestration (refactored Start/Stop/Pause/Resume) -----------------

// NOTE: This code assumes the surrounding Room type and SFU/client APIs (ITrack, Client, Room etc.)
// exist as in the original snippet. Only the recording-related logic is refactored to use
// packet-level concatenation via pion/oggwriter and to simplify control flow.

func (r *Room) StartRecording(cfg RecordingConfig) (string, error) {
	if cfg.BasePath == "" {
		return "", fmt.Errorf("recording base path is required")
	}
	if len(cfg.ChannelMapping) == 0 {
		return "", fmt.Errorf("channel mapping is required")
	}

	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession != nil {
		return "", fmt.Errorf("recording already in progress")
	}

	startTime := time.Now()
	id := fmt.Sprintf("%d_%s", startTime.Unix(), uuid.New().String())
	session := &recordingSession{
		id:        id,
		cfg:       cfg,
		recorders: make(map[string]*trackRecorder),
	}
	session.meta.StartTime = startTime

	baseDir := filepath.Join(cfg.BasePath, id)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", err
	}
	session.baseDir = baseDir

	// helper to append event safely
	appendEvent := func(ev Event) {
		session.mu.Lock()
		session.meta.Events = append(session.meta.Events, ev)
		session.mu.Unlock()
	}

	// client join/leave hooks
	r.OnClientJoined(func(c *Client) {
		appendEvent(Event{Type: "client_join", Time: time.Now(), Data: map[string]interface{}{"client_id": c.ID()}})
	})
	r.OnClientLeft(func(c *Client) {
		appendEvent(Event{Type: "client_leave", Time: time.Now(), Data: map[string]interface{}{"client_id": c.ID()}})
	})

	// addWriter now creates an Ogg Opus file per track and writes packet payloads directly
	addWriter := func(clientID string, track ITrack) error {
		channel := cfg.ChannelMapping[clientID]
		if channel == ChannelUnknown {
			fmt.Printf("WARNING: Client %s not in channel mapping or mapped to ChannelUnknown, skipping recording\n", clientID)
			return nil
		}

		session.mu.Lock()
		defer session.mu.Unlock()

		key := clientID + "_" + track.ID()
		if _, ok := session.recorders[key]; ok {
			return nil // already recording
		}

		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return nil
		}
		if track.MimeType() != webrtc.MimeTypeOpus {
			fmt.Printf("recording: skip codec %s for client %s track %s\n", track.MimeType(), clientID, track.ID())
			return nil
		}

		trackDir := filepath.Join(baseDir, clientID)
		if err := os.MkdirAll(trackDir, 0755); err != nil {
			return err
		}
		filePath := filepath.Join(trackDir, fmt.Sprintf("%s.opus.ogg", track.ID()))
		rec, err := newTrackRecorder(filePath, channel)
		if err != nil {
			return err
		}
		session.recorders[key] = rec

		// on track end: close and move to completed
		track.OnEnded(func() {
			session.mu.Lock()
			defer session.mu.Unlock()
			if rec, ok := session.recorders[key]; ok {
				_ = rec.close()
				switch rec.channel {
				case ChannelOne:
					session.completedFiles.left = append(session.completedFiles.left, rec.filePath)
				case ChannelTwo:
					session.completedFiles.right = append(session.completedFiles.right, rec.filePath)
				}
				delete(session.recorders, key)
				fmt.Printf("Track ended for %s, moved to completed files\n", key)
			}
		})

		// read loop writes RTP packets into the Ogg file
		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			session.mu.Lock()
			paused := session.paused
			stopped := session.stopped
			session.mu.Unlock()
			if paused || stopped {
				return
			}
			_ = rec.writeRTP(pkt)
		})

		fmt.Printf("added writer for client %s, track %s -> %s\n", clientID, track.ID(), filePath)
		return nil
	}

	// subscribe existing clients and future tracks
	for clientID, client := range r.SFU().clients.GetClients() {
		for _, track := range client.Tracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				if err := addWriter(clientID, track); err != nil {
					fmt.Printf("error adding writer for %s: %v\n", clientID, err)
				}
			}
		}
		// bind future tracks
		capturedClientID := clientID
		client.OnTracksReady(func(tracks []ITrack) {
			for _, tr := range tracks {
				if tr.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addWriter(capturedClientID, tr)
				}
			}
		})
	}

	// hook joins after iterating existing clients
	r.OnClientJoined(func(c *Client) {
		clientID := c.ID()
		for _, track := range c.Tracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				_ = addWriter(clientID, track)
			}
		}
		c.OnTracksReady(func(tracks []ITrack) {
			for _, tr := range tracks {
				if tr.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addWriter(clientID, tr)
				}
			}
		})
	})

	r.recordingSession = session
	return id, nil
}

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

func (r *Room) StopRecording() error {
	r.recordingMu.Lock()
	session := r.recordingSession
	r.recordingMu.Unlock()
	if session == nil {
		return fmt.Errorf("no recording in progress")
	}

	fmt.Printf("stopping recording: %s\n", session.id)
	session.mu.Lock()
	session.meta.StopTime = time.Now()
	session.stopped = true
	session.mu.Unlock()

	// collect files
	session.mu.Lock()
	leftInputs := make([]string, 0, len(session.completedFiles.left)+len(session.recorders)/2)
	rightInputs := make([]string, 0, len(session.completedFiles.right)+len(session.recorders)/2)
	leftInputs = append(leftInputs, session.completedFiles.left...)
	rightInputs = append(rightInputs, session.completedFiles.right...)
	// close active recorders
	for key, rec := range session.recorders {
		fmt.Printf("  Closing active recorder %s\n", key)
		_ = rec.close()
		switch rec.channel {
		case ChannelOne:
			leftInputs = append(leftInputs, rec.filePath)
		case ChannelTwo:
			rightInputs = append(rightInputs, rec.filePath)
		}
	}
	session.mu.Unlock()

	// write meta
	metaFile := filepath.Join(session.cfg.BasePath, session.id, "meta.json")
	f, err := os.Create(metaFile)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(session.meta); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Close()

	outPath := filepath.Join(session.cfg.BasePath, session.id, "mixed.m4a")
	if err := mergeToStereoM4A(leftInputs, rightInputs, outPath); err != nil {
		return err
	}
	if err := uploadWithRetry(session.cfg.S3, outPath, session.id, session.meta.StartTime, uploadRetryAttempts, uploadRetryDelay); err != nil {
		return err
	}

	// cleanup async
	go func() {
		time.Sleep(10 * time.Second)
		_ = os.RemoveAll(filepath.Join(session.cfg.BasePath, session.id))
	}()

	r.recordingMu.Lock()
	r.recordingSession = nil
	r.recordingMu.Unlock()
	return nil
}

// mergeToStereoM4A and upload helpers are intentionally left mostly unchanged; they still
// rely on FFmpeg for mixing and MinIO client for S3 uploads. Kept for compatibility with
// existing post-processing.

func mergeToStereoM4A(leftInputs []string, rightInputs []string, outPath string) error {
	args := []string{"-y"}
	for _, in := range leftInputs {
		args = append(args, "-i", in)
	}
	for _, in := range rightInputs {
		args = append(args, "-i", in)
	}
	L := len(leftInputs)
	R := len(rightInputs)
	var filter string

	switch {
	case L == 0 && R == 0:
		args = append(args, "-f", "lavfi", "-t", "1", "-i", "anullsrc=r=48000:cl=stereo")
		args = append(args, "-c:a", "aac", "-b:a", "64k", outPath)
		cmd := exec.Command("ffmpeg", args...)
		return cmd.Run()
	case L > 0 && R > 0:
		// mix groups
		if L == 1 {
			filter += "[0:a]anull[l];"
		} else {
			var leftIns string
			for i := 0; i < L; i++ {
				leftIns += fmt.Sprintf("[%d:a]", i)
			}
			filter += fmt.Sprintf("%samix=inputs=%d:normalize=0[l];", leftIns, L)
		}
		if R == 1 {
			filter += fmt.Sprintf("[%d:a]anull[r];", L)
		} else {
			var rightIns string
			for i := 0; i < R; i++ {
				rightIns += fmt.Sprintf("[%d:a]", L+i)
			}
			filter += fmt.Sprintf("%samix=inputs=%d:normalize=0[r];", rightIns, R)
		}
		filter += "[l][r]amerge=inputs=2,pan=stereo|c0=c0|c1=c1[a]"
		args = append(args, "-filter_complex", filter, "-map", "[a]", "-c:a", "aac", "-b:a", "64k", outPath)
	case L > 0 && R == 0:
		if L == 1 {
			filter = "[0:a]pan=stereo|c0=c0|c1=c0[a]"
		} else {
			var leftIns string
			for i := 0; i < L; i++ {
				leftIns += fmt.Sprintf("[%d:a]", i)
			}
			filter = fmt.Sprintf("%samix=inputs=%d:normalize=0[left];[left]pan=stereo|c0=c0|c1=c0[a]", leftIns, L)
		}
		args = append(args, "-filter_complex", filter, "-map", "[a]", "-c:a", "aac", "-b:a", "64k", outPath)
	case L == 0 && R > 0:
		if R == 1 {
			filter = fmt.Sprintf("[%d:a]pan=stereo|c0=c0|c1=c0[a]", 0)
		} else {
			var rightIns string
			for i := 0; i < R; i++ {
				rightIns += fmt.Sprintf("[%d:a]", i)
			}
			filter = fmt.Sprintf("%samix=inputs=%d:normalize=0[right];[right]pan=stereo|c0=c0|c1=c0[a]", rightIns, R)
		}
		args = append(args, "-filter_complex", filter, "-map", "[a]", "-c:a", "aac", "-b:a", "64k", outPath)
	}

	cmd := exec.Command("ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg error: %v, stderr: %s", err, stderr.String())
	}
	return nil
}

func uploadWithRetry(cfg S3Config, outPath, sessionID string, startTime time.Time, attempts int, delay time.Duration) error {
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		return fmt.Errorf("incomplete S3 configuration")
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := uploadToS3(cfg, outPath, sessionID, startTime); err != nil {
			lastErr = err
			if i < attempts-1 {
				time.Sleep(delay)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("upload failed after %d attempts: %v", attempts, lastErr)
}

func uploadToS3(cfg S3Config, outPath, sessionID string, startTime time.Time) error {
	fileInfo, err := os.Stat(outPath)
	if err != nil {
		return fmt.Errorf("failed to stat output file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return fmt.Errorf("output file is empty")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
	})
	if err != nil {
		return fmt.Errorf("failed to create S3 client: %v", err)
	}

	dateStr := startTime.Format("02-01-2006")
	objectName := path.Join(cfg.FilePrefix, dateStr, sessionID+".m4a")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, err = client.FPutObject(ctx, cfg.Bucket, objectName, outPath, minio.PutObjectOptions{
		ContentType: "audio/mp4",
	})
	if err != nil {
		return fmt.Errorf("failed to upload to S3: %v", err)
	}
	return nil
}
