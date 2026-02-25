package daemon

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/jphenow/sp/internal/store"
)

// Client connects to the sp daemon over Unix socket to issue requests.
type Client struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
}

// Connect establishes a connection to the daemon, starting it if needed.
func Connect() (*Client, error) {
	socketPath, err := EnsureRunning()
	if err != nil {
		return nil, fmt.Errorf("ensuring daemon is running: %w", err)
	}
	return ConnectTo(socketPath)
}

// ConnectTo connects to a daemon at the given Unix socket path.
func ConnectTo(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to daemon at %s: %w", socketPath, err)
	}
	return &Client{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		decoder: json.NewDecoder(conn),
	}, nil
}

// Close closes the connection to the daemon.
func (c *Client) Close() error {
	return c.conn.Close()
}

// call sends a request to the daemon and returns the raw response.
func (c *Client) call(method string, params interface{}) (json.RawMessage, error) {
	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshaling params: %w", err)
		}
	}

	req := Request{
		Method: method,
		Params: rawParams,
	}

	if err := c.encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	var resp Response
	if err := c.decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("daemon error: %s", resp.Error)
	}

	return resp.Result, nil
}

// Ping checks if the daemon is responsive.
func (c *Client) Ping() error {
	_, err := c.call("ping", nil)
	return err
}

// ListSprites returns all sprites matching the given filters.
func (c *Client) ListSprites(opts store.ListOptions) ([]*store.Sprite, error) {
	result, err := c.call("list", opts)
	if err != nil {
		return nil, err
	}
	var sprites []*store.Sprite
	if err := json.Unmarshal(result, &sprites); err != nil {
		return nil, fmt.Errorf("decoding sprites: %w", err)
	}
	return sprites, nil
}

// GetSprite retrieves a single sprite by name.
func (c *Client) GetSprite(name string) (*store.Sprite, error) {
	result, err := c.call("get", map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	var s store.Sprite
	if err := json.Unmarshal(result, &s); err != nil {
		return nil, fmt.Errorf("decoding sprite: %w", err)
	}
	return &s, nil
}

// UpsertSprite creates or updates a sprite in the daemon's database.
func (c *Client) UpsertSprite(s *store.Sprite) error {
	_, err := c.call("upsert", s)
	return err
}

// UpdateSpriteStatus updates the remote status of a sprite.
func (c *Client) UpdateSpriteStatus(name, status string) error {
	_, err := c.call("update_status", map[string]string{
		"name":   name,
		"status": status,
	})
	return err
}

// UpdateSyncStatus updates the sync status of a sprite.
func (c *Client) UpdateSyncStatus(name, syncStatus, syncError string) error {
	_, err := c.call("update_sync_status", map[string]string{
		"name":        name,
		"sync_status": syncStatus,
		"sync_error":  syncError,
	})
	return err
}

// DeleteSprite removes a sprite from the daemon's database.
func (c *Client) DeleteSprite(name string) error {
	_, err := c.call("delete", map[string]string{"name": name})
	return err
}

// TagSprite adds a tag to a sprite.
func (c *Client) TagSprite(name, tag string) error {
	_, err := c.call("tag", map[string]string{"name": name, "tag": tag})
	return err
}

// UntagSprite removes a tag from a sprite.
func (c *Client) UntagSprite(name, tag string) error {
	_, err := c.call("untag", map[string]string{"name": name, "tag": tag})
	return err
}

// GetTags returns all tags for a sprite.
func (c *Client) GetTags(name string) ([]string, error) {
	result, err := c.call("get_tags", map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := json.Unmarshal(result, &tags); err != nil {
		return nil, fmt.Errorf("decoding tags: %w", err)
	}
	return tags, nil
}

// ImportSprite imports an existing sprite into the database with optional local path and tags.
func (c *Client) ImportSprite(name, localPath string, tags []string) (*store.Sprite, error) {
	result, err := c.call("import", map[string]interface{}{
		"name":       name,
		"local_path": localPath,
		"tags":       tags,
	})
	if err != nil {
		return nil, err
	}
	var s store.Sprite
	if err := json.Unmarshal(result, &s); err != nil {
		return nil, fmt.Errorf("decoding imported sprite: %w", err)
	}
	return &s, nil
}

// Subscribe registers for real-time state updates from the daemon.
// Returns a channel that receives updates. The channel is closed when
// the connection ends.
func (c *Client) Subscribe() (<-chan StateUpdate, error) {
	_, err := c.call("subscribe", nil)
	if err != nil {
		return nil, err
	}

	ch := make(chan StateUpdate, 100)
	go func() {
		defer close(ch)
		for {
			var update StateUpdate
			if err := c.decoder.Decode(&update); err != nil {
				return
			}
			ch <- update
		}
	}()

	return ch, nil
}
