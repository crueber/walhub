// Package config: the walhub configuration (11_config_cli.md — key names, defaults, and
// meanings are normative and byte-compatible with the Rust spec §15.1).
// CONTRACT FILE: the Config struct + duration/byte-size scalar types + compiled-in Defaults().
// Loading (TOML file, WALHUB__/WALGIT__ env overlay, PORT lockstep, validation, per-repo
// settings merge, setup save) is implemented by the config owner in sibling files.
package config

import "time"

// Duration accepts Rust-spec spellings on the wire/TOML: "5ms", "1h", "30d", "7d", "0s".
// (time.ParseDuration has no day/week units; this type adds them.)
type Duration time.Duration

// ByteSize accepts "20GiB", "64MiB", "1GiB", "0B", "512MB" spellings.
type ByteSize int64

// --- TLS ---

type TLSStruct struct {
	Mode      string   `toml:"mode"`      // "off" | "self_signed" | "files" (default "off")
	Cert      string   `toml:"cert"`      // files mode: PEM chain
	Key       string   `toml:"key"`       // files mode: PKCS#8/PKCS#1 key
	Hostnames []string `toml:"hostnames"` // self_signed SANs
}

// --- Auth ---

type StaticToken struct {
	Principal string `toml:"principal"`
	Token     string `toml:"token"`     // literal secret (one of token/token_env required)
	TokenEnv  string `toml:"token_env"` // env var NAME read at startup; overrides token
	Write     bool   `toml:"write"`
	Admin     bool   `toml:"admin"`
}

type Auth struct {
	Mode              string        `toml:"mode"` // "none" | "token" | "oidc" (default "none")
	AnonymousRead     bool          `toml:"anonymous_read"`
	Tokens            []StaticToken `toml:"tokens"`
	AdminEmails       []string      `toml:"admin_emails"`
	AdminDomains      []string      `toml:"admin_domains"`
	Issuer            string        `toml:"issuer"`
	AllowedDomains    []string      `toml:"allowed_domains"`
	AllowedEmails     []string      `toml:"allowed_emails"`
	WriteDomains      []string      `toml:"write_domains"`
	OAuthClientID     string        `toml:"oauth_client_id"`
	OAuthClientSecret string        `toml:"oauth_client_secret"`
	SessionSecret     string        `toml:"session_secret"`
	SessionTTL        Duration      `toml:"session_ttl"`      // 30d
	AccessTokenTTL    Duration      `toml:"access_token_ttl"` // 90d
	Audiences         []string      `toml:"audiences"`
	TrustedForwarders []string      `toml:"trusted_forwarders"`
}

// --- Server ---

type Server struct {
	Listen                string    `toml:"listen"` // default "127.0.0.1:8080"; first run "0.0.0.0:8080"
	HTTP2                 bool      `toml:"http2"`
	MaxConcurrentRequests int       `toml:"max_concurrent_requests"`
	MaxConcurrentPerRepo  int       `toml:"max_concurrent_per_repo"`
	RequestTimeout        Duration  `toml:"request_timeout"`
	DrainTimeout          Duration  `toml:"drain_timeout"`
	MaxPushBytes          ByteSize  `toml:"max_push_bytes"`
	Roles                 []string  `toml:"roles"` // serve | maintain | events; empty = all
	AutoCreateOnPush      bool      `toml:"auto_create_on_push"`
	AccelRedirect         bool      `toml:"accel_redirect"`
	PublicURL             string    `toml:"public_url"`
	CorsOrigins           []string  `toml:"cors_origins"`
	TLS                   TLSStruct `toml:"tls"`
	Auth                  Auth      `toml:"auth"`
	SSH                   ServerSSH `toml:"ssh"`
}

// --- Server SSH (17_ssh.md) ---

