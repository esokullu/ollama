// Package browser connects the app to a Chromium instance running with
// --remote-debugging-port, and to the WebBrain extension through the
// webbrain-mcp stdio server.
package browser

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsGUID is the fixed handshake salt from RFC 6455 section 1.3. It is checked
// against the RFC's own test vector in the tests: a typo here is invisible in
// any test where both halves share the constant, and only shows up as a
// rejected handshake against a real server.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxFrameSize caps a single inbound frame. Screencast frames arrive as
// base64 JPEG inside a JSON message, so the ceiling has to clear a full-page
// capture at high DPI with room to spare; anything past this is a protocol
// desync rather than a real payload.
const maxFrameSize = 64 << 20

// wsConn is a minimal RFC 6455 client. It speaks only what CDP needs: text
// messages, client-side masking, and the control frames a server may send
// unprompted. No extensions, no compression, no fragmentation on write.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex // serializes writes; control frames may interleave with data
	rmu sync.Mutex

	closeOnce sync.Once
}

// wsDial performs the opening handshake against a ws:// URL. CDP endpoints are
// always plaintext on loopback, so wss:// is deliberately unsupported.
func wsDial(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse websocket url: %w", err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("unsupported websocket scheme %q, want ws", u.Scheme)
	}

	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(host, "80")
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}

	// Handshake deadlines come from the caller's context; once the connection
	// is live it is long-running, so the deadline is cleared afterwards.
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, fmt.Errorf("generate websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"

	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	br := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read handshake response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		// Chrome answers 403 here when the request carries an Origin it was not
		// launched to allow. We never send Origin, so this usually means a
		// proxy rewrote the request.
		return nil, fmt.Errorf("websocket upgrade rejected: %s", resp.Status)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		conn.Close()
		return nil, errors.New("websocket upgrade missing Upgrade: websocket")
	}

	sum := sha1.Sum([]byte(key + wsGUID))
	if want := base64.StdEncoding.EncodeToString(sum[:]); resp.Header.Get("Sec-WebSocket-Accept") != want {
		conn.Close()
		return nil, errors.New("websocket accept key mismatch")
	}

	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

// WriteText sends a single unfragmented text message.
func (c *wsConn) WriteText(b []byte) error {
	return c.writeFrame(opText, b)
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	// A 4-byte mask key is required on every client frame (RFC 6455 5.3).
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("generate mask: %w", err)
	}

	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode) // FIN set, single frame

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 0x80|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	header = append(header, mask[:]...)

	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	if _, err := c.conn.Write(header); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	if _, err := c.conn.Write(masked); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	return nil
}

// ReadMessage returns the next complete data message, reassembling
// continuation frames and answering control frames inline. Control frames are
// never returned to the caller.
func (c *wsConn) ReadMessage() (byte, []byte, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()

	var (
		msg      []byte
		dataCode byte
		started  bool
	)

	for {
		fin, opcode, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch opcode {
		case opPing:
			// Pong must echo the ping payload.
			if err := c.writeFrame(opPong, payload); err != nil {
				return 0, nil, fmt.Errorf("reply to ping: %w", err)
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, nil)
			return 0, nil, io.EOF
		case opText, opBinary:
			if started {
				return 0, nil, errors.New("websocket: new data frame before previous message finished")
			}
			started = true
			dataCode = opcode
			msg = payload
		case opContinuation:
			if !started {
				return 0, nil, errors.New("websocket: continuation frame without initial frame")
			}
			msg = append(msg, payload...)
		default:
			return 0, nil, fmt.Errorf("websocket: unknown opcode %#x", opcode)
		}

		if fin {
			return dataCode, msg, nil
		}
	}
}

func (c *wsConn) readFrame() (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}

	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0

	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	if length > maxFrameSize {
		return false, 0, nil, fmt.Errorf("websocket: frame of %d bytes exceeds limit", length)
	}

	// Servers must not mask, but unmasking costs nothing and a mismatch here
	// would otherwise surface as unreadable JSON far from the cause.
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
			return false, 0, nil, err
		}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}

	return fin, opcode, payload, nil
}

// Close sends a close frame and tears down the connection. It is safe to call
// concurrently with a blocked ReadMessage: closing the socket unblocks it.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		_ = c.writeFrame(opClose, nil)
		err = c.conn.Close()
	})
	return err
}
