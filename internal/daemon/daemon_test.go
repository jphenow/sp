package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jphenow/sp/internal/store"
)

// testDaemon creates a daemon with a temp database and socket for testing.
func testDaemon(t *testing.T) (*Daemon, Config) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := store.OpenPath(dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	config := Config{
		SocketPath:  filepath.Join(dir, "test.sock"),
		PIDPath:     filepath.Join(dir, "test.pid"),
		IdleTimeout: 0, // no auto-stop in tests
	}

	d := New(config, db)
	return d, config
}

func TestDaemonStartStop(t *testing.T) {
	d, config := testDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Start(ctx)
	}()

	// Wait for socket to appear
	for i := 0; i < 50; i++ {
		if _, err := net.Dial("unix", config.SocketPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify daemon is running
	if !IsRunning(config) {
		t.Fatal("daemon should be running")
	}

	// Stop daemon
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("daemon error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop within 5 seconds")
	}
}

func TestDaemonPingPong(t *testing.T) {
	d, config := testDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Start(ctx)

	// Wait for socket
	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		conn, err = net.Dial("unix", config.SocketPath)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("connecting to daemon: %v", err)
	}
	defer conn.Close()

	// Send ping
	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(Request{Method: "ping"}); err != nil {
		t.Fatalf("sending ping: %v", err)
	}

	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		t.Fatalf("reading response: %v", err)
	}

	if resp.Error != "" {
		t.Fatalf("ping error: %s", resp.Error)
	}

	var result string
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if result != "pong" {
		t.Errorf("expected 'pong', got %q", result)
	}
}

func TestDaemonCRUD(t *testing.T) {
	d, config := testDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Start(ctx)

	// Wait for socket and connect
	var client *Client
	var err error
	for i := 0; i < 50; i++ {
		client, err = ConnectTo(config.SocketPath)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer client.Close()

	// Upsert a sprite
	err = client.UpsertSprite(&store.Sprite{
		Name:      "test-sprite",
		LocalPath: "/home/user/test",
		Status:    "running",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Get the sprite
	s, err := client.GetSprite("test-sprite")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.Name != "test-sprite" {
		t.Errorf("name = %q, want %q", s.Name, "test-sprite")
	}
	if s.Status != "running" {
		t.Errorf("status = %q, want %q", s.Status, "running")
	}

	// List sprites
	sprites, err := client.ListSprites(store.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sprites) != 1 {
		t.Fatalf("expected 1 sprite, got %d", len(sprites))
	}

	// Tag
	err = client.TagSprite("test-sprite", "work")
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	tags, err := client.GetTags("test-sprite")
	if err != nil {
		t.Fatalf("get tags: %v", err)
	}
	if len(tags) != 1 || tags[0] != "work" {
		t.Errorf("tags = %v, want [work]", tags)
	}

	// Filter by tag
	filtered, err := client.ListSprites(store.ListOptions{Tags: []string{"work"}})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(filtered) != 1 {
		t.Errorf("expected 1 filtered sprite, got %d", len(filtered))
	}

	// Update status
	err = client.UpdateSpriteStatus("test-sprite", "warm")
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	s, err = client.GetSprite("test-sprite")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if s.Status != "warm" {
		t.Errorf("status = %q, want %q", s.Status, "warm")
	}

	// Delete
	err = client.DeleteSprite("test-sprite")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	sprites, err = client.ListSprites(store.ListOptions{})
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(sprites) != 0 {
		t.Errorf("expected 0 sprites after delete, got %d", len(sprites))
	}
}
