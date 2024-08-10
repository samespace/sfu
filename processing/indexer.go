package processing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type audioType string

type RecordMetadata struct {
	StartTime time.Time `json:"start_time"`
}

type clientTrack struct {
	TrackID       string
	TrackFileName string
	AudioType     audioType
	StartTime     time.Time
	MetaData      RecordMetadata
}

type roomClient struct {
	ClientID string
	Tracks   []clientTrack
}

type roomData struct {
	RoomId  string
	Clients []roomClient
}

const (
	audioTypeOpus audioType = "opus"
	audioTypeULaw audioType = "pcmu"
	audioTypeALaw audioType = "pcma"
)

const (
	opusExtension = ".opus"
	pcmuExtention = ".pcmu"
	pcmaExtention = ".pcma"
	wavExtension  = ".wav"
)

// readMetadata reads metadata from a JSON file.
func readMetadata(filePath string) (*RecordMetadata, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open metadata file %s: %w", filePath, err)
	}
	defer file.Close()

	var metaData RecordMetadata
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&metaData); err != nil {
		return nil, fmt.Errorf("failed to decode metadata file %s: %w", filePath, err)
	}

	return &metaData, nil
}

// readRoom processes the room directory to extract roomData.
func readRoom(roomId string) (*roomData, error) {
	roomDir := filepath.Join("recordings", roomId)
	if _, err := os.Stat(roomDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("room directory does not exist: %s", roomDir)
	}

	room := &roomData{
		RoomId:  roomId,
		Clients: []roomClient{},
	}

	// Read the room directory
	clientDirs, err := os.ReadDir(roomDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read room directory %s: %w", roomDir, err)
	}

	for _, clientDir := range clientDirs {
		if !clientDir.IsDir() {
			continue
		}

		clientId := clientDir.Name()
		clientDirPath := filepath.Join(roomDir, clientId)
		client := roomClient{ClientID: clientId}

		// Read the client directory
		trackDirs, err := os.ReadDir(clientDirPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read client directory %s: %w", clientDirPath, err)
		}

		clientTracks := []clientTrack{}

		for _, trackDir := range trackDirs {
			if !trackDir.IsDir() {
				continue
			}

			trackDirPath := filepath.Join(clientDirPath, trackDir.Name())
			metaFilePath := filepath.Join(trackDirPath, "meta.json")

			// Read metadata file
			metaData, err := readMetadata(metaFilePath)
			if err != nil {
				return nil, fmt.Errorf("failed to read metadata for track %s: %w", trackDir.Name(), err)
			}

			// Handle audio files
			audioFilePaths := []struct {
				name      string
				audioType audioType
			}{
				{filepath.Join(trackDirPath, "audio.opus"), audioTypeOpus},
				{filepath.Join(trackDirPath, "audio.pcmu"), audioTypeULaw},
				{filepath.Join(trackDirPath, "audio.pcma"), audioTypeALaw},
			}

			for _, audioFilePath := range audioFilePaths {
				if _, err := os.Stat(audioFilePath.name); err == nil {
					clientTracks = append(clientTracks, clientTrack{
						TrackID:       trackDir.Name(),
						TrackFileName: audioFilePath.name,
						AudioType:     audioFilePath.audioType,
						StartTime:     metaData.StartTime,
						MetaData:      *metaData,
					})
				}
			}
		}

		client.Tracks = clientTracks
		room.Clients = append(room.Clients, client)
	}

	return room, nil
}
