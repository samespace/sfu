package sfu

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	rtcp "github.com/pion/rtcp"
)

// TestRtpToNtpUsingSR validates fixed-point conversion accuracy for 20ms at 48kHz
func TestRtpToNtpUsingSR(t *testing.T) {
	m := &Mixer{
		sr:         make(map[uint32]*SRInfo),
		sampleRate: uint64(SampleRate),
	}

	ssrc := uint32(1234)
	baseRTP := uint32(1_000_000)
	baseNTP := timeToNTP(time.Now())

	m.UpdateSR(&rtcp.SenderReport{SSRC: ssrc, RTPTime: baseRTP, NTPTime: baseNTP})

	// 960 samples -> 20ms at 48kHz
	pktRTP := baseRTP + uint32(FrameSamples)
	gotNTP, ok := m.rtpToNTPUsingSR(ssrc, pktRTP)
	if !ok {
		t.Fatalf("no SR mapping")
	}
	// Expect ~20ms delta
	got := ntpToTime(gotNTP).Sub(ntpToTime(baseNTP))
	if d := got - 20*time.Millisecond; d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("delta not ~20ms: got %v", got)
	}
}

// Minimal compatible struct so tests don't import rtcp directly here

// TestMixerWritesM4A runs the mixer with ffmpeg if present and verifies output file exists
func TestMixerWritesM4A(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found in PATH; skipping integration test")
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "mix.m4a")

	m, err := NewMixer(out, DefaultBatchMS, DefaultSafetyMS, 2)
	if err != nil {
		t.Fatalf("NewMixer: %v", err)
	}

	// Create SR for two SSRCs
	ssrcL := uint32(111)
	ssrcR := uint32(222)
	baseRTP := uint32(10_000)
	baseNTP := timeToNTP(time.Now())

	m.UpdateSR(&rtcp.SenderReport{SSRC: ssrcL, RTPTime: baseRTP, NTPTime: baseNTP})
	m.UpdateSR(&rtcp.SenderReport{SSRC: ssrcR, RTPTime: baseRTP, NTPTime: baseNTP})

	// Prepare a single 20ms frame of constant tone
	samples := make([]int16, FrameSamples)
	for i := range samples {
		samples[i] = 1000
	}

	// Send one frame for left and right
	m.incoming <- &DecodedFrame{SSRC: ssrcL, RTPTime: baseRTP, Samples: samples, SamplesN: FrameSamples, Channel: 0}
	m.incoming <- &DecodedFrame{SSRC: ssrcR, RTPTime: baseRTP, Samples: samples, SamplesN: FrameSamples, Channel: 1}

	// Allow flush loop to run
	time.Sleep(300 * time.Millisecond)

	_ = m.Close()

	// Verify file exists and non-empty
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if st.Size() == 0 {
		t.Fatalf("output file is empty")
	}
}
