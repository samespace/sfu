module github.com/samespace/sfu

go 1.22.0

toolchain go1.22.5

// replace github.com/pion/transport => ../../pion/pion-transport
// replace github.com/pion/interceptor => ../../pion/pion-interceptor

require (
	github.com/pion/interceptor v0.1.30
	github.com/pion/rtcp v1.2.14
	github.com/stretchr/testify v1.9.0
)

require (
	github.com/jaevor/go-nanoid v1.4.0
	github.com/pion/sdp/v4 v4.0.0-20240223200530-fb77fb3c6578
	github.com/pion/turn/v4 v4.0.0
	github.com/pion/webrtc/v4 v4.0.0-beta.29
	github.com/quic-go/quic-go v0.47.0
	golang.org/x/exp v0.0.0-20240909161429-701f63a606c0
	golang.org/x/text v0.18.0
)

require (
	github.com/go-task/slim-sprig/v3 v3.0.0 // indirect
	github.com/google/pprof v0.0.0-20240910150728-a0b0bb1d4134 // indirect
	github.com/onsi/ginkgo/v2 v2.20.2 // indirect
	github.com/pion/dtls/v3 v3.0.2 // indirect
	github.com/pion/mdns/v2 v2.0.7 // indirect
	github.com/pion/srtp/v3 v3.0.3 // indirect
	github.com/pion/stun/v3 v3.0.0 // indirect
	github.com/pion/transport/v3 v3.0.7 // indirect
	github.com/wlynxg/anet v0.0.4 // indirect
	go.uber.org/mock v0.4.0 // indirect
	golang.org/x/mod v0.21.0 // indirect
	golang.org/x/sync v0.8.0 // indirect
	golang.org/x/tools v0.25.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/uuid v1.6.0
	github.com/pion/datachannel v1.5.9 // indirect
	github.com/pion/ice/v4 v4.0.1
	github.com/pion/logging v0.2.2
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtp v1.8.9
	github.com/pion/sctp v1.8.33 // indirect
	github.com/pion/sdp/v3 v3.0.9 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.27.0 // indirect
	golang.org/x/net v0.29.0 // direct
	golang.org/x/sys v0.25.0
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
