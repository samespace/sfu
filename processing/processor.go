package processing

import (
	"fmt"
	"strings"
)

func ProcessRoom(roomId string) error {
	room, err := readRoom(roomId)
	if err != nil {
		return fmt.Errorf("failed to read room: %w", err)
	}

	for _, client := range room.Clients {
		if err := processClient(client); err != nil {
			fmt.Printf("Error processing client %s: %v\n", client.ClientID, err)
		}
	}

	return nil
}

func processClient(c roomClient) error {
	if c.Tracks == nil {
		return fmt.Errorf("no tracks found for client %s", c.ClientID)
	}
	return convertTracks(c.Tracks)
}

func convertTracks(tracks []clientTrack) error {
	for _, track := range tracks {
		var outputFileName string
		var err error

		// Determine output file name based on the track's audio type
		switch track.AudioType {
		case audioTypeOpus:
			outputFileName = strings.TrimSuffix(track.TrackFileName, opusExtension) + wavExtension
			err = opusToPcm(track.TrackFileName, outputFileName)

		case audioTypeULaw, audioTypeALaw:
			// Assumes that ulaw and alaw files have a .raw extension
			outputFileName = strings.TrimSuffix(track.TrackFileName, pcmuExtention)
			outputFileName = strings.TrimSuffix(outputFileName, pcmaExtention) + wavExtension
			err = pcmToLPcm(track.AudioType, track.TrackFileName, outputFileName)

		default:
			err = fmt.Errorf("unsupported audio type: %s", track.AudioType)
		}

		if err != nil {
			fmt.Printf("Error converting track %s: %v\n", track.TrackFileName, err)
		} else {
			fmt.Printf("Successfully converted %s to %s\n", track.TrackFileName, outputFileName)
		}
	}

	return nil
}
