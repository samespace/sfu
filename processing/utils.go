package processing

func getRoomTracks(room roomData) []clientTrack {
	tracks := make([]clientTrack, 0)

	for _, c := range room.Clients {
		for _, t := range c.Tracks {
			tracks = append(tracks, t)
		}
	}

	return tracks
}
