package sprite

import "time"

// Info represents a sprite as returned by the Sprites API.
type Info struct {
	ID                 string      `json:"id"`
	Name               string      `json:"name"`
	Status             string      `json:"status"` // running, warm, cold
	Version            string      `json:"version"`
	URL                string      `json:"url"`
	UpdatedAt          time.Time   `json:"updated_at"`
	CreatedAt          time.Time   `json:"created_at"`
	Organization       string      `json:"organization"`
	URLSettings        *URLSetting `json:"url_settings"`
	LastStartedAt      *time.Time  `json:"last_started_at"`
	LastActiveAt       *time.Time  `json:"last_active_at"`
	EnvironmentVersion *string     `json:"environment_version"`
}

// URLSetting holds the authentication setting for a sprite's public URL.
type URLSetting struct {
	Auth string `json:"auth"` // "sprite" or "public"
}

// ListResponse is the API response for listing sprites.
type ListResponse struct {
	Sprites               []Info  `json:"sprites"`
	NextContinuationToken *string `json:"next_continuation_token"`
	HasMore               bool    `json:"has_more"`
}

// Service represents a sprite service configuration.
type Service struct {
	Port    int    `json:"port"`
	Command string `json:"command"`
	Primary bool   `json:"primary"`
}

// ExecOptions configures a sprite exec call.
type ExecOptions struct {
	Sprite  string
	Org     string
	Command []string
	TTY     bool
	Dir     string
	Env     map[string]string
	Files   map[string]string // local:remote pairs
	Detach  bool
}

// ProxyOptions configures a sprite proxy call.
type ProxyOptions struct {
	Sprite string
	Org    string
	Ports  []string // "local:remote" or just "port"
}
