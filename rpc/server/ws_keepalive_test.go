package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Reiers/lantern/api"
	"github.com/Reiers/lantern/chain/trustedroot"
	"github.com/Reiers/lantern/rpc/handlers"
)

// TestWebsocketPingGetsPong guards the keepalive contract go-ethereum's
// websocket client relies on (Curio's EthClient): when the client has been
// idle for 30s it sends a ping and arms a 30s read deadline that only a
// pong clears. go-jsonrpc v0.10.1 swallowed pings without replying, so
// every idle Curio eth connection died 30s later with
// "read tcp ...:1234: i/o timeout", failing all eth_calls in flight on it.
// go-jsonrpc v0.10.3 replies with a protocol-level pong.
func TestWebsocketPingGetsPong(t *testing.T) {
	dir := t.TempDir()
	tr := &trustedroot.TrustedRoot{Epoch: 1}
	chainAPI := handlers.New(tr, nil, nil, nil, "mainnet")
	srv, err := New(Config{ListenAddress: "127.0.0.1:0", DataDir: dir}, chainAPI)
	if err != nil {
		t.Fatal(err)
	}
	chainAPI.AuthIssuer = srv.Auth()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop(context.Background())

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+srv.Auth().Token(api.PermRead))
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+srv.Addr().String()+"/rpc/v1", hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	pong := make(chan string, 1)
	conn.SetPongHandler(func(data string) error {
		select {
		case pong <- data:
		default:
		}
		return nil
	})
	// Server pings (go-jsonrpc keepalive) are answered by the default
	// handler; a reader goroutine is needed for control frames to be
	// processed at all.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	if err := conn.WriteControl(websocket.PingMessage, []byte("lantern"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-pong:
		if data != "lantern" {
			t.Fatalf("pong payload %q, want the ping payload echoed", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pong for a client ping: idle go-ethereum websocket clients will hit a read i/o timeout")
	}
}
