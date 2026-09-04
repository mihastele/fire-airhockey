// Package main — minimal RFC 6455 WebSocket server framing.
//
// The server needs real-time bidirectional play but must build with the Go
// standard library only (no external websocket dependency), so this file
// implements just enough of the protocol: opening handshake, masked text
// frames from the client (including fragmentation), ping/pong and close.
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsMaxMessage caps a single reassembled message at 1 MiB.
const wsMaxMessage = 1 << 20

var errWSClosed = errors.New("websocket closed")

// WSConn is one accepted WebSocket connection. Reads are single-threaded
// (one read pump per connection); writes are safe for concurrent use.
type WSConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// HijackWS performs the server side of the opening handshake and returns the
// raw connection wrapped for frame I/O.
func HijackWS(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	if !headerHasToken(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("missing websocket key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return nil, errors.New("hijack not supported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &WSConn{conn: conn, br: rw.Reader}, nil
}

func headerHasToken(hdr, token string) bool {
	for _, part := range strings.Split(hdr, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// ReadMessage returns the next complete text/binary message payload,
// transparently handling fragmentation and ping/pong.
func (c *WSConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		op, fin, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0x0: // continuation
			if len(msg)+len(payload) > wsMaxMessage {
				return nil, errors.New("message too large")
			}
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		case 0x1, 0x2: // text / binary (binary is accepted and treated as text)
			if len(payload) > wsMaxMessage {
				return nil, errors.New("message too large")
			}
			if fin {
				return payload, nil
			}
			msg = append(msg[:0], payload...)
		case 0x8: // close
			_ = c.writeFrame(0x8, payload)
			return nil, errWSClosed
		case 0x9: // ping
			_ = c.writeFrame(0xA, payload)
		case 0xA: // pong
		default:
			return nil, fmt.Errorf("unsupported opcode %d", op)
		}
	}
}

// readFrame reads a single frame and returns its opcode, FIN flag and payload.
func (c *WSConn) readFrame() (op byte, fin bool, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, false, nil, err
	}
	fin = hdr[0]&0x80 != 0
	op = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	n := int64(hdr[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, false, nil, err
		}
		n = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, false, nil, err
		}
		u := binary.BigEndian.Uint64(ext[:])
		if u > wsMaxMessage {
			return 0, false, nil, errors.New("frame too large")
		}
		n = int64(u)
	}
	if n > wsMaxMessage {
		return 0, false, nil, errors.New("frame too large")
	}
	// Control frames must be short and final.
	if op >= 0x8 {
		if !fin || n > 125 {
			return 0, false, nil, errors.New("bad control frame")
		}
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, key[:]); err != nil {
			return 0, false, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return op, fin, payload, nil
}

// WriteMessage sends one text frame.
func (c *WSConn) WriteMessage(p []byte) error {
	return c.writeFrame(0x1, p)
}

// Ping sends a ping control frame (clients answer automatically).
func (c *WSConn) Ping() error {
	return c.writeFrame(0x9, nil)
}

func (c *WSConn) writeFrame(op byte, p []byte) error {
	var hdr [10]byte
	hdr[0] = 0x80 | op
	n := 2 // opcode + length byte
	switch {
	case len(p) < 126:
		hdr[1] = byte(len(p))
	case len(p) < 65536:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(len(p)))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(len(p)))
		n = 10
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := writeFull(c.conn, hdr[:n]); err != nil {
		return err
	}
	if _, err := writeFull(c.conn, p); err != nil {
		return err
	}
	return nil
}

func writeFull(conn net.Conn, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := conn.Write(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Close closes the underlying connection.
func (c *WSConn) Close() error {
	return c.conn.Close()
}
