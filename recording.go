package sfu

import (
	"bytes"
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
	opus "gopkg.in/hraban/opus.v2"
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
}

// StartRecording begins recording audio tracks in the room according to the provided config.
func (r *Room) StartRecording(cfg RecordingConfig) (string, error) {
	// Validate config
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
		channel := cfg.ChannelMapping[clientID]
		if channel == ChannelUnknown {
			fmt.Printf("WARNING: Client %s not in channel mapping or mapped to ChannelUnknown, skipping recording\n", clientID)
			return nil
		}

		session.mu.Lock()
		defer session.mu.Unlock()

		fmt.Printf("adding writer for client %s, track %s\n", clientID, track.ID())

		trackDir := filepath.Join(baseDir, clientID)
		if err := os.MkdirAll(trackDir, 0755); err != nil {
			return err
		}

		key := clientID + "_" + track.ID()

		if _, ok := session.recorders[key]; !ok {
			// only record opus for now
			if track.Kind() != webrtc.RTPCodecTypeAudio {
				return nil
			}
			mime := track.MimeType()
			if mime != webrtc.MimeTypeOpus {
				// skip unsupported codec for now
				fmt.Printf("recording: skip codec %s for client %s track %s\n", mime, clientID, track.ID())
				return nil
			}

			filePath := filepath.Join(trackDir, fmt.Sprintf("%s.wav", track.ID()))

			rec, err := newTrackRecorder(filePath, channel)
			if err != nil {
				return err
			}
			session.recorders[key] = rec

			track.OnEnded(func() {
				session.mu.Lock()
				delete(session.recorders, key)
				session.mu.Unlock()
				_ = rec.close()
			})

			track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
				session.mu.Lock()
				paused := session.paused
				stopped := session.stopped
				session.mu.Unlock()
				if paused || stopped {
					return // Skip processing when paused/stopped
				}
				_ = rec.writeRTP(pkt)
			})
		}

		fmt.Printf("added writer for client %s, track %s\n", clientID, track.ID())

		return nil
	}

	// Subscribe existing clients' tracks
	clientCount := len(r.SFU().clients.GetClients())
	fmt.Printf("Recording starting: Found %d existing clients\n", clientCount)
	fmt.Printf("Channel mapping: %+v\n", cfg.ChannelMapping)
	for clientID, client := range r.SFU().clients.GetClients() {
		fmt.Printf("Client Loop: %s\n", clientID)
		tracks := client.Tracks()
		fmt.Printf("  Client %s has %d tracks\n", clientID, len(tracks))
		for _, track := range tracks {
			fmt.Printf("    Track %s: Kind=%v, MimeType=%s\n", track.ID(), track.Kind(), track.MimeType())
			// Remove goroutine to avoid race condition
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				if err := addWriter(clientID, track); err != nil {
					fmt.Printf("error adding writer for client %s, track %s: %v\n", clientID, track.ID(), err)
				}
			} else {
				fmt.Printf("    Skipping non-audio track %s\n", track.ID())
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
		fmt.Printf("Client Joined: %s\n", c.ID())
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

	fmt.Printf("closing writers: %s\n", session.id)
	fmt.Printf("Total recorders: %d\n", len(session.recorders))

	// close all track recorders and collect file paths
	session.mu.Lock()
	leftInputs := make([]string, 0, len(session.recorders)/2)
	rightInputs := make([]string, 0, len(session.recorders)/2)
	for key, rec := range session.recorders {
		fmt.Printf("  Closing recorder %s: channel=%v, samples=%d, bytes=%d\n", key, rec.channel, rec.samples, rec.dataBytes)
		if err := rec.close(); err != nil {
			fmt.Printf("error closing recorder: %v\n", err)
		}
		// Check if file has content
		if info, err := os.Stat(rec.filePath); err == nil {
			fmt.Printf("    File %s size: %d bytes\n", rec.filePath, info.Size())
		}
		switch rec.channel {
		case ChannelOne:
			leftInputs = append(leftInputs, rec.filePath)
		case ChannelTwo:
			rightInputs = append(rightInputs, rec.filePath)
		}
	}
	session.mu.Unlock()

	fmt.Printf("Left inputs: %d, Right inputs: %d\n", len(leftInputs), len(rightInputs))
	fmt.Printf("writing meta.json: %s\n", session.id)

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

	fmt.Printf("merging and uploading: %s\n", session.id)

	// Merge to stereo m4a 64k using ffmpeg then upload to S3
	outPath := filepath.Join(session.cfg.BasePath, session.id, "mixed.m4a")
	if err := mergeToStereoM4A(leftInputs, rightInputs, outPath); err != nil {
		return err
	}

	if err := uploadWithRetry(session.cfg.S3, outPath, session.id, session.meta.StartTime, uploadRetryAttempts, uploadRetryDelay); err != nil {
		return err
	}

	// Clean up temporary files
	go func() {
		time.Sleep(10 * time.Second) // Give time for any final operations
		_ = os.RemoveAll(filepath.Join(session.cfg.BasePath, session.id))
	}()

	r.recordingMu.Lock()
	r.recordingSession = nil
	r.recordingMu.Unlock()
	return nil
}

// trackRecorder records opus RTP into a 48kHz mono WAV file
type trackRecorder struct {
	mu         sync.Mutex
	file       *os.File
	filePath   string
	decoder    *opus.Decoder
	dataBytes  uint32
	samples    uint32
	closed     bool
	channel    ChannelType
	pcmBuffer  []int16 // Reuse buffer for PCM samples
	byteBuffer []byte  // Reuse buffer for byte conversion
}

func newTrackRecorder(path string, ch ChannelType) (*trackRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	dec, err := opus.NewDecoder(48000, 1)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	tr := &trackRecorder{
		file:       f,
		filePath:   path,
		decoder:    dec,
		channel:    ch,
		pcmBuffer:  make([]int16, 5760),  // 120ms at 48kHz
		byteBuffer: make([]byte, 5760*2), // 2 bytes per sample
	}
	if err := tr.writeWAVHeader(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return tr, nil
}

func (t *trackRecorder) writeWAVHeader() error {
	// 16-bit PCM mono, 48kHz
	// RIFF header with placeholder sizes, will fix on close
	var hdr bytes.Buffer
	// RIFF
	hdr.WriteString("RIFF")
	// ChunkSize placeholder
	writeLE32(&hdr, 36)
	hdr.WriteString("WAVE")
	// fmt chunk
	hdr.WriteString("fmt ")
	writeLE32(&hdr, 16)      // Subchunk1Size for PCM
	writeLE16(&hdr, 1)       // PCM format
	writeLE16(&hdr, 1)       // NumChannels = 1
	writeLE32(&hdr, 48000)   // SampleRate
	writeLE32(&hdr, 48000*2) // ByteRate = SampleRate * NumChannels * BitsPerSample/8
	writeLE16(&hdr, 2)       // BlockAlign = NumChannels * BitsPerSample/8
	writeLE16(&hdr, 16)      // BitsPerSample
	// data chunk
	hdr.WriteString("data")
	writeLE32(&hdr, 0) // Subchunk2Size placeholder
	_, err := t.file.Write(hdr.Bytes())
	return err
}

func writeLE16(w io.Writer, v uint16) {
	_ = binaryWrite(w, uint16(v))
}

func writeLE32(w io.Writer, v uint32) {
	_ = binaryWrite(w, uint32(v))
}

func binaryWrite(w io.Writer, v interface{}) error {
	var buf [4]byte
	switch x := v.(type) {
	case uint16:
		buf[0] = byte(x)
		buf[1] = byte(x >> 8)
		_, err := w.Write(buf[:2])
		return err
	case uint32:
		buf[0] = byte(x)
		buf[1] = byte(x >> 8)
		buf[2] = byte(x >> 16)
		buf[3] = byte(x >> 24)
		_, err := w.Write(buf[:4])
		return err
	default:
		return fmt.Errorf("unsupported type")
	}
}

func (t *trackRecorder) writeRTP(pkt *rtp.Packet) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	// Log first packet for each recorder
	if t.samples == 0 {
		fmt.Printf("First RTP packet received for recorder %s, payload size: %d\n", t.filePath, len(pkt.Payload))
	}
	// Decode Opus payload to PCM int16
	n, err := t.decoder.Decode(pkt.Payload, t.pcmBuffer)
	if err != nil {
		// Opus silence frame on decode error
		silence := []byte{0xF8, 0xFF, 0xFE}
		n, _ = t.decoder.Decode(silence, t.pcmBuffer)
		if n <= 0 {
			return nil
		}
	}
	if n <= 0 {
		return nil
	}
	// write PCM little endian
	// convert []int16 to []byte using pre-allocated buffer
	byteCount := n * 2
	for i := 0; i < n; i++ {
		v := uint16(t.pcmBuffer[i])
		t.byteBuffer[2*i] = byte(v)
		t.byteBuffer[2*i+1] = byte(v >> 8)
	}
	if _, err := t.file.Write(t.byteBuffer[:byteCount]); err != nil {
		return err
	}
	t.dataBytes += uint32(byteCount)
	t.samples += uint32(n)
	return nil
}

func (t *trackRecorder) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	// Fix header sizes
	// ChunkSize = 36 + Subchunk2Size
	chunkSize := 36 + t.dataBytes
	// write ChunkSize at offset 4
	if _, err := t.file.Seek(4, 0); err == nil {
		_ = binaryWrite(t.file, uint32(chunkSize))
	}
	// write Subchunk2Size at offset 40
	if _, err := t.file.Seek(40, 0); err == nil {
		_ = binaryWrite(t.file, uint32(t.dataBytes))
	}
	err := t.file.Close()
	t.closed = true
	return err
}

