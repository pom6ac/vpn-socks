package server

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func makeFrame(streamType byte, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	return frame
}

func TestDockerLogDemux_SingleFrame(t *testing.T) {
	data := makeFrame(1, []byte("hello world"))
	src := io.NopCloser(bytes.NewReader(data))
	d := newDockerLogDemux(src)

	buf := make([]byte, 1024)
	n, err := io.ReadFull(d, buf[:11])
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Fatalf("read: %v", err)
	}
	if n != 11 {
		t.Fatalf("read %d bytes, want 11", n)
	}
	if string(buf[:11]) != "hello world" {
		t.Errorf("got %q, want %q", string(buf[:11]), "hello world")
	}

	_, err = d.Read(buf)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestDockerLogDemux_MultipleFrames(t *testing.T) {
	data := append(makeFrame(1, []byte("first\n")), makeFrame(2, []byte("second\n"))...)
	src := io.NopCloser(bytes.NewReader(data))
	d := newDockerLogDemux(src)

	var out bytes.Buffer
	buf := make([]byte, 1024)
	for {
		n, err := d.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	want := "first\nsecond\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}

func TestDockerLogDemux_EmptyStream(t *testing.T) {
	src := io.NopCloser(bytes.NewReader(nil))
	d := newDockerLogDemux(src)

	buf := make([]byte, 1024)
	_, err := d.Read(buf)
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestDockerLogDemux_ZeroPayload(t *testing.T) {
	data := makeFrame(1, nil)
	src := io.NopCloser(bytes.NewReader(data))
	d := newDockerLogDemux(src)

	buf := make([]byte, 1024)
	_, err := d.Read(buf)
	if err != io.EOF {
		t.Errorf("expected EOF for zero-length payload, got %v", err)
	}
}

func TestDockerLogDemux_SmallReadBuffer(t *testing.T) {
	payload := []byte("this is a longer payload split across reads")
	data := makeFrame(1, payload)
	src := io.NopCloser(bytes.NewReader(data))
	d := newDockerLogDemux(src)

	var out bytes.Buffer
	buf := make([]byte, 5)
	for {
		n, err := d.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			t.Fatalf("read: %v", err)
		}
		if n == 0 && err != nil {
			break
		}
	}

	if out.String() != string(payload) {
		t.Errorf("got %q, want %q", out.String(), string(payload))
	}
}

func TestDockerLogDemux_Close(t *testing.T) {
	src := &closeCounter{Reader: bytes.NewReader(nil)}
	d := newDockerLogDemux(src)

	if err := d.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if !src.closed {
		t.Error("underlying source not closed")
	}
}

type closeCounter struct {
	*bytes.Reader
	closed bool
}

func (c *closeCounter) Close() error {
	c.closed = true
	return nil
}
