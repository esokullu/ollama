package browser

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// wsTestServer is a hand-rolled server half, only as capable as the tests
// need: it completes the handshake, then reads client frames and writes
// unmasked server frames.
type wsTestServer struct {
	conn net.Conn
	br   *bufio.Reader

	// ready closes once conn and br are populated. Callers must go through
	// waitReady before touching either, so the handshake goroutine's writes
	// happen-before any read of them.
	ready chan struct{}
}

func newWSTestServer(t *testing.T) (*wsTestServer, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	srv := &wsTestServer{ready: make(chan struct{})}
	accepted := make(chan struct{})

	go func() {
		defer close(accepted)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			conn.Close()
			return
		}

		key := req.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])

		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")

		srv.conn = conn
		srv.br = br
		close(srv.ready)
	}()

	url := "ws://" + ln.Addr().String() + "/devtools/page/TEST"
	t.Cleanup(func() {
		<-accepted
		select {
		case <-srv.ready:
			srv.conn.Close()
		default:
		}
	})
	return srv, url
}

// waitReady blocks until the accept goroutine has finished the handshake.
func (s *wsTestServer) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-s.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server never completed handshake")
	}
}

// writeFrame writes an unmasked server frame.
func (s *wsTestServer) writeFrame(fin bool, opcode byte, payload []byte) error {
	var first byte = opcode
	if fin {
		first |= 0x80
	}
	header := []byte{first}

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}

	if _, err := s.conn.Write(header); err != nil {
		return err
	}
	_, err := s.conn.Write(payload)
	return err
}

// readFrame reads one masked client frame and unmasks it.
func (s *wsTestServer) readFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(s.br, head[:]); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0

	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(s.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(s.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(s.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(s.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

func dialTest(t *testing.T, url string) *wsConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := wsDial(ctx, url)
	if err != nil {
		t.Fatalf("wsDial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Pinned to the worked example in RFC 6455 section 1.3. Every other test here
// shares wsGUID between both halves of the connection, so a typo in it stays
// invisible to them while breaking against every real server.
func TestWSAcceptKeyMatchesRFCVector(t *testing.T) {
	const (
		key  = "dGhlIHNhbXBsZSBub25jZQ=="
		want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	)

	sum := sha1.Sum([]byte(key + wsGUID))
	if got := base64.StdEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("accept key for the RFC vector = %q, want %q (wsGUID is wrong)", got, want)
	}
}

func TestWSClientMasksAndFramesWrites(t *testing.T) {
	srv, url := newWSTestServer(t)
	c := dialTest(t, url)
	srv.waitReady(t)

	// Three sizes to cover all three length encodings.
	for _, payload := range []string{
		`{"id":1,"method":"Page.enable"}`,
		strings.Repeat("a", 200),
		strings.Repeat("b", 70000),
	} {
		if err := c.WriteText([]byte(payload)); err != nil {
			t.Fatalf("WriteText(%d bytes): %v", len(payload), err)
		}

		opcode, got, err := srv.readFrame()
		if err != nil {
			t.Fatalf("server readFrame: %v", err)
		}
		if opcode != opText {
			t.Errorf("opcode = %#x, want text", opcode)
		}
		if string(got) != payload {
			t.Errorf("payload of %d bytes did not round-trip", len(payload))
		}
	}
}

func TestWSClientRejectsBadAcceptKey(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		http.ReadRequest(bufio.NewReader(conn))
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: wrongkey\r\n\r\n")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := wsDial(ctx, "ws://"+ln.Addr().String()+"/"); err == nil {
		t.Fatal("wsDial accepted a bad Sec-WebSocket-Accept")
	}
}

func TestWSClientReassemblesFragments(t *testing.T) {
	srv, url := newWSTestServer(t)
	c := dialTest(t, url)
	srv.waitReady(t)

	// A CDP screencast frame arrives split across continuation frames when it
	// is large, which is the common case rather than the exotic one.
	srv.writeFrame(false, opText, []byte(`{"method":"Page.screen`))
	srv.writeFrame(false, opContinuation, []byte(`castFrame","params":{`))
	srv.writeFrame(true, opContinuation, []byte(`"sessionId":7}}`))

	opcode, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if opcode != opText {
		t.Errorf("opcode = %#x, want text", opcode)
	}
	want := `{"method":"Page.screencastFrame","params":{"sessionId":7}}`
	if string(msg) != want {
		t.Errorf("got %q, want %q", msg, want)
	}
}

func TestWSClientAnswersPingWithoutSurfacingIt(t *testing.T) {
	srv, url := newWSTestServer(t)
	c := dialTest(t, url)
	srv.waitReady(t)

	srv.writeFrame(true, opPing, []byte("keepalive"))
	srv.writeFrame(true, opText, []byte(`{"id":1}`))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage: %v", err)
			return
		}
		// The ping must not be returned as a message.
		if string(msg) != `{"id":1}` {
			t.Errorf("got %q, want the text frame", msg)
		}
	}()

	opcode, payload, err := srv.readFrame()
	if err != nil {
		t.Fatalf("server readFrame: %v", err)
	}
	if opcode != opPong {
		t.Errorf("opcode = %#x, want pong", opcode)
	}
	if string(payload) != "keepalive" {
		t.Errorf("pong payload = %q, want the ping payload echoed", payload)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ReadMessage never returned the text frame")
	}
}

func TestWSClientReturnsEOFOnClose(t *testing.T) {
	srv, url := newWSTestServer(t)
	c := dialTest(t, url)
	srv.waitReady(t)

	srv.writeFrame(true, opClose, nil)

	if _, _, err := c.ReadMessage(); err != io.EOF {
		t.Fatalf("ReadMessage after close = %v, want io.EOF", err)
	}
}
