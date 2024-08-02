package sfu

type Recorder interface {
	Start() error
	Stop() error
	Pause() error
	Contiune() error
	Close() error
}