// ServerSSH is the SSH git transport listener: disabled unless Listen is set.
// User public keys are NOT config: authenticated users manage their own keys
// through the UI/API, stored in the object store (17_ssh.md §3). The TOML
// surface is the server only: listener + host key.
type ServerSSH struct {
	Listen     string `toml:"listen"`       // e.g. "0.0.0.0:2222"; empty = disabled
	HostKey    string `toml:"host_key"`     // path to an OpenSSH/PEM private key
	HostKeyEnv string `toml:"host_key_env"` // env var NAME holding the private key; overrides host_key
}

// --- Store ---

type S3 struct {
	Endpoint       string `toml:"endpoint"`
	Region         string `toml:"region"`
	AccessKeyEnv   string `toml:"access_key_env"`
	SecretKeyEnv   string `toml:"secret_key_env"`
	ForcePathStyle bool   `toml:"force_path_style"`
}

type GCS struct {
	Endpoint              string `toml:"endpoint"` // override (emulators)
	SigningServiceAccount string `toml:"signing_service_account"`
	BulkClients           int    `toml:"bulk_clients"`
	BulkConcurrency       int    `toml:"bulk_concurrency"`
}

type Store struct {
	Backend            string   `toml:"backend"` // "s3" | "gcs" | "memory" | "filesystem"
	Bucket             string   `toml:"bucket"`
	Prefix             string   `toml:"prefix"`
	Root               string   `toml:"root"` // filesystem backend root (D4)
	MaxRetries         int      `toml:"max_retries"`
	MultipartThreshold ByteSize `toml:"multipart_threshold"`
	MultipartPartSize  ByteSize `toml:"multipart_part_size"`
	S3                 S3       `toml:"s3"`
	GCS                GCS      `toml:"gcs"`
}

// --- Cache ---

type Cache struct {
	Dir                 string   `toml:"dir"`
	Mode                string   `toml:"mode"` // "budget" | "disk" | "auto"
	MaxBytes            ByteSize `toml:"max_bytes"`
	DiskHighWatermark   float64  `toml:"disk_high_watermark"`
	EvictIdleAfter      Duration `toml:"evict_idle_after"`
	Prewarm             []string `toml:"prewarm"`
	PrewarmParallelism  int      `toml:"prewarm_parallelism"`
	PrewarmReadyTimeout Duration `toml:"prewarm_ready_timeout"`
	RefAdvertEntries    int      `toml:"ref_advert_entries"`
	ObjectInfoEntries   int      `toml:"object_info_entries"`
	BundleListEntries   int      `toml:"bundle_list_entries"`
	RemoteBlockBytes    ByteSize `toml:"remote_block_bytes"`
	RemoteObjectBytes   ByteSize `toml:"remote_object_bytes"`
	SharedRenderCache   bool     `toml:"shared_render_cache"`
	StoreMount          string   `toml:"store_mount"`
}

// --- WAL ---

type WAL struct {
	BatchWindow           Duration `toml:"batch_window"`
	MaxBatch              int      `toml:"max_batch"`
	PushBrokerURL         string   `toml:"push_broker_url"`
	PushBrokerToken       string   `toml:"push_broker_token"`
	PushBrokerBufferBytes ByteSize `toml:"push_broker_buffer_bytes"`
	SnapshotEveryEntries  uint64   `toml:"snapshot_every_entries"`
	CheckpointInterval    Duration `toml:"checkpoint_interval"`
	CheckpointTailBytes   ByteSize `toml:"checkpoint_tail_bytes"`
	CASMaxRetries         uint32   `toml:"cas_max_retries"`
	FsckObjects           bool     `toml:"fsck_objects"`
	CheckConnectivity     bool     `toml:"check_connectivity"`
	FreshnessTTL          Duration `toml:"freshness_ttl"`
	PrefetchPacks         bool     `toml:"prefetch_packs"`
	PrefetchMaxBytes      ByteSize `toml:"prefetch_max_bytes"`
	RemoteObjects         bool     `toml:"remote_objects"`
}

// --- Maintenance / placement / compaction ---

