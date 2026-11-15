// Package pool turns the proxy's accounts into a pool of subscriptions that several people
// share. It adds what upstream has no notion of:
//
//   - tokens that belong to a person (access.go)
//   - rules per subscription, set by its owner: who may use it, the share of the week kept
//     for the owner, blocked models, fast mode (rules.go)
//   - which subscription serves a request: among those the person may use, the one whose
//     weekly reset is closest (scheduler.go)
//   - fast mode taken off a request for a subscription that does not allow it (fastmode.go)
//   - token counts per person, model and subscription (usage.go)
//   - an API for the site that manages all of this, on a port of its own (api.go)
//
// Everything is off unless POOL_STATE_DIR is set: without it this build behaves as upstream.
// With it, a request needs a key: one of the pool's tokens, or one of upstream's own list.
// Upstream's code calls into this package at three places only (see patches/ in the fork).
package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Config is read from the environment, so upstream's configuration file keeps its shape.
type Config struct {
	// StateDir holds state.json (tokens and rules) and usage.json. POOL_STATE_DIR.
	StateDir string
	// Listen is the address of the site API, empty for none. POOL_LISTEN, default ":8320".
	Listen string
	// SiteKeyFile holds the key the site sends to the site API. POOL_SITE_KEY_FILE.
	SiteKeyFile string
	// ManagementURL is upstream's management API, reached over loopback for sign-ins and
	// for removing an account. POOL_MANAGEMENT_URL.
	ManagementURL string
	// ManagementKeyFile holds the management key in clear. POOL_MANAGEMENT_KEY_FILE.
	ManagementKeyFile string
}

func configFromEnv() (Config, bool) {
	dir := strings.TrimSpace(os.Getenv("POOL_STATE_DIR"))
	if dir == "" {
		return Config{}, false
	}
	listen, set := os.LookupEnv("POOL_LISTEN")
	if !set {
		listen = ":8320"
	}
	url := strings.TrimSpace(os.Getenv("POOL_MANAGEMENT_URL"))
	if url == "" {
		url = "http://127.0.0.1:8317/v8/management"
	}
	return Config{
		StateDir:          dir,
		Listen:            strings.TrimSpace(listen),
		SiteKeyFile:       strings.TrimSpace(os.Getenv("POOL_SITE_KEY_FILE")),
		ManagementURL:     strings.TrimRight(url, "/"),
		ManagementKeyFile: strings.TrimSpace(os.Getenv("POOL_MANAGEMENT_KEY_FILE")),
	}, true
}

// Pool is the running pool. There is at most one in a process.
type Pool struct {
	cfg     Config
	manager *coreauth.Manager
	state   *State
	usage   *Usage
	probes  *probes
	signins *signins
	now     func() time.Time
}

var (
	current atomic.Pointer[Pool]
	startMu sync.Mutex
)

// active returns the running pool, or nil when the pool is off.
func active() *Pool { return current.Load() }

// Start loads the pool's state and attaches it to the proxy. It returns an error when the
// pool is asked for but cannot start: the proxy must not serve without its rules.
func Start(ctx context.Context, manager *coreauth.Manager, access *sdkaccess.Manager) error {
	startMu.Lock()
	defer startMu.Unlock()
	if active() != nil {
		return nil
	}
	cfg, on := configFromEnv()
	if !on {
		log.Info("pool: off (POOL_STATE_DIR is not set)")
		return nil
	}
	if manager == nil || access == nil {
		return errors.New("pool: the service has no account manager or no access manager")
	}
	p, err := newPool(cfg, manager)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	if cfg.Listen != "" {
		if err := p.serveAPI(ctx); err != nil {
			return fmt.Errorf("pool: %w", err)
		}
	}
	current.Store(p)
	// From here on the proxy's list of ways to sign in is never empty, whatever its own key
	// list holds. An empty list would let everyone in. This runs before the proxy listens.
	sdkaccess.RegisterProvider(accessProviderName, accessProvider{})
	access.SetProviders(sdkaccess.RegisteredProviders())
	coreusage.RegisterNamedPlugin("pool", p.usage)
	coreauth.SetSchedulerDecorator(func(coreauth.PluginScheduler) coreauth.PluginScheduler {
		return &scheduler{pool: p}
	})
	manager.SetPluginScheduler(nil)
	go p.run(ctx)
	log.Infof("pool: on, state in %s, %d token(s), %d subscription rule(s)", cfg.StateDir, p.state.tokenCount(), p.state.ruleCount())
	return nil
}

func newPool(cfg Config, manager *coreauth.Manager) (*Pool, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	state, err := loadState(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	usage, err := loadUsage(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	p := &Pool{cfg: cfg, manager: manager, state: state, usage: usage, probes: newProbes(), signins: newSignins(), now: time.Now}
	usage.person = p.personOfPrincipal
	return p, nil
}

// run keeps what the pool holds in memory on disk and refreshes account usage pages.
// It ends with ctx, after a last save.
func (p *Pool) run(ctx context.Context) {
	save := time.NewTicker(5 * time.Second)
	defer save.Stop()
	probe := time.NewTicker(time.Minute)
	defer probe.Stop()
	for {
		select {
		case <-ctx.Done():
			p.flush()
			return
		case <-save.C:
			p.flush()
		case <-probe.C:
			p.readUsagePages(ctx)
		}
	}
}

func (p *Pool) flush() {
	if err := p.state.saveIfDirty(); err != nil {
		log.Errorf("pool: saving state: %v", err)
	}
	if err := p.usage.saveIfDirty(); err != nil {
		log.Errorf("pool: saving usage: %v", err)
	}
}
