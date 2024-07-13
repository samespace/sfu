module github.com/inlivedev/sfu

go 1.21

// replace github.com/pion/transport => ../../pion/pion-transport
// replace github.com/pion/interceptor => ../../pion/pion-interceptor

require (
	github.com/pion/ice/v2 v2.3.13
	github.com/pion/interceptor v0.1.29
	github.com/pion/rtcp v1.2.14
	github.com/stretchr/testify v1.9.0
)

require (
	github.com/jaevor/go-nanoid v1.3.0
	github.com/pion/webrtc/v4 v4.0.0-beta.21
	golang.org/x/exp v0.0.0-20230905200255-921286631fa9
	golang.org/x/text v0.15.0
)

require (
	github.com/pion/ice/v3 v3.0.7 // indirect
	github.com/pion/mdns/v2 v2.0.7 // indirect
	github.com/pion/srtp/v3 v3.0.1 // indirect
	github.com/pion/stun/v2 v2.0.0 // indirect
	github.com/pion/transport/v3 v3.0.2 // indirect
	github.com/pion/turn/v3 v3.0.3 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/google/uuid v1.6.0
	github.com/pion/datachannel v1.5.6 // indirect
	github.com/pion/dtls/v2 v2.2.11 // indirect
	github.com/pion/logging v0.2.2
	github.com/pion/mdns v0.0.12 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtp v1.8.6
	github.com/pion/sctp v1.8.16 // indirect
	github.com/pion/sdp/v3 v3.0.9
	github.com/pion/stun v0.6.1 // indirect
	github.com/pion/transport/v2 v2.2.4 // indirect
	github.com/pion/turn/v2 v2.1.3
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/crypto v0.23.0 // indirect
	golang.org/x/net v0.25.0 // direct
	golang.org/x/sys v0.20.0
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