type Maintenance struct {
	Interval       Duration `toml:"interval"`
	Checkpoints    bool     `toml:"checkpoints"`
	MaxPackBytes   ByteSize `toml:"max_pack_bytes"`
	Disk           string   `toml:"disk"` // "tmpfs" | "ssd"
	Host           string   `toml:"host"`
	FsckInterval   Duration `toml:"fsck_interval"`
	FollowInterval Duration `toml:"follow_interval"`
}

type Placement struct {
	Serve           []string `toml:"serve"`
	ServeExclude    []string `toml:"serve_exclude"`
	Maintain        []string `toml:"maintain"`
	MaintainExclude []string `toml:"maintain_exclude"`
}

type Compaction struct {
	Enabled             bool     `toml:"enabled"`
	Factor              int      `toml:"factor"`
	TriggerPacks        int      `toml:"trigger_packs"`
	TriggerBytes        ByteSize `toml:"trigger_bytes"`
	LeaseTTL            Duration `toml:"lease_ttl"`
	RetentionSuperseded Duration `toml:"retention_superseded"`
	Engine              string   `toml:"engine"` // "git"
}

// --- Bundles ---

type BundleStrategy struct {
	Name        string   `toml:"name"`
	Kind        string   `toml:"kind"` // "full" | "incremental"
	Base        string   `toml:"base,omitempty"`
	Schedule    string   `toml:"schedule"` // 6-field UTC cron
	Keep        int      `toml:"keep,omitempty"`
	BackfillMax int      `toml:"backfill_max"`
	Chain       bool     `toml:"chain,omitempty"`
	Filter      string   `toml:"filter,omitempty"`
	Refs        []string `toml:"refs,omitempty"`
	MinCommits  int      `toml:"min_commits,omitempty"`
}

type Bundles struct {
	Strategy          []BundleStrategy `toml:"strategy"`
	MinCommits        int              `toml:"min_commits"`
	MinBytes          ByteSize         `toml:"min_bytes"`
	MainOnly          bool             `toml:"main_only"`
	ExtraRefs         []string         `toml:"extra_refs"`
	ServeVia          string           `toml:"serve_via"` // "proxy" | "signed_url"
	SignedURLTTL      Duration         `toml:"signed_url_ttl"`
	SignedURLFor      []string         `toml:"signed_url_for"`
	Advertise         bool             `toml:"advertise"`
	AdvertiseFiltered bool             `toml:"advertise_filtered"`
	Require           []string         `toml:"require"`
}

// --- LFS / upstream / git / telemetry / events ---

type LFS struct {
	Enabled        bool     `toml:"enabled"`
	ServeVia       string   `toml:"serve_via"`
	SignedURLTTL   Duration `toml:"signed_url_ttl"`
	MaxObjectBytes ByteSize `toml:"max_object_bytes"`
}

type Upstream struct {
	Git      string   `toml:"git"`
	Lfs      string   `toml:"lfs"`
	TokenEnv string   `toml:"token_env"`
	Follow   []string `toml:"follow"`
}

type Git struct {
	Binary                 string `toml:"binary"`
	UploadPackEngine       string `toml:"upload_pack_engine"` // auto|git|gix (gix == git at load)
	AllowFilter            bool   `toml:"allow_filter"`
	AllowAnySHA1InWant     bool   `toml:"allow_any_sha1_in_want"`
	ObjectFormat           string `toml:"object_format"`
	CommitGraph            bool   `toml:"commit_graph"`
	CommitGraphChangedPath bool   `toml:"commit_graph_changed_paths"`
	HistoryPack            bool   `toml:"history_pack"`
	MaxWants               int    `toml:"max_wants"`
}

type Telemetry struct {
	LogFormat    string   `toml:"log_format"` // "pretty" | "json"
	LogFilter    string   `toml:"log_filter"`
	Metrics      bool     `toml:"metrics"`
	LockWaitWarn Duration `toml:"lock_wait_warn"`
}

