package server

import (
	"encoding/binary"
	"errors"
	"io"
)

// dockerLogDemux reads the Docker multiplexed log stream format and yields
// the raw payload bytes. Each frame has an 8-byte header:
//
//	[1 byte stream type] [3 bytes padding] [4 bytes big-endian payload length]
//
// followed by the payload of that length.
type dockerLogDemux struct {
	src    io.ReadCloser
	remain int
	header [8]byte
}

// newDockerLogDemux wraps an io.ReadCloser from Docker ContainerLogs.
func newDockerLogDemux(src io.ReadCloser) *dockerLogDemux {
	return &dockerLogDemux{src: src}
}

// Read implements io.Reader, concatenating payload data from all frames.
func (d *dockerLogDemux) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Zero-length frames are legal and carry nothing: skip to the next one.
	for d.remain == 0 {
		if _, err := io.ReadFull(d.src, d.header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, io.EOF
			}
			return 0, err
		}
		d.remain = int(binary.BigEndian.Uint32(d.header[4:8]))
	}

	n := len(p)
	if n > d.remain {
		n = d.remain
	}
	read, err := io.ReadFull(d.src, p[:n])
	d.remain -= read
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return read, err
}

// Close closes the underlying stream.
func (d *dockerLogDemux) Close() error {
	return d.src.Close()
}
