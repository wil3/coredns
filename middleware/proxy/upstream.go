package proxy

import (
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coredns/coredns/middleware"
	"github.com/coredns/coredns/middleware/pkg/dnsutil"
	"github.com/coredns/coredns/middleware/pkg/tls"

	"github.com/mholt/caddy/caddyfile"
	"github.com/miekg/dns"
)

var (
	supportedPolicies = make(map[string]func() Policy)
)

type staticUpstream struct {
	from   string
	Hosts  HostPool
	Policy Policy
	Spray  Policy

	FailTimeout time.Duration
	MaxFails    int32
	HealthCheck struct {
		Path     string
		Port     string
		Interval time.Duration
	}
	WithoutPathPrefix string
	IgnoredSubDomains []string
	ex                Exchanger
}

// NewStaticUpstreams parses the configuration input and sets up
// static upstreams for the proxy middleware.
func NewStaticUpstreams(c *caddyfile.Dispenser) ([]Upstream, error) {
	var upstreams []Upstream
	for c.Next() {
		upstream := &staticUpstream{
			from:        ".",
			Hosts:       nil,
			Policy:      &Random{},
			Spray:       nil,
			FailTimeout: 10 * time.Second,
			MaxFails:    1,
			ex:          newDNSEx(),
		}

		if !c.Args(&upstream.from) {
			return upstreams, c.ArgErr()
		}
		to := c.RemainingArgs()
		if len(to) == 0 {
			return upstreams, c.ArgErr()
		}

		// process the host list, substituting in any nameservers in files
		toHosts, err := dnsutil.ParseHostPortOrFile(to...)
		if err != nil {
			return upstreams, err
		}

		for c.NextBlock() {
			if err := parseBlock(c, upstream); err != nil {
				return upstreams, err
			}
		}

		upstream.Hosts = make([]*UpstreamHost, len(toHosts))
		for i, host := range toHosts {
			uh := &UpstreamHost{
				Name:        host,
				Conns:       0,
				Fails:       0,
				FailTimeout: upstream.FailTimeout,
				Unhealthy:   false,

				CheckDown: func(upstream *staticUpstream) UpstreamHostDownFunc {
					return func(uh *UpstreamHost) bool {
						if uh.Unhealthy {
							return true
						}

						fails := atomic.LoadInt32(&uh.Fails)
						if fails >= upstream.MaxFails && upstream.MaxFails != 0 {
							return true
						}
						return false
					}
				}(upstream),
				WithoutPathPrefix: upstream.WithoutPathPrefix,
			}

			upstream.Hosts[i] = uh
		}

		if upstream.HealthCheck.Path != "" {
			go upstream.HealthCheckWorker(nil)
		}
		upstreams = append(upstreams, upstream)
	}
	return upstreams, nil
}

// RegisterPolicy adds a custom policy to the proxy.
func RegisterPolicy(name string, policy func() Policy) {
	supportedPolicies[name] = policy
}

func (u *staticUpstream) From() string {
	return u.from
}

