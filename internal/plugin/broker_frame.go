package plugin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	BrokerFDEnvironment        = "BLAZN_PLUGIN_BROKER_FD"
	brokerProtocolVersion      = 1
	brokerHeaderSize           = 16
	brokerMaxControlBytes      = 1024 * 1024
	brokerMaxDataBytes         = 1024 * 1024
	brokerMaxStreams           = 4096
	brokerFrameRequest    byte = 1
	brokerFrameResponse   byte = 2
	brokerFrameData       byte = 3
	brokerFrameEnd        byte = 4
	brokerFrameCancel     byte = 5
	brokerControlStreamID      = ^uint32(0)
)

var brokerMagic = [4]byte{'B', 'Z', 'P', 'B'}

type brokerFrame struct {
	Type     byte
	StreamID uint32
	Payload  []byte
}

func readBrokerFrame(reader io.Reader) (brokerFrame, error) {
	header := make([]byte, brokerHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return brokerFrame{}, err
	}
	if string(header[:4]) != string(brokerMagic[:]) || header[4] != brokerProtocolVersion || header[6] != 0 || header[7] != 0 {
		return brokerFrame{}, errors.New("broker frame header is invalid")
	}
	typeValue := header[5]
	if typeValue < brokerFrameRequest || typeValue > brokerFrameCancel {
		return brokerFrame{}, errors.New("broker frame type is invalid")
	}
	streamID := binary.BigEndian.Uint32(header[8:12])
	if streamID == 0 {
		return brokerFrame{}, errors.New("broker frame stream ID is invalid")
	}
	length := binary.BigEndian.Uint32(header[12:16])
	limit := uint32(brokerMaxControlBytes)
	if typeValue == brokerFrameData {
		limit = brokerMaxDataBytes
	}
	if length > limit {
		return brokerFrame{}, errors.New("broker frame payload exceeds its limit")
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return brokerFrame{}, fmt.Errorf("read broker payload: %w", err)
	}
	return brokerFrame{Type: typeValue, StreamID: streamID, Payload: payload}, nil
}

func writeBrokerFrame(writer io.Writer, frame brokerFrame) error {
	if frame.StreamID == 0 || frame.Type < brokerFrameRequest || frame.Type > brokerFrameCancel {
		return errors.New("broker frame is invalid")
	}
	limit := brokerMaxControlBytes
	if frame.Type == brokerFrameData {
		limit = brokerMaxDataBytes
	}
	if len(frame.Payload) > limit {
		return errors.New("broker frame payload exceeds its limit")
	}
	header := make([]byte, brokerHeaderSize)
	copy(header[:4], brokerMagic[:])
	header[4] = brokerProtocolVersion
	header[5] = frame.Type
	binary.BigEndian.PutUint32(header[8:12], frame.StreamID)
	binary.BigEndian.PutUint32(header[12:16], uint32(len(frame.Payload)))
	if err := writeAll(writer, header); err != nil {
		return err
	}
	return writeAll(writer, frame.Payload)
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
