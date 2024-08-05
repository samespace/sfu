package sfu

type Recorder interface {
	Start() error
	Stop() error
	Pause() error
	Continue() error
	Close() error
}
