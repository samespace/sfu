package sfu

import (
	"fmt"
	"os"
	"sync"
	"time"
)

type postProcessor struct {
	processingQueue chan string
	processingLock  sync.Mutex
}

func newPostProcessor() postProcessor {
	return postProcessor{
		processingQueue: make(chan string),
		processingLock:  sync.Mutex{},
	}
}

func (pp *postProcessor) startProcessingRoom(roomId string) {
	pp.processingQueue <- roomId
}

func (pp *postProcessor) startProcessingQueue() {
	for roomID := range pp.processingQueue {
		pp.processRoom(roomID)
	}
}

func (pp *postProcessor) processRoom(roomID string) {
	pp.processingLock.Lock()
	defer pp.processingLock.Unlock()

	dir := fmt.Sprintf("recordings/%s", roomID)
	err := pp.processRecordings(dir)
	if err != nil {
		fmt.Printf("Error processing room %s: %v\n", roomID, err)
	} else {
		fmt.Printf("Successfully processed room %s\n", roomID)
	}
}

func (pp *postProcessor) processRecordings(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	for _, file := range files {
		fmt.Printf("Processing file: %s\n", file.Name())
		time.Sleep(1 * time.Second)
	}

	return nil
}
