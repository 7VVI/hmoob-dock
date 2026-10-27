package dockerops

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"

	"hmoob-dock/internal/protocol"
)

// The layout of the frames of the Docker log multiplex.
//
// A stream that does not belong to a terminal carries an eight byte header
// before every piece of the output: the stream (stdout or stderr), three
// bytes of padding and the length of the piece in big endian order. A
// terminal stream carries the raw bytes and no header.
const (
	// logHeaderSize is the size of the header of a multiplexed frame.
	logHeaderSize = 8
	// logReadBufferSize is the largest piece one read of the stream
	// produces. It is the limit of the text of one log frame.
	logReadBufferSize = protocol.MaxLogFrameTextSize
)

// The errors of the logs of a container. The agent tells them apart, so the
// page shows the reason of a missing stream instead of an empty log.
var (
	// ErrContainerNotFound reports a container that does not exist.
	ErrContainerNotFound = errors.New("dockerops: the container does not exist")
	// ErrLogsUnavailable reports logs that the engine cannot hand out, for
	// example because the logging driver does not support reading.
	ErrLogsUnavailable = errors.New("dockerops: the logs of the container are not available")
)

// Logs streams the logs of a container.
//
// The text is handed to emit in the order of the stream. A stream of a
// terminal is read as it is; the stream of a container without a terminal
// is demultiplexed, so the headers of stdout and stderr never reach the
// page. The offsets of the frames grow with the text, so a reader keeps the
// order.
//
// The read ends when the container stops, when the context ends or when the
// stream fails. The body of the stream of the engine is closed in every
// case.
func (l *Local) Logs(ctx context.Context, req protocol.LogRequest, emit func(protocol.LogFrame) error) error {
	id := strings.TrimSpace(req.ContainerID)
	if id == "" {
		return fmt.Errorf("dockerops: read the logs of a container without an ID")
	}
	if emit == nil {
		return fmt.Errorf("dockerops: read the logs of the container %s without a reader", id)
	}

	// The terminal mode belongs to the container and not to the request:
	// only the engine knows whether the stream carries frame headers.
	details, err := l.client.ContainerInspect(ctx, id)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The subscription ended while the container was inspected.
			return ctxErr
		}
		return wrapLogError("inspect the container "+id, err)
	}
	terminal := details.Config != nil && details.Config.Tty

	reader, err := l.client.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     req.Follow,
		Tail:       strconv.Itoa(protocol.NormalizeLogTail(req.Tail)),
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The subscription ended while the stream was opened.
			return ctxErr
		}
		return wrapLogError("read the logs of the container "+id, err)
	}

	var offset uint64
	emitText := func(p []byte) error {
		frame := protocol.LogFrame{
			SubscriptionID: req.SubscriptionID,
			Offset:         offset,
			Text:           string(p),
		}
		offset += uint64(len(p))
		return emit(frame)
	}

	var readErr error
	if terminal {
		readErr = readRawLogStream(reader, emitText)
	} else {
		readErr = readLogStream(reader, emitText)
	}
	closeErr := reader.Close()
	if readErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The subscription ended; the stream of the engine was closed
			// by the cancellation and not by a failure of the logs.
			return ctxErr
		}
		return fmt.Errorf("dockerops: stream the logs of the container %s: %w", id, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("dockerops: close the logs of the container %s: %w", id, closeErr)
	}
	return nil
}

// readLogStream reads the multiplexed stream of a container without a
// terminal. The stdout and the stderr pieces are handed over in the order
// the engine wrote them.
func readLogStream(reader io.Reader, emit func([]byte) error) error {
	header := make([]byte, logHeaderSize)
	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		size := int64(binary.BigEndian.Uint32(header[4:logHeaderSize]))
		if err := readPayload(reader, size, emit); err != nil {
			return err
		}
	}
}

// readRawLogStream reads the raw stream of a terminal container. The bytes
// are not interpreted: a byte that looks like a header stays part of the
// text.
func readRawLogStream(reader io.Reader, emit func([]byte) error) error {
	buffer := make([]byte, logReadBufferSize)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if emitErr := emit(buffer[:n]); emitErr != nil {
				return emitErr
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

// readPayload hands a piece of the multiplexed stream over in chunks of at
// most MaxLogFrameTextSize bytes. The length comes from the stream, so the
// payload is read in chunks instead of being allocated at once.
func readPayload(reader io.Reader, size int64, emit func([]byte) error) error {
	buffer := make([]byte, logReadBufferSize)
	for size > 0 {
		length := int64(len(buffer))
		if size < length {
			length = size
		}
		n, err := io.ReadFull(reader, buffer[:length])
		if n > 0 {
			if emitErr := emit(buffer[:n]); emitErr != nil {
				return emitErr
			}
			size -= int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

// wrapLogError keeps the reason of a failing log stream: a container that
// disappeared is told apart from a logging driver that cannot be read.
func wrapLogError(action string, err error) error {
	reason := ErrLogsUnavailable
	if errdefs.IsNotFound(err) {
		reason = ErrContainerNotFound
	}
	return fmt.Errorf("dockerops: %s: %w: %v", action, reason, err)
}