type Events struct {
	WebhookURL    string   `toml:"webhook_url"`
	WebhookSecret string   `toml:"webhook_secret"`
	SweepInterval Duration `toml:"sweep_interval"`
}

// Releases caps the collaboration release surface (docs/features/07 §1.2:
// releases.max_asset_bytes, default 2 GiB → 413 over cap).
type Releases struct {
	MaxAssetBytes ByteSize `toml:"max_asset_bytes"`
}

// Import bounds the repository-import surface (docs/features/10 §6:
// additive [import] section, 14_extensibility.md §14.12 — existing keys
// never change meaning). All timeouts are per-spawn ctx timeouts; sizes
// gate the scratch checkout (post-clone du) and published packs.
type Import struct {
	CloneTimeout         Duration `toml:"clone_timeout"`          // clone --mirror ctx (default 1800s)
	GitTimeout           Duration `toml:"git_timeout"`            // for-each-ref/verify/repack ctx (default 300s)
	MaxBytes             ByteSize `toml:"max_bytes"`              // scratch du + pack gate (default = server.max_push_bytes at load)
	MaxRefs              int      `toml:"max_refs"`               // ref-enumeration cap (default 100000)
	MaxConcurrent        int      `toml:"max_concurrent"`         // server-side concurrent clones (default 2)
	AllowPrivateNetworks bool     `toml:"allow_private_networks"` // SSRF: allow loopback/RFC1918 sources (default false)
	URLAllowlist         []string `toml:"url_allowlist"`          // empty = public hosts allowed; non-empty = only these hosts
	AllowFileURLs        bool     `toml:"allow_file_urls"`        // file:// sources (default false; tests/fixtures only)
}

// Config is the whole file. Section/field names match the TOML keys exactly.
type Config struct {
	Server      Server      `toml:"server"`
	Store       Store       `toml:"store"`
	Cache       Cache       `toml:"cache"`
	WAL         WAL         `toml:"wal"`
	Maintenance Maintenance `toml:"maintenance"`
	Placement   Placement   `toml:"placement"`
	Compaction  Compaction  `toml:"compaction"`
	Bundles     Bundles     `toml:"bundles"`
	LFS         LFS         `toml:"lfs"`
	Upstream    Upstream    `toml:"upstream"`
	Git         Git         `toml:"git"`
	Telemetry   Telemetry   `toml:"telemetry"`
	Events      Events      `toml:"events"`
	Releases    Releases    `toml:"releases"`
	Import      Import      `toml:"import"`

	// DataDir is the zero-config root (divergence D5): --data-dir / WALHUB_DATA_DIR,
	// default ~/.local/share/walhub. Holds store/, cache/, and the saved walhub.toml.
	// Not a TOML key (never loaded from the file); set by the loader.
	DataDir string `toml:"-"`
}

