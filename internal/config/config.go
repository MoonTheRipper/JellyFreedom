package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"jellyfreedom/internal/picker"
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
	WebSources WebSourcesConfig `yaml:"web_sources"`

	// Legacy flat library config — auto-migrated to Libraries during Load.
	LegacyLibrary legacyLibraryConfig `yaml:"library"`
}

// VPNConfig holds the orchestrator-owned WireGuard config directory used by the dashboard's
// upload/activate feature. FHS-standard, not user-specific (release-installer friendly).
type VPNConfig struct {
	ConfigDir string `yaml:"config_dir"` // default /var/lib/jellyfreedom/vpnconfigs
}

// WebSourcesConfig configures the paste-a-link feature: a video page URL is extracted
// with yt-dlp and becomes a playable library entry.
//
// It is OFF unless enabled, because it has an external dependency (yt-dlp) that the
// installer may not have been able to fetch, and a feature whose button is present but
// whose backend is missing is worse than one that is absent.
type WebSourcesConfig struct {
	Enabled bool `yaml:"enabled"`
	// YTDLPPath is where yt-dlp lives. Empty means "find yt-dlp on PATH".
	YTDLPPath string `yaml:"ytdlp_path"`
	// TempDir is scratch space for the extractor. Empty means the standard location
	// under the state directory.
	//
	// It needs a default, and the default must not be /tmp. The official yt-dlp binary
	// unpacks ~76MB of itself into TMPDIR on every run, and /tmp is a RAM-backed tmpfs
	// on a stock Ubuntu — so leaving it alone spends RAM per extraction and fails with
	// an unreadable PyInstaller error once that tmpfs is full.
	TempDir string `yaml:"temp_dir"`
	// ProxyAddr is the host:port of the SOCKS proxy running INSIDE the vpntorrent
	// namespace — `orchestrator netns-proxy`, started by jf-netnsproxy.service.
	//
	// It has no fallback to "direct" and never will. Extraction and playback both
	// identify the requester to the site, so a web source fetched without the tunnel
	// puts the user's home address on a request to it. An empty value disables the
	// feature; it does not enable a direct one.
	ProxyAddr string `yaml:"proxy_addr"`
}

// WebSourcesProxyAddr returns the configured proxy address, defaulting to the SOCKS port
// on the namespace's veth address — the same 10.42.0.2 the TorrServer default names, so
// a stock install needs no extra configuration.
func (c *Config) WebSourcesProxyAddr() string {
	if c.WebSources.ProxyAddr != "" {
		return c.WebSources.ProxyAddr
	}
	return "10.42.0.2:1080"
}

// WebSourcesTempDir returns the extractor's scratch directory, defaulting to a
// disk-backed path under the FHS state directory rather than to /tmp.
func (c *Config) WebSourcesTempDir() string {
	if c.WebSources.TempDir != "" {
		return c.WebSources.TempDir
	}
	return "/var/lib/jellyfreedom/tmp"
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
	Type    string        `yaml:"type"` // "movie" | "tv"
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
	// SecureCookies sets the Secure flag on the session cookie. Default FALSE: the
	// primary deployment is plain HTTP on a LAN, where Secure would stop the browser
	// sending the cookie at all and log every user out instantly. Turn it on when a
	// TLS-terminating reverse proxy sits in front.
	SecureCookies bool `yaml:"secure_cookies"`
}

type TMDBConfig struct {
	APIKey string `yaml:"api_key"`
}

type IndexerConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type TorrServerConfig struct {
	BaseURL string          `yaml:"base_url"`
	Cache   TorrCacheConfig `yaml:"cache"`
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

// PickerConfig is the user-facing quality policy. It deliberately exposes INTENT — what
// resolution you want, whether a transcode is acceptable, how much bitrate your link can
// carry — and not the individual point weights. Those stay hardcoded in the picker: a
// scoring spreadsheet in YAML is a support burden nobody can reason about, and the
// weights are only meaningful relative to one another.
type PickerConfig struct {
	MinSeeders        int      `yaml:"min_seeders"`
	PreferVideoCodecs []string `yaml:"prefer_video_codecs"`
	PreferAudioCodecs []string `yaml:"prefer_audio_codecs"`
	PreferContainers  []string `yaml:"prefer_containers"`
	MaxSizeGB         int      `yaml:"max_size_gb"`
	RejectCAM         *bool    `yaml:"reject_cam"` // nil = default (true): never auto-pick camera/telesync rips

	// TargetResolution is the rung the picker aims for: "2160p", "1080p", "720p",
	// "576p" or "480p" ("4k"/"uhd" are accepted spellings of 2160p). Empty = 1080p.
	// Releases are scored by distance from it, and ABOVE it scores lower than at it —
	// 4K costs three to four times the bitrate, which on a swarm-fed stream buys stalls
	// more often than detail.
	TargetResolution string `yaml:"target_resolution"`

	// RequireDirectPlay makes "Apple TV can play this without the server transcoding it"
	// a hard filter instead of a strong preference. nil = false, and that default is
	// deliberate: turning it on for an existing install would silently empty release
	// lists that work fine today. Turn it on if playback stutters on the Apple TV.
	RequireDirectPlay *bool `yaml:"require_direct_play"`

	// MaxMbps is the average bitrate the link between TorrServer and the player can
	// comfortably sustain. 0 = no cap. It only takes effect when the item's runtime is
	// known, since bitrate is size ÷ runtime.
	MaxMbps int `yaml:"max_mbps"`
}

// RejectCAMValue resolves the reject_cam setting, defaulting to true (reject).
func (p PickerConfig) RejectCAMValue() bool {
	return p.RejectCAM == nil || *p.RejectCAM
}

// RequireDirectPlayValue resolves the require_direct_play setting, defaulting to false.
func (p PickerConfig) RequireDirectPlayValue() bool {
	return p.RequireDirectPlay != nil && *p.RequireDirectPlay
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
	if err := validatePublicURL(cfg.Server.PublicURL); err != nil {
		return err
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
		// "hevc" used to be the third entry here and could never match anything: the
		// indexer folds x265/HEVC/H265 into the single name "h265" while parsing the
		// release title, so the picker never sees the string "hevc" on a release. The
		// dead entry also inflated the weights — with three entries h264 scored 120 and
		// h265 80, purely because of a value that matched nothing. The picker now also
		// canonicalises codec names, so an existing config that still says "hevc" keeps
		// working; the default no longer ships the inconsistency.
		cfg.Picker.PreferVideoCodecs = []string{"h264", "h265"}
	}
	if len(cfg.Picker.PreferAudioCodecs) == 0 {
		cfg.Picker.PreferAudioCodecs = []string{"aac", "ac3", "eac3"}
	}
	if len(cfg.Picker.PreferContainers) == 0 {
		cfg.Picker.PreferContainers = []string{"mp4", "mkv"}
	}
	if cfg.Picker.TargetResolution == "" {
		cfg.Picker.TargetResolution = picker.DefaultTargetResolution
	}
	// Validate the global picker and every library override. A typo'd target resolution
	// is worse than a hard failure at startup would suggest: it silently falls back to
	// the default, so the user's deliberate "I want 2160p" quietly does nothing and the
	// only symptom is picks that feel wrong months later.
	if err := validatePicker(&cfg.Picker, "picker"); err != nil {
		return err
	}
	for i := range cfg.Libraries {
		if cfg.Libraries[i].Picker == nil {
			continue
		}
		where := fmt.Sprintf("libraries[%d] (%s).picker", i, cfg.Libraries[i].Name)
		if err := validatePicker(cfg.Libraries[i].Picker, where); err != nil {
			return err
		}
	}
	return nil
}

// validatePicker checks and canonicalises one picker block, in place.
//
// picker.NormaliseTargetResolution is the single authority on which spellings are
// accepted, so importing the picker here is on purpose: a private copy of the table in
// this package would drift the moment a rung is added, and the failure would be a
// config value the validator accepts and the picker then ignores.
func validatePicker(p *PickerConfig, where string) error {
	if p.TargetResolution != "" {
		canonical := picker.NormaliseTargetResolution(p.TargetResolution)
		if canonical == "" {
			return fmt.Errorf("%s.target_resolution %q is not a resolution — "+
				"use one of 2160p (or 4k), 1080p, 720p, 576p, 480p", where, p.TargetResolution)
		}
		p.TargetResolution = canonical
	}
	if p.MaxMbps < 0 {
		return fmt.Errorf("%s.max_mbps must be 0 (no cap) or a positive megabits-per-second "+
			"value, got %d", where, p.MaxMbps)
	}
	if p.MinSeeders < 0 {
		return fmt.Errorf("%s.min_seeders must be 0 or more, got %d", where, p.MinSeeders)
	}
	if p.MaxSizeGB < 0 {
		return fmt.Errorf("%s.max_size_gb must be 0 (unlimited) or more, got %d", where, p.MaxSizeGB)
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
	if lp.TargetResolution != "" {
		merged.TargetResolution = lp.TargetResolution
	}
	if lp.MaxMbps > 0 {
		merged.MaxMbps = lp.MaxMbps
	}
	// The *bool overrides test for non-nil, not for truth: that is the whole reason they
	// are pointers. reject_cam was silently NOT merged at all, so a library that set
	// `reject_cam: false` (an adult library, say, where camera rips may be all that
	// exists) still had the global `true` applied and got "no suitable release found"
	// with no indication that its own setting had been discarded.
	if lp.RejectCAM != nil {
		merged.RejectCAM = lp.RejectCAM
	}
	if lp.RequireDirectPlay != nil {
		merged.RequireDirectPlay = lp.RequireDirectPlay
	}
	return merged
}

// publicURLPlaceholders are the literal values the installer template ships with. If one
// of them survives into a running config, every .strm written from that point on contains
// an unreachable host — which is precisely the "Jellyfin shows it as ready but it won't
// play" failure, and it is invisible until someone tries to watch something.
//
// Hard-failing at startup is the right trade: a config that cannot produce a playable
// .strm is not a config the service should run with, and the message names the exact fix.
var publicURLPlaceholders = []string{
	"change-me-lan-ip",
	"changeme",
	"your-lan-ip",
	"<lan-ip>",
	"lan-ip-here",
}

// validatePublicURL rejects a public_url that could never produce a working .strm.
func validatePublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("server.public_url %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("server.public_url %q must start with http:// or https:// — "+
			"it is written verbatim into every .strm file", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("server.public_url %q has no host — "+
			"set it to this machine's LAN address, e.g. http://192.168.1.50:1990", raw)
	}

	lower := strings.ToLower(raw)
	for _, ph := range publicURLPlaceholders {
		if strings.Contains(lower, ph) {
			return fmt.Errorf("server.public_url is still the installer placeholder (%q). "+
				"Set it to this machine's LAN address, e.g. http://192.168.1.50:1990 — "+
				"this value is baked into every .strm file, so until it is correct Jellyfin "+
				"will list items as ready but refuse to play them", raw)
		}
	}
	return nil
}

// WarnIfPublicURLNotReachableFromLAN reports a public_url that is syntactically fine but
// only resolvable on this machine. It is a WARNING, not an error: a single-box setup where
// Jellyfin runs locally genuinely works this way, and someone may be mid-setup. But an
// Apple TV cannot reach 127.0.0.1, so it is worth saying out loud.
func (c *Config) WarnIfPublicURLNotReachableFromLAN() string {
	u, err := url.Parse(c.Server.PublicURL)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return fmt.Sprintf("server.public_url is %q, which only this machine can reach. "+
			"Every .strm points there, so other devices (Apple TV, phones) will not be able "+
			"to play anything. Set it to this machine's LAN address.", c.Server.PublicURL)
	}
	return ""
}
