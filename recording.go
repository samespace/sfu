package sfu

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"gopkg.in/hraban/opus.v2"
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

		decoder, err := opus.NewDecoder(48000, 1)
		if err != nil {
			return err
		}

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			if session.paused || session.stopped {
				return
			}

			out := make([]int16, SamplesPerFrame)
			n, err := decoder.Decode(pkt.Payload, out)
			if err != nil {
				return
			}
			source.pcmCh <- out[:n]
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
	defer r.recordingMu.Unlock()

	session := r.recordingSession
	if session == nil {
		return fmt.Errorf("no recording in progress")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.meta.StopTime = time.Now()
	session.stopped = true

	// stop the mixers
	session.channelOneMixer.Stop()
	session.channelTwoMixer.Stop()

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

	r.recordingSession = nil
	return nil
}
