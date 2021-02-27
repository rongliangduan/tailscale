// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package router

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/tailscale/wireguard-go/device"
	"github.com/tailscale/wireguard-go/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
	"tailscale.com/logtail/backoff"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/router/dns"
	"tailscale.com/wgengine/router/firewall"
)

type winRouter struct {
	logf                func(fmt string, args ...interface{})
	tunname             string
	nativeTun           *tun.NativeTun
	wgdev               *device.Device
	routeChangeCallback *winipcfg.RouteChangeCallback
	dns                 *dns.Manager
	firewall            *firewallTweaker
	wgFirewallEnabled   bool
}

func newUserspaceRouter(logf logger.Logf, wgdev *device.Device, tundev tun.Device) (Router, error) {
	tunname, err := tundev.Name()
	if err != nil {
		return nil, err
	}

	nativeTun := tundev.(*tun.NativeTun)
	luid := winipcfg.LUID(nativeTun.LUID())
	guid, err := luid.GUID()
	if err != nil {
		return nil, err
	}

	mconfig := dns.ManagerConfig{
		Logf:          logf,
		InterfaceName: guid.String(),
	}

	return &winRouter{
		logf:      logf,
		wgdev:     wgdev,
		tunname:   tunname,
		nativeTun: nativeTun,
		dns:       dns.NewManager(mconfig),
		firewall:  &firewallTweaker{logf: logger.WithPrefix(logf, "firewall: ")},
	}, nil
}

func (r *winRouter) Up() error {
	r.firewall.clear()

	var err error
	t0 := time.Now()
	r.routeChangeCallback, err = monitorDefaultRoutes(r.nativeTun)
	d := time.Since(t0).Round(time.Millisecond)
	if err != nil {
		return fmt.Errorf("monitorDefaultRoutes, after %v: %v", d, err)
	}
	r.logf("monitorDefaultRoutes done after %v", d)
	return nil
}

func (r *winRouter) Set(cfg *Config) error {
	if cfg == nil {
		cfg = &shutdownConfig
	}

	var localAddrs []string
	for _, la := range cfg.LocalAddrs {
		localAddrs = append(localAddrs, la.String())
	}
	r.firewall.set(localAddrs)

	err := configureInterface(cfg, r.nativeTun)
	if err != nil {
		r.logf("ConfigureInterface: %v", err)
		return err
	}

	if err := r.dns.Set(cfg.DNS); err != nil {
		return fmt.Errorf("dns set: %w", err)
	}

	// If default routes are being sent via Tailscale, we have to
	// configure the Windows firewall to block outbound traffic
	// through the "old" default route. Windows makes connections
	// "sticky" to the default route and egress interface that was
	// active when the connection was first established. So, if (say)
	// Chrome has cached sockets that predate the Tailscale default
	// route becoming active, those sockets will continue to use the
	// older default route, bypassing Tailscale.
	//
	// To remedy this, when Tailscale has programmed a default route,
	// we install firewall rules that block everything except
	// Tailscale and a few core network services (DHCP, NDP, ARP) from
	// using anything other than the Tailscale interface to dial
	// out. This results in existing connections getting a prompt
	// connection reset, and when they redial they'll use Tailscale as
	// their default route.
	//
	// Privacy VPN services call this their "internet killswitch", and
	// make a big deal of it from a privacy perspective. But it turns
	// out that it's also a technical requirement to make default
	// route switchover actually work!
	hasdefault := false
	for _, r := range cfg.Routes {
		if r.Bits == 0 {
			hasdefault = true
			break
		}
	}
	if hasdefault && !r.wgFirewallEnabled {
		r.logf("enabling default route killswitch")
		if err := firewall.EnableFirewall(r.nativeTun.LUID(), false, nil); err != nil {
			r.logf("enabling default routing firewall failed: %v", err)
		}
		r.wgFirewallEnabled = true
	} else if !hasdefault && r.wgFirewallEnabled {
		r.logf("disabling default route killswitch")
		firewall.DisableFirewall()
		r.wgFirewallEnabled = false
	}

	return nil
}

func (r *winRouter) Close() error {
	r.firewall.clear()

	if err := r.dns.Down(); err != nil {
		return fmt.Errorf("dns down: %w", err)
	}
	if r.routeChangeCallback != nil {
		r.routeChangeCallback.Unregister()
	}

	return nil
}

func cleanup(logf logger.Logf, interfaceName string) {
	// Nothing to do here.
}

