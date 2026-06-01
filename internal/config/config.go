package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	TMDB       TMDBConfig       `yaml:"tmdb"`
	Indexer    IndexerConfig    `yaml:"indexer"`
	TorrServer TorrServerConfig `yaml:"torrserver"`
	Jellyfin   JellyfinConfig   `yaml:"jellyfin"`
	Libraries  []Library        `yaml:"libraries"`
	Picker     PickerConfig     `yaml:"picker"`
	VPN        VPNConfig        `yaml:"vpn"`

	// Legacy flat library config — auto-migrated to Libraries during Load.
	LegacyLibrary legacyLibraryConfig `yaml:"library"`
}

// VPNConfig holds the orchestrator-owned WireGuard config directory used by the dashboard's
// upload/activate feature. FHS-standard, not user-specific (release-installer friendly).
type VPNConfig struct {
	ConfigDir string `yaml:"config_dir"` // default /var/lib/jellyfreedom/vpnconfigs
}

// VPNConfigDir returns the configured config dir or the portable default.
func (c *Config) VPNConfigDir() string {
	if c.VPN.ConfigDir != "" {
		return c.VPN.ConfigDir
	}
	return "/var/lib/jellyfreedom/vpnconfigs"
}

// Library is a named content destination: a directory, a media type, and optional picker overrides.
type Library struct {
	Name    string        `yaml:"name"`
	Type    string        `yaml:"type"`    // "movie" | "tv"
	Path    string        `yaml:"path"`
	Default bool          `yaml:"default"` // used when no library is explicitly requested
	Adult   bool          `yaml:"adult"`   // infrastructure flag — kept here for future filtering
	Picker  *PickerConfig `yaml:"picker,omitempty"`
}

type legacyLibraryConfig struct {
	MoviesDir string `yaml:"movies_dir"`
	TVDir     string `yaml:"tv_dir"`
}

type ServerConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
}

type TMDBConfig struct {
	APIKey string `yaml:"api_key"`
}

type IndexerConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type TorrServerConfig struct {
	BaseURL string            `yaml:"base_url"`
	Cache   TorrCacheConfig   `yaml:"cache"`
}

// TorrCacheConfig is applied to TorrServer's /settings on startup so the same
// binary adapts to the host (RAM-rich server vs low-RAM device with an SSD).
type TorrCacheConfig struct {
	Mode               string `yaml:"mode"`                 // "ram" | "disk" (empty = leave TorrServer as-is)
	SizeMB             int    `yaml:"size_mb"`              // cache ring-buffer size
	Path               string `yaml:"path"`                 // required when mode=disk
	DisconnectTimeoutS int    `yaml:"disconnect_timeout_s"` // how long an idle torrent stays warm
	ConnectionsLimit   int    `yaml:"connections_limit"`    // peer connections (higher = faster buffering)
	RetrackersMode     *int   `yaml:"retrackers_mode"`      // 0 off, 1 add, 2 replace (nil = leave as-is)
	UploadRateLimitKB  int    `yaml:"upload_rate_limit_kb"` // low nonzero keeps tit-for-tat healthy
}

type JellyfinConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type PickerConfig struct {
	MinSeeders        int      `yaml:"min_seeders"`
	PreferVideoCodecs []string `yaml:"prefer_video_codecs"`
	PreferAudioCodecs []string `yaml:"prefer_audio_codecs"`
	PreferContainers  []string `yaml:"prefer_containers"`
	MaxSizeGB         int      `yaml:"max_size_gb"`
	RejectCAM         *bool    `yaml:"reject_cam"` // nil = default (true): never auto-pick camera/telesync rips
}