// Defaults returns the COMPILED-IN defaults (11_config_cli.md §2 — equal to Rust-spec defaults).
// The zero-config first run applies FirstRunDefaults(dataDir) on top (divergence D5, §2.3);
// the config owner implements it in sibling files.
func Defaults() *Config {
	return &Config{
		Server: Server{
			Listen:                "127.0.0.1:8080",
			HTTP2:                 true,
			MaxConcurrentRequests: 512,
			MaxConcurrentPerRepo:  64,
			RequestTimeout:        Duration(time.Hour),
			DrainTimeout:          Duration(20 * time.Second),
			MaxPushBytes:          64 << 30,
			Auth: Auth{
				Mode:           "none",
				AnonymousRead:  true,
				SessionTTL:     Duration(30 * 24 * time.Hour),
				AccessTokenTTL: Duration(90 * 24 * time.Hour),
			},
			TLS: TLSStruct{Mode: "off"},
		},
		Store: Store{
			Backend:            "s3",
			Bucket:             "walgit",
			MaxRetries:         8,
			MultipartThreshold: 64 << 20,
			MultipartPartSize:  32 << 20,
			S3: S3{
				Endpoint:     "https://s3.us-east-1.amazonaws.com",
				Region:       "us-east-1",
				AccessKeyEnv: "AWS_ACCESS_KEY_ID",
				SecretKeyEnv: "AWS_SECRET_ACCESS_KEY",
			},
			GCS: GCS{BulkClients: 4, BulkConcurrency: 32},
		},
		Cache: Cache{
			Dir:                "/tmp/walgit",
			Mode:               "auto",
			MaxBytes:           20 << 30,
			DiskHighWatermark:  0.9,
			EvictIdleAfter:     Duration(6 * time.Hour),
			PrewarmParallelism: 2,
			RefAdvertEntries:   256,
			ObjectInfoEntries:  4096,
			BundleListEntries:  128,
			RemoteBlockBytes:   1 << 30,
			RemoteObjectBytes:  256 << 20,
			SharedRenderCache:  true,
		},
		WAL: WAL{
			BatchWindow:           Duration(5 * time.Millisecond),
			MaxBatch:              64,
			PushBrokerBufferBytes: 64 << 20,
			SnapshotEveryEntries:  256,
			CheckpointInterval:    Duration(time.Hour),
			CheckpointTailBytes:   8 << 20,
			CASMaxRetries:         16,
			FsckObjects:           true,
			CheckConnectivity:     true,
			PrefetchPacks:         true,
			PrefetchMaxBytes:      1 << 30,
			RemoteObjects:         true,
		},
		Maintenance: Maintenance{
			Interval:       Duration(time.Minute),
			Checkpoints:    true,
			Disk:           "tmpfs",
			FsckInterval:   Duration(7 * 24 * time.Hour),
			FollowInterval: Duration(30 * time.Second),
		},
		Placement: Placement{Serve: []string{"*"}, Maintain: []string{"*"}},
		Compaction: Compaction{
			Enabled:             true,
			Factor:              2,
			TriggerPacks:        16,
			TriggerBytes:        1 << 30,
			LeaseTTL:            Duration(10 * time.Minute),
			RetentionSuperseded: Duration(7 * 24 * time.Hour),
			Engine:              "git",
		},
		Bundles: Bundles{
			Strategy: []BundleStrategy{
				{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0", Keep: 2, BackfillMax: 1}, // Sunday 23:00 UTC (@weekly; numeric dow — names are rejected by the parser)
				{Name: "daily", Kind: "incremental", Base: "weekly", Schedule: "0 0 23 * * *", BackfillMax: 0, Chain: true},
				{Name: "hourly", Kind: "incremental", Base: "daily", Schedule: "0 0 * * * *", BackfillMax: 48},
			},
			MinCommits:   25,
			MainOnly:     true,
			ServeVia:     "proxy",
			SignedURLTTL: Duration(time.Hour),
			Advertise:    true,
		},
		LFS: LFS{
			Enabled:        true,
			ServeVia:       "proxy",
			SignedURLTTL:   Duration(time.Hour),
			MaxObjectBytes: 16 << 30,
		},
		Git: Git{
			Binary:           "git",
			UploadPackEngine: "auto",
			AllowFilter:      true,
			ObjectFormat:     "sha1",
			CommitGraph:      true,
			HistoryPack:      true,
		},
		Telemetry: Telemetry{
			LogFormat:    "pretty",
			LogFilter:    "info,walgit=debug",
			Metrics:      true,
			LockWaitWarn: Duration(time.Second),
		},
		Events: Events{SweepInterval: Duration(5 * time.Minute)},
		Releases: Releases{
			MaxAssetBytes: 2 << 30,
		},
		Import: Import{
			CloneTimeout:  Duration(1800 * time.Second),
			GitTimeout:    Duration(300 * time.Second),
			MaxBytes:      64 << 30,
			MaxRefs:       100000,
			MaxConcurrent: 2,
		},
	}
}