func parseBlock(c *caddyfile.Dispenser, u *staticUpstream) error {
	switch c.Val() {
	case "policy":
		if !c.NextArg() {
			return c.ArgErr()
		}
		policyCreateFunc, ok := supportedPolicies[c.Val()]
		if !ok {
			return c.ArgErr()
		}
		u.Policy = policyCreateFunc()
	case "fail_timeout":
		if !c.NextArg() {
			return c.ArgErr()
		}
		dur, err := time.ParseDuration(c.Val())
		if err != nil {
			return err
		}
		u.FailTimeout = dur
	case "max_fails":
		if !c.NextArg() {
			return c.ArgErr()
		}
		n, err := strconv.Atoi(c.Val())
		if err != nil {
			return err
		}
		u.MaxFails = int32(n)
	case "health_check":
		if !c.NextArg() {
			return c.ArgErr()
		}
		var err error
		u.HealthCheck.Path, u.HealthCheck.Port, err = net.SplitHostPort(c.Val())
		if err != nil {
			return err
		}
		u.HealthCheck.Interval = 30 * time.Second
		if c.NextArg() {
			dur, err := time.ParseDuration(c.Val())
			if err != nil {
				return err
			}
			u.HealthCheck.Interval = dur
		}
	case "without":
		if !c.NextArg() {
			return c.ArgErr()
		}
		u.WithoutPathPrefix = c.Val()
	case "except":
		ignoredDomains := c.RemainingArgs()
		if len(ignoredDomains) == 0 {
			return c.ArgErr()
		}
		for i := 0; i < len(ignoredDomains); i++ {
			ignoredDomains[i] = strings.ToLower(dns.Fqdn(ignoredDomains[i]))
		}
		u.IgnoredSubDomains = ignoredDomains
	case "spray":
		u.Spray = &Spray{}
	case "protocol":
		encArgs := c.RemainingArgs()
		if len(encArgs) == 0 {
			return c.ArgErr()
		}
		switch encArgs[0] {
		case "dns":
			u.ex = newDNSEx()
		case "https_google":
			boot := []string{"8.8.8.8:53", "8.8.4.4:53"}
			if len(encArgs) > 2 && encArgs[1] == "bootstrap" {
				boot = encArgs[2:]
			}

			u.ex = newGoogle("", boot) // "" for default in google.go
		case "grpc":
			if len(encArgs) == 2 && encArgs[1] == "insecure" {
				u.ex = newGrpcClient(nil, u)
				return nil
			}
			tls, err := tls.NewTLSConfigFromArgs(encArgs[1:]...)
			if err != nil {
				return err
			}
			u.ex = newGrpcClient(tls, u)
		default:
			return fmt.Errorf("%s: %s", errInvalidProtocol, encArgs[0])
		}

	default:
		return c.Errf("unknown property '%s'", c.Val())
	}
	return nil
}

func (u *staticUpstream) healthCheck() {
	for _, host := range u.Hosts {
		port := ""
		if u.HealthCheck.Port != "" {
			port = ":" + u.HealthCheck.Port
		}
		hostURL := host.Name + port + u.HealthCheck.Path
		if r, err := http.Get(hostURL); err == nil {
			io.Copy(ioutil.Discard, r.Body)
			r.Body.Close()
			host.Unhealthy = r.StatusCode < 200 || r.StatusCode >= 400
		} else {
			host.Unhealthy = true
		}
	}
}

func (u *staticUpstream) HealthCheckWorker(stop chan struct{}) {
	ticker := time.NewTicker(u.HealthCheck.Interval)
	u.healthCheck()
	for {
		select {
		case <-ticker.C:
			u.healthCheck()
		case <-stop:
			// TODO: the library should provide a stop channel and global
			// waitgroup to allow goroutines started by plugins a chance
			// to clean themselves up.
		}
	}
}

func (u *staticUpstream) Select() *UpstreamHost {
	pool := u.Hosts
	if len(pool) == 1 {
		if pool[0].Down() && u.Spray == nil {
			return nil
		}
		return pool[0]
	}
	allDown := true
	for _, host := range pool {
		if !host.Down() {
			allDown = false
			break
		}
	}
	if allDown {
		if u.Spray == nil {
			return nil
		}
		return u.Spray.Select(pool)
	}

	if u.Policy == nil {
		h := (&Random{}).Select(pool)
		if h == nil && u.Spray == nil {
			return nil
		}
		return u.Spray.Select(pool)
	}

	h := u.Policy.Select(pool)
	if h != nil {
		return h
	}

	if u.Spray == nil {
		return nil
	}
	return u.Spray.Select(pool)
}

func (u *staticUpstream) IsAllowedDomain(name string) bool {
	if dns.Name(name) == dns.Name(u.From()) {
		return true
	}

	for _, ignoredSubDomain := range u.IgnoredSubDomains {
		if middleware.Name(ignoredSubDomain).Matches(name) {
			return false
		}
	}
	return true
}

func (u *staticUpstream) Exchanger() Exchanger { return u.ex }
