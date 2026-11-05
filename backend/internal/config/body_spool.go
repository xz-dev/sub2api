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
	FallbackLimit int64 `mapstructure:"fallback_limit"`
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
	KSkel          float64 `mapstructure:"k_skel"`
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
	// Measured on the patched skeleton path: 10.7-13.1 at a 16 MiB text body
	// in a 320m Sub2API (task 7.2 follow-up), rounded up with ~7 % margin.
	DefaultBodySpoolKSkel          float64 = 14
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
	if c.DiskReserve <= 0 {
		c.DiskReserve = bodyspool.DefaultDiskReserve
	}
	if c.KSkel <= 0 {
		c.KSkel = DefaultBodySpoolKSkel
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
	viper.SetDefault("gateway.body_spool.disk_reserve", bodyspool.DefaultDiskReserve)
	viper.SetDefault("gateway.body_spool.ledger_dir", "")
	viper.SetDefault("gateway.body_spool.cgroup_dir", "")
	viper.SetDefault("gateway.body_spool.k_skel", DefaultBodySpoolKSkel)
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
