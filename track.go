package sfu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inlivedev/sfu/pkg/interceptors/voiceactivedetector"
	"github.com/inlivedev/sfu/pkg/networkmonitor"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	TrackTypeMedia  = "media"
	TrackTypeScreen = "screen"
)

var (
	ErrTrackExists      = errors.New("client: error track already exists")
	ErrTrackIsNotExists = errors.New("client: error track is not exists")
)

type TrackType string

func (t TrackType) String() string {
	return string(t)
}

type baseTrack struct {
	id           string
	msid         string
	streamid     string
	client       *Client
	isProcessed  bool
	kind         webrtc.RTPCodecType
	codec        webrtc.RTPCodecParameters
	isScreen     *atomic.Bool // source of the track, can be media or screen
	clientTracks *clientTrackList
	pool         *rtppool.RTPPool
}

type ITrack interface {
	ID() string
	StreamID() string
	ClientID() string
	IsSimulcast() bool
	IsScaleable() bool
	IsProcessed() bool
	SetSourceType(TrackType)
	SourceType() TrackType
	SetAsProcessed()
	OnRead(func(interceptor.Attributes, *rtp.Packet, QualityLevel))
	IsScreen() bool
	IsRelay() bool
	Kind() webrtc.RTPCodecType
	MimeType() string
	TotalTracks() int
	Context() context.Context
	Relay(func(webrtc.SSRC, interceptor.Attributes, *rtp.Packet))
	PayloadType() webrtc.PayloadType
	OnEnded(func())
}

type Track struct {
	context          context.Context
	mu               sync.Mutex
	base             *baseTrack
	remoteTrack      *remoteTrack
	onEndedCallbacks []func()
	onReadCallbacks  []func(interceptor.Attributes, *rtp.Packet, QualityLevel)
}

type AudioTrack struct {
	*Track
	vad          *voiceactivedetector.VoiceDetector
	vadCallbacks []func([]voiceactivedetector.VoicePacketData)
}

func newTrack(ctx context.Context, client *Client, trackRemote IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), stats stats.Getter, onStatsUpdated func(*stats.Stats)) ITrack {
	ctList := newClientTrackList()
	pool := rtppool.New()
	baseTrack := &baseTrack{
		id:           trackRemote.ID(),
		isScreen:     &atomic.Bool{},
		msid:         trackRemote.Msid(),
		streamid:     trackRemote.StreamID(),
		client:       client,
		kind:         trackRemote.Kind(),
		codec:        trackRemote.Codec(),
		clientTracks: ctList,
		pool:         pool,
	}

	t := &Track{
		mu:               sync.Mutex{},
		base:             baseTrack,
		onReadCallbacks:  make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0),
		onEndedCallbacks: make([]func(), 0),
	}

	onRead := func(attrs interceptor.Attributes, p *rtp.Packet) {
		tracks := t.base.clientTracks.GetTracks()

		for _, track := range tracks {
			//nolint:ineffassign,staticcheck // packet is from the pool
			packet := pool.CopyPacket(p)

			track.push(packet, QualityHigh)

			pool.PutPacket(packet)
		}

		//nolint:ineffassign // this is required
		packet := pool.CopyPacket(p)

		t.onRead(attrs, packet, QualityHigh)

		pool.PutPacket(packet)
	}

	onNetworkConditionChanged := func(condition networkmonitor.NetworkConditionType) {
		client.onNetworkConditionChanged(condition)
	}

	t.remoteTrack = newRemoteTrack(ctx, client.log, client.options.ReorderPackets, trackRemote, minWait, maxWait, pliInterval, onPLI, stats, onStatsUpdated, onRead, pool, onNetworkConditionChanged)

	var cancel context.CancelFunc

	t.context, cancel = context.WithCancel(client.Context())

	t.remoteTrack.OnEnded(func() {
		cancel()
		t.onEnded()
	})

	if trackRemote.Kind() == webrtc.RTPCodecTypeAudio {
		ta := &AudioTrack{
			Track: t,
		}

		return ta
	}

	return t
}

func (t *Track) ClientID() string {
	return t.base.client.id
}

func (t *Track) Context() context.Context {
	return t.context
}

func (t *Track) createLocalTrack() *webrtc.TrackLocalStaticRTP {
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(t.remoteTrack.track.Codec().RTPCodecCapability, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	return track
}

func (t *Track) createOpusLocalTrack() *webrtc.TrackLocalStaticRTP {
	c := t.remoteTrack.track.Codec().RTPCodecCapability
	c.MimeType = webrtc.MimeTypeOpus
	c.SDPFmtpLine = "minptime=10;useinbandfec=1"
	track, newTrackErr := webrtc.NewTrackLocalStaticRTP(c, t.base.id, t.base.streamid)
	if newTrackErr != nil {
		panic(newTrackErr)
	}

	return track
}

func (t *Track) ID() string {
	return t.base.id
}

func (t *Track) StreamID() string {
	return t.base.streamid
}

func (t *Track) SSRC() webrtc.SSRC {
	return t.remoteTrack.track.SSRC()
}

func (t *AudioTrack) SetVAD(vad *voiceactivedetector.VoiceDetector) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.vad = vad
	vad.OnVoiceDetected(func(pkts []voiceactivedetector.VoicePacketData) {
		// send through datachannel
		t.onVoiceDetected(pkts)
	})
}