// RejectCAMValue resolves the reject_cam setting, defaulting to true (reject).
func (p PickerConfig) RejectCAMValue() bool {
	return p.RejectCAM == nil || *p.RejectCAM
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	// env overrides for secrets
	if v := os.Getenv("TMDB_API_KEY"); v != "" {
		cfg.TMDB.APIKey = v
	}
	if v := os.Getenv("INDEXER_API_KEY"); v != "" {
		cfg.Indexer.APIKey = v
	}
	if v := os.Getenv("JELLYFIN_API_KEY"); v != "" {
		cfg.Jellyfin.APIKey = v
	}
	if v := os.Getenv("LISTEN"); v != "" {
		cfg.Server.Listen = v
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &cfg, nil
}

func validate(cfg *Config) error {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "127.0.0.1:8080"
	}
	if cfg.Server.PublicURL == "" {
		// Set the LAN-reachable URL in the dashboard/config for correct .strm links.
		cfg.Server.PublicURL = "http://localhost:1990"
	}
	// TMDB / Indexer / Jellyfin keys and URLs may be empty at startup — they are entered in
	// the dashboard (stored in the DB and applied to the clients at runtime). So never hard-fail
	// here; just provide URL defaults. The dashboard's Health panel shows what's unconfigured.
	if cfg.Indexer.BaseURL == "" {
		cfg.Indexer.BaseURL = "http://127.0.0.1:9696"
	}
	if cfg.TorrServer.BaseURL == "" {
		cfg.TorrServer.BaseURL = "http://127.0.0.1:8090"
	}
	if cfg.Jellyfin.BaseURL == "" {
		cfg.Jellyfin.BaseURL = "http://127.0.0.1:8096"
	}

	// Migrate legacy flat library config if no named libraries defined
	if len(cfg.Libraries) == 0 {
		if cfg.LegacyLibrary.MoviesDir != "" {
			cfg.Libraries = append(cfg.Libraries, Library{
				Name: "Movies", Type: "movie",
				Path: cfg.LegacyLibrary.MoviesDir, Default: true,
			})
		}
		if cfg.LegacyLibrary.TVDir != "" {
			cfg.Libraries = append(cfg.Libraries, Library{
				Name: "TV Shows", Type: "tv",
				Path: cfg.LegacyLibrary.TVDir, Default: true,
			})
		}
	}
	if len(cfg.Libraries) == 0 {
		return fmt.Errorf("at least one library must be configured under 'libraries:'")
	}
	for i, lib := range cfg.Libraries {
		if lib.Name == "" {
			return fmt.Errorf("libraries[%d]: name is required", i)
		}
		if lib.Type != "movie" && lib.Type != "tv" {
			return fmt.Errorf("libraries[%d] (%s): type must be 'movie' or 'tv'", i, lib.Name)
		}
		if lib.Path == "" {
			return fmt.Errorf("libraries[%d] (%s): path is required", i, lib.Name)
		}
	}

	// Picker defaults
	if cfg.Picker.MinSeeders == 0 {
		cfg.Picker.MinSeeders = 5
	}
	if cfg.Picker.MaxSizeGB == 0 {
		cfg.Picker.MaxSizeGB = 20
	}
	if len(cfg.Picker.PreferVideoCodecs) == 0 {
		cfg.Picker.PreferVideoCodecs = []string{"h264", "h265", "hevc"}
	}
	if len(cfg.Picker.PreferAudioCodecs) == 0 {
		cfg.Picker.PreferAudioCodecs = []string{"aac", "ac3", "eac3"}
	}
	if len(cfg.Picker.PreferContainers) == 0 {
		cfg.Picker.PreferContainers = []string{"mp4", "mkv"}
	}
	return nil
}

// DefaultLibrary returns the default library for a given media type,
// falling back to the first library of that type if none is marked default.
func (c *Config) DefaultLibrary(mediaType string) *Library {
	var first *Library
	for i := range c.Libraries {
		if c.Libraries[i].Type != mediaType {
			continue
		}
		if first == nil {
			first = &c.Libraries[i]
		}
		if c.Libraries[i].Default {
			return &c.Libraries[i]
		}
	}
	return first
}

// FindLibrary looks up a library by name (case-sensitive).
func (c *Config) FindLibrary(name string) *Library {
	for i := range c.Libraries {
		if c.Libraries[i].Name == name {
			return &c.Libraries[i]
		}
	}
	return nil
}

// PickerFor returns the effective picker config for a library,
// merging library-level overrides on top of the global picker.
// The return type matches picker.Config field-for-field.
func (c *Config) PickerFor(lib *Library) PickerConfig {
	if lib == nil || lib.Picker == nil {
		return c.Picker
	}
	merged := c.Picker
	lp := lib.Picker
	if lp.MinSeeders > 0 {
		merged.MinSeeders = lp.MinSeeders
	}
	if lp.MaxSizeGB > 0 {
		merged.MaxSizeGB = lp.MaxSizeGB
	}
	if len(lp.PreferVideoCodecs) > 0 {
		merged.PreferVideoCodecs = lp.PreferVideoCodecs
	}
	if len(lp.PreferAudioCodecs) > 0 {
		merged.PreferAudioCodecs = lp.PreferAudioCodecs
	}
	if len(lp.PreferContainers) > 0 {
		merged.PreferContainers = lp.PreferContainers
	}
	return merged
}
