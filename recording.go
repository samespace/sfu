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
	mu      sync.Mutex
	paused  bool
	stopped bool
	meta    struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
	}

	channelOneMixer *Mixer
	channelTwoMixer *Mixer
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
		id:  id,
		cfg: cfg,
	}
	session.meta.StartTime = startTime

	baseDir := filepath.Join(cfg.BasePath, id)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", err
	}

	// Record client join/leave events
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

		// check if the track is already being recorded in any of the channel or not
		var source *Source
		switch channel {
		case ChannelOne:
			if session.channelOneMixer == nil {
				mixer, err := NewMixer(filepath.Join(baseDir, "one.pcm"))
				if err != nil {
					return err
				}
				session.channelOneMixer = mixer
				// start the mixer
				session.channelOneMixer.Start()
			}
			if session.channelOneMixer.GetSource(clientID) == nil {
				src, err := session.channelOneMixer.AddSource(clientID)
				if err != nil {
					return err
				}
				source = src
			} else {
				fmt.Printf("source already exists for client %s, track %s", clientID, track.ID())
				return nil
			}
		case ChannelTwo:
			if session.channelTwoMixer == nil {
				mixer, err := NewMixer(filepath.Join(baseDir, "two.pcm"))
				if err != nil {
					return err
				}
				session.channelTwoMixer = mixer
				// start the mixer
				session.channelTwoMixer.Start()
			}
			if session.channelTwoMixer.GetSource(clientID) == nil {
				src, err := session.channelTwoMixer.AddSource(clientID)
				if err != nil {
					return err
				}
				source = src
			} else {
				fmt.Printf("source already exists for client %s, track %s", clientID, track.ID())
				return nil
			}
		}

		if source == nil {
			return fmt.Errorf("source not found for client %s, track %s", clientID, track.ID())
		}

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			session.mu.Lock()
			stopped := session.stopped
			paused := session.paused
			session.mu.Unlock()

			if stopped || paused {
				return
			}

			if pkt != nil && source != nil && source.jb != nil {
				cloned := pkt.Clone()
				if cloned != nil {
					source.jb.push(cloned)
				}
			}
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
					if err := addWriter(clientID, track); err != nil {
						fmt.Printf("error adding writer for client %s, track %s: %v", clientID, track.ID(), err)
					}
				}
			}
		})
	}

	// Hook future client additions - consolidate with metadata recording
	r.OnClientJoined(func(c *Client) {
		fmt.Printf("Client Joined: %s", c.ID())

		// Record client join event in metadata
		session.mu.Lock()
		session.meta.Events = append(session.meta.Events, Event{
			Type: "client_join",
			Time: time.Now(),
			Data: map[string]interface{}{"client_id": c.ID()},
		})
		session.mu.Unlock()

		// Handle existing tracks
		for _, track := range c.Tracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				if err := addWriter(c.ID(), track); err != nil {
					fmt.Printf("error adding writer for client %s, track %s: %v", c.ID(), track.ID(), err)
				}
			}
		}

		// add a hook for add track too
		c.OnTracksReady(func(tracks []ITrack) {
			for _, track := range tracks {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					if err := addWriter(c.ID(), track); err != nil {
						fmt.Printf("error adding writer for client %s, track %s: %v", c.ID(), track.ID(), err)
					}
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

	session := r.recordingSession
	if session == nil {
		return fmt.Errorf("no recording in progress")
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.stopped {
		return fmt.Errorf("recording already stopped")
	}

	if session.paused {
		return fmt.Errorf("recording already paused")
	}

	session.paused = true
	session.meta.Events = append(session.meta.Events, Event{Type: "pause", Time: time.Now(), Data: nil})
	return nil
}

// ResumeRecording resumes writing RTP packets to files.
func (r *Room) ResumeRecording() error {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()

	session := r.recordingSession
	if session == nil {
		return fmt.Errorf("no recording in progress")
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.stopped {
		return fmt.Errorf("recording already stopped")
	}

	if !session.paused {
		return fmt.Errorf("recording is not paused")
	}

	session.paused = false
	session.meta.Events = append(session.meta.Events, Event{Type: "resume", Time: time.Now(), Data: nil})
	return nil
}

// StopRecording stops the recording session, closes files, and writes metadata.
func (r *Room) StopRecording() error {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()

	session := r.recordingSession
	if session == nil {
		return fmt.Errorf("no recording in progress")
	}

	fmt.Printf("stopping recording: %s", session.id)

	session.mu.Lock()

	// Check if already stopped to avoid double-stop
	if session.stopped {
		session.mu.Unlock()
		return fmt.Errorf("recording already stopped")
	}

	session.meta.StopTime = time.Now()
	session.stopped = true

	oneExists := false
	twoExists := false

	// stop the mixers
	if session.channelOneMixer != nil {
		oneExists = true
		session.channelOneMixer.Stop()
	}
	if session.channelTwoMixer != nil {
		twoExists = true
		session.channelTwoMixer.Stop()
	}

	// Copy meta data before unlocking to avoid race conditions
	metaCopy := session.meta
	sessionID := session.id
	basePath := session.cfg.BasePath
	s3Config := session.cfg.S3
	startTime := session.meta.StartTime

	session.mu.Unlock()

	fmt.Printf("writing meta.json: %s", sessionID)

	// Write meta.json
	metaFile := filepath.Join(basePath, sessionID, "meta.json")
	f, err := os.Create(metaFile)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(metaCopy); err != nil {
		return err
	}

	// merge and upload
	go r.mergeAndUpload(basePath, sessionID, oneExists, twoExists, s3Config, startTime)

	r.recordingSession = nil
	return nil
}

func (r *Room) mergeAndUpload(basePath string, id string, oneExists bool, twoExists bool, s3 S3Config, startTime time.Time) error {
	outputPath := filepath.Join(basePath, id, "output.m4a")

	if oneExists && twoExists {
		ffmpegCmd := exec.Command("ffmpeg", "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", filepath.Join(basePath, id, "one.pcm"),
			"-f", "s16le", "-ar", "48000", "-ac", "1", "-i", filepath.Join(basePath, id, "two.pcm"),
			"-filter_complex", "[0:a]afftdn[a0];[1:a]afftdn[a1];[a0][a1]join=inputs=2:channel_layout=stereo[a]",
			"-map", "[a]", "-c:a", "aac", "-b:a", "32k", outputPath)
		if err := ffmpegCmd.Run(); err != nil {
			fmt.Printf("error running ffmpeg for stereo merge: %v", err)
			return err
		}
	} else if oneExists && !twoExists {
		ffmpegCmd := exec.Command("ffmpeg", "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", filepath.Join(basePath, id, "one.pcm"),
			"-af", "afftdn", "-c:a", "aac", "-b:a", "32k", outputPath)
		if err := ffmpegCmd.Run(); err != nil {
			fmt.Printf("error running ffmpeg for channel one: %v", err)
			return err
		}
	} else if !oneExists && twoExists {
		ffmpegCmd := exec.Command("ffmpeg", "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", filepath.Join(basePath, id, "two.pcm"),
			"-af", "afftdn", "-c:a", "aac", "-b:a", "32k", outputPath)
		if err := ffmpegCmd.Run(); err != nil {
			fmt.Printf("error running ffmpeg for channel two: %v", err)
			return err
		}
	} else {
		// No audio channels to process
		fmt.Printf("no audio channels to process for recording %s", id)
		return nil
	}

	// Upload to S3 only if we have a valid output file
	if _, err := os.Stat(outputPath); err != nil {
		fmt.Printf("output file not found, skipping S3 upload: %v", err)
		return err
	}

	mc, err := minio.New(s3.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s3.AccessKey, s3.SecretKey, ""),
		Secure: s3.Secure,
	})
	if err != nil {
		fmt.Printf("error creating minio client: %v", err)
		return err
	}

	dateStr := startTime.Format("02-01-2006")
	object := path.Join(s3.FilePrefix, dateStr, id+".m4a")
	ctx := context.Background()

	_, err = mc.FPutObject(ctx, s3.Bucket, object, outputPath, minio.PutObjectOptions{ContentType: "audio/mp4"})
	if err != nil {
		fmt.Printf("error uploading to s3: %v", err)
		return err
	}
	fmt.Println("uploaded to s3: ", object)

	// Cleanup local files - happens only if the upload is successful
	fmt.Println("removing local files: ", filepath.Join(basePath, id))
	os.RemoveAll(filepath.Join(basePath, id))

	return nil
}
