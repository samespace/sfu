package sfu

import (
	"fmt"

	"github.com/samespace/sfu/processing"
)

type RecorderExtention struct{}

func NewRecorderExtension() IManagerExtension {
	return &RecorderExtention{}
}

func (r *RecorderExtention) OnGetRoom(manager *Manager, roomID string) (*Room, error) {
	fmt.Println("OnGetRoom")
	return nil, nil
}
func (r *RecorderExtention) OnBeforeNewRoom(id, name, roomType string) error {
	fmt.Println("OnBeforeNewRoom")

	return nil
}
func (r *RecorderExtention) OnNewRoom(manager *Manager, room *Room) {
	fmt.Println("OnNewRoom")
}

func (r *RecorderExtention) OnRoomClosed(manager *Manager, room *Room) {
	fmt.Println("OnRoomClosed")
	go processing.ProcessRoom(room.id)
}
