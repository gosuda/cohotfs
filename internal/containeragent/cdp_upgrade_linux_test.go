//go:build linux

package containeragent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCDPWebSocketForwardsBufferedClientBytes(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	payload := []byte{0x81, 0x02, 'o', 'k'}
	received := make(chan []byte, 1)
	go func() {
		connection, err := upstream.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_ = request.Body.Close()
		body := make([]byte, len(payload))
		_, err = io.ReadFull(reader, body)
		if err != nil {
			return
		}
		received <- body
		_, _ = fmt.Fprint(connection, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	}()
	proxy := cdpProxy{dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Addr().String())
	}}
	server := httptest.NewServer(proxy)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	request := "GET /devtools/page/1 HTTP/1.1\r\nHost: " + address + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: fixture\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := connection.Write(append([]byte(request), payload...)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if string(got) != string(payload) {
			t.Fatalf("buffered payload = %x, want %x", got, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("buffered WebSocket bytes were not forwarded")
	}
}
