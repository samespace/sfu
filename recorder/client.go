package recorder

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"time"

	"github.com/quic-go/quic-go"
)

type RecorderConfig struct {
	Host     string
	Port     uint16
	CertFile string
	KeyFile  string
}

type ClientConfig struct {
	ClientId   string
	BucketName string
	FileName   string
	Channel    int // 1 to left channel, 2 to right channel
}

type SplitConfig struct {
	Start    time.Duration
	End      time.Duration
	FileName string
}

type Channel int

const (
	LeftChannel  Channel = 1
	RightChannel Channel = 2
)

type ChannelConfig map[string]int
type StopConfig struct {
	Splits        []SplitConfig
	ChannelConfig ChannelConfig
}

func readCertAndKey(config *RecorderConfig) *tls.Config {
	cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{"samespace-recorder"},
		InsecureSkipVerify: true,
	}
}

func NewQuicClient(ctx context.Context, clientConfig ClientConfig, config *RecorderConfig) (quic.Connection, error) {
	var conn quic.Connection
	var err error

	tlsConfig := readCertAndKey(config)
	address := fmt.Sprintf("%s:%d", config.Host, config.Port)

	for retries := 0; retries < 5; retries++ {
		fmt.Printf("Attempting connection (attempt %d)...\n", retries+1)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		conn, err = quic.DialAddr(ctx, address, tlsConfig, &quic.Config{
			EnableDatagrams: true,
		})
		if err == nil {
			j, _ := json.Marshal(clientConfig)
			err := conn.SendDatagram(j)
			if err != nil {
				return nil, err
			}
			return conn, nil
		}

		fmt.Printf("Connection failed (attempt %d): %v\n", retries+1, err)
		time.Sleep(time.Duration(2<<retries) * time.Second) // backoff exponentially
	}

	return nil, fmt.Errorf("failed to establish QUIC connection after retries: %w", err)
}
