//go:build linux

package hostservice

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosuda/cohotfs/internal/hostroot"
)

func TestTCPLeaseBridgeClosesLiveConnections(t *testing.T) {
	root, err := hostroot.OpenForTest(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := upstream.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	server := &Server{root: root}
	bridge, err := server.newLeaseTCPBridge(filepath.Join("run", "cdp.sock"), upstream.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("unix", bridge.identity.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var target net.Conn
	select {
	case target = <-accepted:
		defer target.Close()
	case <-time.After(time.Second):
		t.Fatal("bridge did not connect upstream")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bridge.close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(bridge.identity.Path); !os.IsNotExist(err) {
		t.Fatalf("bridge socket remains: %v", err)
	}
}
