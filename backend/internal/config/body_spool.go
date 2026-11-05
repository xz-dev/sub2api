package config

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/bodyspool"
	"github.com/spf13/viper"
)

// GatewayBodySpoolConfig is the large-request-capacity switch (design D2-D4):
// skeleton + blob spool on the covered Responses paths, the inline-bytes
// limit, fallback limits, the per-hop memory charge and the ticket lease.
// Off by default; zero numeric values take the defaults below.
type GatewayBodySpoolConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Dir is the spool directory: a disk volume, mode 0700, never tmpfs.
	Dir string `mapstructure:"dir"`
	// Threshold is T: tier-A strings of at least this many bytes are spooled.
	Threshold int `mapstructure:"threshold"`
	// InlineLimit is S for requests without a ticket (16 MiB).
	InlineLimit int64 `mapstructure:"inline_limit"`
	// TicketedInlineLimit is S for ticketed requests (8 MiB).
	TicketedInlineLimit int64 `mapstructure:"ticketed_inline_limit"`
	// FallbackLimit is L for paths that are not converted (at most 16 MiB).
	// The hop lowers it to the largest body whose charge fits beside
	// IdleBaseline under memory.max (design D2).
	FallbackLimit int64 `mapstructure:"fallback_limit"`
	// IdleBaseline is the hop's idle working set that the effective fallback
	// limit leaves room for (design D2, D4).
	IdleBaseline int64 `mapstructure:"idle_baseline"`
	// DiskReserve is the free-space floor of every spool write (1 GiB).
	DiskReserve int64 `mapstructure:"disk_reserve"`
	// LedgerDir is the read-only capacity-ledger mount; empty refuses every
	// body of 64 MiB or more.
	LedgerDir string `mapstructure:"ledger_dir"`
	// CgroupDir holds memory.max/memory.current; empty = the process cgroup.
	CgroupDir string `mapstructure:"cgroup_dir"`
	// KSkel, KMem, KPath and PageCacheBound are the per-hop memory charge
	// factors (design D4): k_skel x inline bytes on covered paths,
	// k_path x N on fallback paths, plus k_mem x N + P_h for a ticket.
	KSkel float64 `mapstructure:"k_skel"`
	// KConv is the factor of a request that a protocol converter rewrites
	// (Responses->Chat, Responses->Anthropic, Chat->Responses, ...): the
	// converter charges (k_conv - k) x the inline bytes on top of the
	// request's k_skel or k_path before it allocates, and refuses bodies
	// above S_conv (covered) or L_conv (fallback), both derived from k_conv.
	KConv          float64 `mapstructure:"k_conv"`
	KMem           float64 `mapstructure:"k_mem"`
	KPath          float64 `mapstructure:"k_path"`
	PageCacheBound int64   `mapstructure:"page_cache_bound"`
	// TicketForwardHosts are the internal upstream hosts (host[:port]) that
	// receive X-AI-Gateway-Capacity-Ticket (apisix-models). The header is
	// stripped for every other host and whenever the account uses a proxy.
	TicketForwardHosts []string `mapstructure:"ticket_forward_hosts"`
}

// Body spool defaults.
const (
	DefaultBodySpoolInlineLimit         int64 = 16 << 20
	DefaultBodySpoolTicketedInlineLimit int64 = 8 << 20
	DefaultBodySpoolFallbackLimit       int64 = 16 << 20
	// Settled idle working set measured at 320m: 33-38 MiB, once 57 MiB with
	// the executable's page cache active; design D4 uses 45 MiB.
	DefaultBodySpoolIdleBaseline int64 = 48 << 20
	// k_skel: a covered request served by a Responses-native account
	// (passthrough, the production setup: openai_responses_mode=
	// force_responses). Measured with the prompt guard on, (memory.peak -
	// idle memory.current) / 16 MiB in a 320m Sub2API, async and blocking,
	// after the passthrough normalizers stopped decoding unchanged bodies
	// (review rr3): at most 4.81 (d10-utext-16), 4.94 on invalid UTF-8 text
	// (d10-invalid-16, up to its pre-existing 502), 4.12 (d10-text-16), 3.00
	// (d10-small-items-16), 2.44 (d10-tiny-msgs-16), 1.62 (M0 text-16).
	// 6 is the worst plus ~20 %. S_h = (320 - 32 - 48) / 6 = 40 MiB >= S.
	DefaultBodySpoolKSkel float64 = 6
	// k_conv: a request a protocol converter rewrites (Responses -> Chat
	// Completions measured; every converter takes it). Same method on a
	// force_chat_completions account. In a 1g Sub2API at 16 MiB: at most
	// 18.88 (d10-tiny-msgs-16, ~35-byte messages; 22.6 x peak heap
	// in-process, review rr3), 14.69 (d10-small-items-16), 11.00
	// (d10-text-16), 10.31 (d10-utext-16). In a 320m Sub2API at 10 MiB
	// (below the old S_conv): 24.70 (d10-tiny-msgs-10), 15.40
	// (d10-small-items-10); k is higher on a smaller body in a tighter
	// container. 30 is the worst plus ~20 %. k_conv only sizes S_conv and
	// L_conv: S_conv at 320m = (320 - 32 - 48) / 30 = 8 MiB.
	DefaultBodySpoolKConv          float64 = 30
	DefaultBodySpoolKMem           float64 = 0.1
	DefaultBodySpoolKPath          float64 = 17
	DefaultBodySpoolPageCacheBound int64   = 16 << 20
)