func mergeToStereoM4A(leftInputs []string, rightInputs []string, outPath string) error {
	// Build ffmpeg command
	args := []string{"-y"}
	inputCount := 0
	for _, in := range leftInputs {
		args = append(args, "-i", in)
		inputCount++
	}
	for _, in := range rightInputs {
		args = append(args, "-i", in)
		inputCount++
	}

	var filter string
	// Indices: 0..L-1 left, L..L+R-1 right
	L := len(leftInputs)
	R := len(rightInputs)

	switch {
	case L == 0 && R == 0:
		// Nothing to merge; create silent 1s stereo
		args = append(args, "-f", "lavfi", "-t", "1", "-i", "anullsrc=r=48000:cl=stereo")
		filter = "anull"
		inputCount++
		args = append(args, "-c:a", "aac", "-b:a", "64k", outPath)
		cmd := exec.Command("ffmpeg", args...)
		return cmd.Run()

	case L > 0 && R > 0:
		// Mix left group if needed
		if L == 1 {
			filter += "[0:a]anull[l];"
		} else {
			// build amix for left
			var leftIns string
			for i := 0; i < L; i++ {
				leftIns += fmt.Sprintf("[%d:a]", i)
			}
			filter += fmt.Sprintf("%samix=inputs=%d:normalize=0[l];", leftIns, L)
		}
		// Mix right group if needed
		if R == 1 {
			// right input index base is L
			filter += fmt.Sprintf("[%d:a]anull[r];", L)
		} else {
			var rightIns string
			for i := 0; i < R; i++ {
				rightIns += fmt.Sprintf("[%d:a]", L+i)
			}
			filter += fmt.Sprintf("%samix=inputs=%d:normalize=0[r];", rightIns, R)
		}
		// Merge to stereo
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
			filter = fmt.Sprintf("[%d:a]pan=stereo|c0=c0|c1=c0[a]", 0) // Only one input which is right index 0 when L==0
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
	// Validate S3 config
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
	// Check if file exists
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