// firewallTweaker changes the Windows firewall. Normally this wouldn't be so complicated,
// but it can be REALLY SLOW to change the Windows firewall for reasons not understood.
// Like 4 minutes slow. But usually it's tens of milliseconds.
// See https://github.com/tailscale/tailscale/issues/785.
// So this tracks the desired state and runs the actual adjusting code asynchrounsly.
type firewallTweaker struct {
	logf logger.Logf

	mu          sync.Mutex
	didProcRule bool
	running     bool     // doAsyncSet goroutine is running
	known       bool     // firewall is in known state (in lastVal)
	want        []string // next value we want, or "" to delete the firewall rule
	lastVal     []string // last set value, if known
}

func (ft *firewallTweaker) clear() { ft.set(nil) }

// set takes the IPv4 and/or IPv6 CIDRs to allow; an empty slice
// removes the firwall rules.
//
// set takes ownership of the slice.
func (ft *firewallTweaker) set(cidrs []string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	if len(cidrs) == 0 {
		ft.logf("marking for removal")
	} else {
		ft.logf("marking allowed %v", cidrs)
	}
	ft.want = cidrs
	if ft.running {
		// The doAsyncSet goroutine will check ft.want
		// before returning.
		return
	}
	ft.logf("starting netsh goroutine")
	ft.running = true
	go ft.doAsyncSet()
}

func (ft *firewallTweaker) runFirewall(args ...string) (time.Duration, error) {
	t0 := time.Now()
	args = append([]string{"advfirewall", "firewall"}, args...)
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err := cmd.Run()
	return time.Since(t0).Round(time.Millisecond), err
}

func (ft *firewallTweaker) doAsyncSet() {
	bo := backoff.NewBackoff("win-firewall", ft.logf, time.Minute)
	ctx := context.Background()

	ft.mu.Lock()
	for { // invariant: ft.mu must be locked when beginning this block
		val := ft.want
		if ft.known && strsEqual(ft.lastVal, val) {
			ft.running = false
			ft.logf("ending netsh goroutine")
			ft.mu.Unlock()
			return
		}
		needClear := !ft.known || len(ft.lastVal) > 0 || len(val) == 0
		needProcRule := !ft.didProcRule
		ft.mu.Unlock()

		if needClear {
			ft.logf("clearing Tailscale-In firewall rules...")
			// We ignore the error here, because netsh returns an error for
			// deleting something that doesn't match.
			// TODO(bradfitz): care? That'd involve querying it before/after to see
			// whether it was necessary/worked. But the output format is localized,
			// so can't rely on parsing English. Maybe need to use OLE, not netsh.exe?
			d, _ := ft.runFirewall("delete", "rule", "name=Tailscale-In", "dir=in")
			ft.logf("cleared Tailscale-In firewall rules in %v", d)
		}
		if needProcRule {
			ft.logf("deleting any prior Tailscale-Process rule...")
			d, err := ft.runFirewall("delete", "rule", "name=Tailscale-Process", "dir=in") // best effort
			if err == nil {
				ft.logf("removed old Tailscale-Process rule in %v", d)
			}
			var exe string
			exe, err = os.Executable()
			if err != nil {
				ft.logf("failed to find Executable for Tailscale-Process rule: %v", err)
			} else {
				ft.logf("adding Tailscale-Process rule to allow UDP for %q ...", exe)
				d, err = ft.runFirewall("add", "rule", "name=Tailscale-Process",
					"dir=in",
					"action=allow",
					"edge=yes",
					"program="+exe,
					"protocol=udp",
					"profile=any",
					"enable=yes",
				)
				if err != nil {
					ft.logf("error adding Tailscale-Process rule: %v", err)
				} else {
					ft.mu.Lock()
					ft.didProcRule = true
					ft.mu.Unlock()
					ft.logf("added Tailscale-Process rule in %v", d)
				}
			}
		}
		var err error
		for _, cidr := range val {
			ft.logf("adding Tailscale-In rule to allow %v ...", cidr)
			var d time.Duration
			d, err = ft.runFirewall("add", "rule", "name=Tailscale-In", "dir=in", "action=allow", "localip="+cidr, "profile=private", "enable=yes")
			if err != nil {
				ft.logf("error adding Tailscale-In rule to allow %v: %v", cidr, err)
				break
			}
			ft.logf("added Tailscale-In rule to allow %v in %v", cidr, d)
		}
		bo.BackOff(ctx, err)

		ft.mu.Lock()
		ft.lastVal = val
		ft.known = (err == nil)
	}
}

func strsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