// Effective returns c with zero values replaced by the defaults.
func (c GatewayBodySpoolConfig) Effective() GatewayBodySpoolConfig {
	if c.Threshold <= 0 {
		c.Threshold = bodyspool.DefaultThreshold
	}
	if c.InlineLimit <= 0 {
		c.InlineLimit = DefaultBodySpoolInlineLimit
	}
	if c.TicketedInlineLimit <= 0 {
		c.TicketedInlineLimit = DefaultBodySpoolTicketedInlineLimit
	}
	if c.FallbackLimit <= 0 {
		c.FallbackLimit = DefaultBodySpoolFallbackLimit
	}
	if c.IdleBaseline <= 0 {
		c.IdleBaseline = DefaultBodySpoolIdleBaseline
	}
	if c.DiskReserve <= 0 {
		c.DiskReserve = bodyspool.DefaultDiskReserve
	}
	if c.KSkel <= 0 {
		c.KSkel = DefaultBodySpoolKSkel
	}
	if c.KConv <= 0 {
		c.KConv = DefaultBodySpoolKConv
	}
	if c.KMem <= 0 {
		c.KMem = DefaultBodySpoolKMem
	}
	if c.KPath <= 0 {
		c.KPath = DefaultBodySpoolKPath
	}
	if c.PageCacheBound <= 0 {
		c.PageCacheBound = DefaultBodySpoolPageCacheBound
	}
	return c
}

func setBodySpoolDefaults() {
	viper.SetDefault("gateway.body_spool.enabled", false)
	viper.SetDefault("gateway.body_spool.dir", "")
	viper.SetDefault("gateway.body_spool.threshold", bodyspool.DefaultThreshold)
	viper.SetDefault("gateway.body_spool.inline_limit", DefaultBodySpoolInlineLimit)
	viper.SetDefault("gateway.body_spool.ticketed_inline_limit", DefaultBodySpoolTicketedInlineLimit)
	viper.SetDefault("gateway.body_spool.fallback_limit", DefaultBodySpoolFallbackLimit)
	viper.SetDefault("gateway.body_spool.idle_baseline", DefaultBodySpoolIdleBaseline)
	viper.SetDefault("gateway.body_spool.disk_reserve", bodyspool.DefaultDiskReserve)
	viper.SetDefault("gateway.body_spool.ledger_dir", "")
	viper.SetDefault("gateway.body_spool.cgroup_dir", "")
	viper.SetDefault("gateway.body_spool.k_skel", DefaultBodySpoolKSkel)
	viper.SetDefault("gateway.body_spool.k_conv", DefaultBodySpoolKConv)
	viper.SetDefault("gateway.body_spool.k_mem", DefaultBodySpoolKMem)
	viper.SetDefault("gateway.body_spool.k_path", DefaultBodySpoolKPath)
	viper.SetDefault("gateway.body_spool.page_cache_bound", DefaultBodySpoolPageCacheBound)
	viper.SetDefault("gateway.body_spool.ticket_forward_hosts", []string{})
}

// validateBodySpool refuses to start on a bad spool setup: tmpfs/ramfs, a
// wrong mode, a named file, no O_TMPFILE, a fallback limit above 16 MiB, or
// no cgroup memory limit to charge against.
func validateBodySpool(c GatewayBodySpoolConfig) error {
	if !c.Enabled {
		return nil
	}
	c = c.Effective()
	if c.FallbackLimit > DefaultBodySpoolFallbackLimit {
		return fmt.Errorf("gateway.body_spool.fallback_limit must be at most %d", DefaultBodySpoolFallbackLimit)
	}
	if c.TicketedInlineLimit > c.InlineLimit {
		return fmt.Errorf("gateway.body_spool.ticketed_inline_limit must not exceed inline_limit")
	}
	if _, err := bodyspool.OpenDir(c.Dir, c.DiskReserve); err != nil {
		return fmt.Errorf("gateway.body_spool.dir: %w", err)
	}
	m, err := bodyspool.OpenMeter(c.CgroupDir)
	if err != nil {
		return fmt.Errorf("gateway.body_spool: %w", err)
	}
	m.Close()
	return nil
}
