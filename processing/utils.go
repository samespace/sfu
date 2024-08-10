package processing

func getRoomTracks(room roomData) []clientTrack {
	tracks := make([]clientTrack, 0)

	for _, c := range room.Clients {
		tracks = append(tracks, c.Tracks...)
	}

	return tracks
}