func (t *AudioTrack) onVoiceDetected(pkts []voiceactivedetector.VoicePacketData) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, callback := range t.vadCallbacks {
		callback(pkts)
	}
}

func (t *AudioTrack) OnVoiceDetected(callback func(pkts []voiceactivedetector.VoicePacketData)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.vadCallbacks = append(t.vadCallbacks, callback)
}

func (t *AudioTrack) subscribe(c *Client) iClientTrack {
	var ct iClientTrack

	cta := newClientTrackAudio(c, t)

	if t.PayloadType() == 63 {
		t.base.client.log.Tracef("track: red enabled %v", c.receiveRED)

		// TODO: detect if client supports RED and it's audio then send RED encoded packets
		ct = newClientTrackRed(cta)
	} else {
		ct = cta
	}

	t.base.clientTracks.Add(ct)

	return ct
}

func (t *Track) RemoteTrack() *remoteTrack {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.remoteTrack
}

func (t *Track) IsScreen() bool {
	return t.base.isScreen.Load()
}

func (t *Track) IsSimulcast() bool {
	return false
}

func (t *Track) IsScaleable() bool {
	// Audio tracks are not scaleable in the same way as video
	return false
}

func (t *Track) IsProcessed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.base.isProcessed
}

func (t *Track) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *Track) MimeType() string {
	return t.base.codec.MimeType
}

func (t *Track) SSRCHigh() webrtc.SSRC {
	return t.remoteTrack.Track().SSRC()
}

func (t *Track) SSRCMid() webrtc.SSRC {
	return t.remoteTrack.Track().SSRC()
}

func (t *Track) SSRCLow() webrtc.SSRC {
	return t.remoteTrack.Track().SSRC()
}

func (t *Track) TotalTracks() int {
	return 1
}

func (t *Track) subscribe(c *Client) iClientTrack {
	// Only audio tracks are supported in voice-only SFU
	ct := newClientTrack(c, t, t.IsScreen(), nil)
	t.base.clientTracks.Add(ct)
	return ct
}

func (t *Track) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *Track) SourceType() TrackType {
	if t.base.isScreen.Load() {
		return TrackTypeScreen
	}

	return TrackTypeMedia
}

func (t *Track) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.base.isProcessed = true
}

func (t *Track) OnRead(callback func(interceptor.Attributes, *rtp.Packet, QualityLevel)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onReadCallbacks = append(t.onReadCallbacks, callback)
}

func (t *Track) onRead(attrs interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
	callbacks := make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0)

	t.mu.Lock()
	callbacks = append(callbacks, t.onReadCallbacks...)
	t.mu.Unlock()

	for _, callback := range callbacks {
		copyPacket := t.base.pool.CopyPacket(p)
		callback(attrs, copyPacket, quality)
		t.base.pool.PutPacket(copyPacket)
	}
}

func (t *Track) Relay(f func(webrtc.SSRC, interceptor.Attributes, *rtp.Packet)) {
	t.OnRead(func(attrs interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
		f(t.SSRC(), attrs, p)
	})
}

func (t *Track) PayloadType() webrtc.PayloadType {
	return t.base.codec.PayloadType
}

func (t *Track) IsRelay() bool {
	return t.remoteTrack.IsRelay()
}

func (t *Track) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *Track) onEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, f := range t.onEndedCallbacks {
		f()
	}
}

type SubscribeTrackRequest struct {
	ClientID string `json:"client_id"`
	TrackID  string `json:"track_id"`
}

type trackList struct {
	tracks map[string]ITrack
	mu     sync.RWMutex
	log    logging.LeveledLogger
}

func newTrackList(log logging.LeveledLogger) *trackList {
	return &trackList{
		tracks: make(map[string]ITrack),
		log:    log,
	}
}

func (t *trackList) Add(track ITrack) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := track.ID()
	if _, ok := t.tracks[id]; ok {
		t.log.Warnf("tracklist: track  %s already added", id)
		return ErrTrackExists
	}

	t.tracks[id] = track

	return nil
}

func (t *trackList) Get(ID string) (ITrack, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if track, ok := t.tracks[ID]; ok {
		return track, nil
	}

	return nil, ErrTrackIsNotExists
}

//nolint:copylocks // This is a read only operation
func (t *trackList) remove(ids []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, id := range ids {
		delete(t.tracks, id)
	}

}

func (t *trackList) Reset() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	t.tracks = make(map[string]ITrack)
}

func (t *trackList) GetTracks() []ITrack {
	t.mu.RLock()
	defer t.mu.RUnlock()

	tracks := make([]ITrack, 0)
	for _, track := range t.tracks {
		tracks = append(tracks, track)
	}

	return tracks
}

func (t *trackList) Length() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return len(t.tracks)
}

func RIDToQuality(RID string) QualityLevel {
	switch RID {
	case "high":
		return QualityHigh
	case "mid":
		return QualityMid
	default:
		return QualityLow
	}
}
