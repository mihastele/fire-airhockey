package main

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
)

// pipeConns returns a WSConn whose wire bytes can be observed.
func pipeConns(t *testing.T) (*WSConn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	return &WSConn{conn: a, br: bufio.NewReader(a)}, b
}

func TestWriteShortFrameBytes(t *testing.T) {
	ws, peer := pipeConns(t)
	defer ws.Close()
	defer peer.Close()
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 64)
		total := 0
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		for total < 4 {
			n, err := peer.Read(buf[total:])
			if err != nil {
				break
			}
			total += n
		}
		got <- buf[:total]
	}()
	if err := ws.WriteMessage([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	raw := <-got
	want := []byte{0x81, 0x02, 'h', 'i'}
	if !bytes.Equal(raw, want) {
		t.Fatalf("short frame = %x, want %x", raw, want)
	}
}

func TestWriteExtendedFrameBytes(t *testing.T) {
	ws, peer := pipeConns(t)
	defer ws.Close()
	defer peer.Close()
	payload := bytes.Repeat([]byte("a"), 200)
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 512)
		total := 0
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		for total < 204 {
			n, err := peer.Read(buf[total:])
			if err != nil {
				break
			}
			total += n
		}
		got <- buf[:total]
	}()
	if err := ws.WriteMessage(payload); err != nil {
		t.Fatal(err)
	}
	raw := <-got
	if len(raw) != 204 || raw[0] != 0x81 || raw[1] != 126 || raw[2] != 0 || raw[3] != 200 {
		t.Fatalf("extended frame header = %x (len %d)", raw[:4], len(raw))
	}
	if !bytes.Equal(raw[4:], payload) {
		t.Fatalf("extended frame payload corrupted")
	}
}

func TestReadMaskedClientFrame(t *testing.T) {
	ws, peer := pipeConns(t)
	defer ws.Close()
	defer peer.Close()
	payload := []byte(`{"t":"hello","name":"Ada"}`)
	key := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, 0x80 | byte(len(payload))}
	frame = append(frame, key[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ key[i%4]
	}
	frame = append(frame, masked...)
	go func() {
		_, _ = peer.Write(frame)
	}()
	_ = ws.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("read = %q, want %q", got, payload)
	}
}

func TestPingGetsPong(t *testing.T) {
	ws, peer := pipeConns(t)
	defer ws.Close()
	defer peer.Close()
	// One writer: ping first, then the follow-up message (unmasked,
	// test-only), so consumption order is deterministic. A second
	// goroutine drains the pong reply concurrently: without a pending
	// reader the pong write would block on the synchronous pipe.
	go func() {
		_, _ = peer.Write([]byte{0x89, 0x03, 'a', 'b', 'c'})
		_, _ = peer.Write([]byte{0x81, 0x03, 'x', 'y', 'z'})
	}()
	go func() {
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = peer.Read(make([]byte, 8))
	}()
	_ = ws.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "xyz" {
		t.Fatalf("after ping, read = %q", got)
	}
}
