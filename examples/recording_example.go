// Package recording_example demonstrates how to use the SFU recording feature
package main

import (
	"fmt"
	"log"
	"time"

	sfu "github.com/inlivedev/sfu"
)

// This example demonstrates how to properly configure and use the recording feature
func recordingExample() {
	// Assume you have a room with clients already connected
	// For this example, let's say we have clients with IDs: "user1", "user2", "user3"

	// Create recording configuration
	recordingConfig := sfu.RecordingConfig{
		// Base path where recordings will be saved
		BasePath: "/tmp/recordings",

		// Channel mapping - CRITICAL: Map each client ID to a channel
		// ChannelOne: Left channel in stereo output
		// ChannelTwo: Right channel in stereo output
		// ChannelUnknown: Will not be recorded
		ChannelMapping: map[string]sfu.ChannelType{
			"user1": sfu.ChannelOne, // Will be recorded on left channel
			"user2": sfu.ChannelTwo, // Will be recorded on right channel
			"user3": sfu.ChannelOne, // Also on left channel (will be mixed with user1)
			// Any client not listed here will be ignored
		},

		// S3 configuration for uploading (optional)
		S3: sfu.S3Config{
			Secure:     true,
			Endpoint:   "s3.amazonaws.com",
			AccessKey:  "your-access-key",
			SecretKey:  "your-secret-key",
			Bucket:     "your-bucket-name",
			FilePrefix: "recordings",
		},
	}

	// Assuming you have a room instance
	var room *sfu.Room

	// Start recording
	recordingID, err := room.StartRecording(recordingConfig)
	if err != nil {
		log.Fatalf("Failed to start recording: %v", err)
	}

	fmt.Printf("Recording started with ID: %s\n", recordingID)

	// The recording will now capture:
	// - All audio tracks from "user1" mixed to the left channel
	// - All audio tracks from "user2" to the right channel
	// - All audio tracks from "user3" mixed with user1 on the left channel
	// - Audio from any other users will be ignored

	// Let the recording run for some time
	time.Sleep(30 * time.Second)

	// Optionally pause recording
	if err := room.PauseRecording(); err != nil {
		log.Printf("Failed to pause recording: %v", err)
	}

	// Resume recording
	if err := room.ResumeRecording(); err != nil {
		log.Printf("Failed to resume recording: %v", err)
	}

	// Stop recording when done
	if err := room.StopRecording(); err != nil {
		log.Fatalf("Failed to stop recording: %v", err)
	}

	fmt.Println("Recording stopped successfully")
}

// Common issues and solutions:
//
// 1. Getting 1-second silence file:
//    - Check that ChannelMapping includes the client IDs that are sending audio
//    - Verify clients are actually sending audio tracks (not just video)
//    - Ensure clients are using Opus codec for audio
//    - Make sure recording starts AFTER clients have joined and started sending media
//
// 2. Missing audio from some clients:
//    - Add their client IDs to the ChannelMapping
//    - Check debug logs to see if their tracks are being detected
//
// 3. Recording not starting:
//    - Ensure BasePath directory exists and is writable
//    - Check that ChannelMapping is not empty
//    - Verify no other recording is already in progress

// Debug tips:
// The enhanced debug logging will show:
// - Number of existing clients when recording starts
// - The channel mapping configuration
// - Each client's tracks (audio/video, codec)
// - Whether tracks are being added to recorders
// - Number of recorders created
// - File sizes when recording stops
// - Number of left/right channel inputs for mixing

func main() {
	// This example shows how to configure recording
	// In a real application, you would call this after setting up your room and clients
	fmt.Println("See recordingExample() function for usage details")
}
