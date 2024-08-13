package processing

import (
	"fmt"
	"strings"
	"time"
)

/*
x``
ffmpeg -f lavfi -t 4.234 -i anullsrc=r=8000:cl=mono -i audio.wav -filter_complex "[0] [1] concat=n=2:v=0:a=1 [a]" -map "[a]" silenced.wav
*/

func ProcessRoom(recDir, roomId string) error {
	room, err := readRoom(recDir, roomId)
	if err != nil {
		return fmt.Errorf("failed to read room: %w", err)
	}

	for _, client := range room.Clients {
		if err := processClient(client); err != nil {
			fmt.Printf("Error processing client %s: %v\n", client.ClientID, err)
		}
	}

	if err = mergeTracks(recDir, *room); err != nil {
		return err
	}

	return nil
}

func processClient(c roomClient) error {
	if c.Tracks == nil {
		return fmt.Errorf("no tracks found for client %s", c.ClientID)
	}
	if err := convertTracks(c.Tracks); err != nil {
		return fmt.Errorf("error converting tracks: %w", err)
	}

	return nil
}

func convertTracks(tracks []clientTrack) error {
	for _, track := range tracks {
		var outputFileName string
		var err error

		switch track.AudioType {
		case audioTypeOpus:
			outputFileName = strings.TrimSuffix(track.TrackFileName, opusExtension) + wavExtension
			err = opusToPcm(track.TrackFileName, outputFileName)

		case audioTypeULaw, audioTypeALaw:
			outputFileName = strings.TrimSuffix(track.TrackFileName, pcmuExtention)
			outputFileName = strings.TrimSuffix(outputFileName, pcmaExtention) + wavExtension
			err = pcmToLPcm(track.AudioType, track.TrackFileName, outputFileName)

		default:
			err = fmt.Errorf("unsupported audio type: %s", track.AudioType)
		}

		if err != nil {
			fmt.Printf("Error converting track %s: %v\n", track.TrackFileName, err)
		}
	}

	return nil
}

func mergeTracks(recDir string, room roomData) error {
	var tracks = getRoomTracks(room)
	var earliestTimestamp time.Time
	var trackOffsets = make(map[string]float32)

	earliestTimestamp = time.Now().Add(24 * time.Hour)

	for _, t := range tracks {
		if t.StartTime.Before(earliestTimestamp) {
			earliestTimestamp = t.StartTime
		}
	}

	for _, t := range tracks {
		offset := int(t.StartTime.Sub(earliestTimestamp).Milliseconds())
		trackOffsets[t.TrackID] = float32(offset)
	}

	addSilence(tracks, trackOffsets)

	mixInputs := []string{}

	for _, t := range tracks {
		in := t.TrackFileName
		for _, n := range []string{"audio.pcma", "audio.pcmu", "audio.opus"} {
			in = strings.Replace(in, n, "offset.wav", 1)
		}
		mixInputs = append(mixInputs, in)
	}
	err := mixAudio(mixInputs, fmt.Sprintf("%s/%s/merged.wav", recDir, room.RoomId))

	if err != nil {
		return err
	}

	return nil
}

func addSilence(tracks []clientTrack, offsets map[string]float32) {
	for _, t := range tracks {
		in := t.TrackFileName
		out := t.TrackFileName
		for _, n := range []string{"audio.pcma", "audio.pcmu", "audio.opus"} {
			out = strings.ReplaceAll(out, n, "offset.wav")
		}
		for _, n := range []string{"audio.pcma", "audio.pcmu", "audio.opus"} {
			in = strings.ReplaceAll(in, n, "audio.wav")
		}
		err := addAudioSilence(in, out, offsets[t.TrackID])
		if err != nil {
			fmt.Println("err adding audio silence: %w", err)
		}
	}
}
