package network

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/taskcluster/taskcluster-worker/runtime/atomics"

	"gopkg.in/tylerb/graceful.v1"
)

const metaDataIP = "169.254.169.254"

var remoteAddrPattern = regexp.MustCompile("^(192\\.168\\.\\d{1,3})\\.\\d{1,3}:\\d{1,5}$")

// Pool manages a static set of networks (TAP devices).
type Pool struct {
	m           sync.Mutex
	networks    map[string]*entry // mapping from ip-prefix to entry
	server      *graceful.Server
	serverDone  <-chan struct{} // closed when server is stopped
	dnsmasq     *exec.Cmd
	dnsmasqKill atomics.Bool
	dnsmasqDone <-chan struct{} // closed when dnsmasq is terminated
}

// entry is a strictly internal presentation of a TAP device network.
type entry struct {
	tapDevice string
	ipPrefix  string // 192.168.xxx (subnet without the last ".0")
	m         sync.RWMutex
	handler   http.Handler
	pool      *Pool
	inUse     bool
}

// NewPool creates N virtual networks and returns Pool.
// This should be called before the worker starts operating, we don't wish to
// dynamically reconfigure networks at runtime.
func NewPool(N int) *Pool {
	p := &Pool{
		networks: make(map[string]*entry),
	}

	// Maybe we could split the address space further someday in the future
	if N > 100 {
		panic("Can't create more than 100 networks")
	}

	// Create a number of networks
	for i := 0; i < N; i++ {
		// Construct the network object
		n := createNetwork(i, p)
		p.networks[n.ipPrefix] = n
	}

	// Enable IPv4 forwarding
	err := script([][]string{
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
	})
	if err != nil {
		panic(fmt.Sprint("Failed to enable ipv4 forwarding: ", err))
	}

	// Create dnsmasq configuration
	dnsmasqConfig := `
    strict-order
    bind-interfaces
    except-interface=lo
    conf-file=""
    dhcp-no-override
    host-record=taskcluster,` + metaDataIP + `
    keep-in-foreground
    bogus-priv
    domain-needed` // Consider adding "no-ping"
	for _, n := range p.networks {
		dnsmasqConfig += `
      interface=` + n.tapDevice + `
      dhcp-range=tag:` + n.tapDevice + `,` + n.ipPrefix + `.2,` + n.ipPrefix + `.254,255.255.255.0,20m
      dhcp-option=tag:` + n.tapDevice + `,option:router,` + n.ipPrefix + `.1`
	}

	// Start dnsmasq
	dnsmasqDone := make(chan struct{})
	p.dnsmasqDone = dnsmasqDone
	p.dnsmasq = exec.Command("dnsmasq", "--conf-file=-")
	p.dnsmasq.Stdin = bytes.NewBufferString(dnsmasqConfig)
	p.dnsmasq.Stderr = nil
	p.dnsmasq.Stdout = nil
	err = p.dnsmasq.Start()
	if err != nil {
		panic(fmt.Sprint("Failed to start dnsmasq, error: ", err))
	}
	// Monitor dnsmasq and panic if it crashes unexpectedly
	go (func(p *Pool, done chan<- struct{}) {
		err := p.dnsmasq.Wait()
		close(done)
		// Ignore errors if dnsmasqKill is true, otherwise this is a fatal issue
		if err != nil && !p.dnsmasqKill.Get() {
			// We could probably restart the dnsmasq, as long as we avoid an infinite
			// loop that should be fine. But dnsmasq probably won't crash without a
			// good reason.
			panic(fmt.Sprint("Fatal: dnsmasq died unexpectedly, error: ", err))
		}
	})(p, dnsmasqDone)

	// Add meta-data IP to loopback device
	err = script([][]string{
		{"ip", "addr", "add", metaDataIP, "dev", "lo"},
	})
	if err != nil {
		panic(fmt.Sprint("Failed to add: ", metaDataIP, " to the loopback device: ", err))
	}

	// Create the server
	serverDone := make(chan struct{})
	p.serverDone = serverDone
	p.server = &graceful.Server{
		Timeout: 30 * time.Second,
		Server: &http.Server{
			Addr:    metaDataIP + ":80",
			Handler: http.HandlerFunc(p.dispatchRequest),
		},
		NoSignalHandling: true,
	}

	// Start listening (we handle listener error as a special thing)
	listener, err := net.Listen("tcp", p.server.Addr)
	if err != nil {
		// If this happens ensure that we have configured the loopback device with:
		// sudo ip addr add 169.254.169.254/24 scope link dev lo
		panic(fmt.Sprint("Failed to listen on ", p.server.Addr, " error: ", err))
	}

	// Start the server
	go (func(p *Pool, done chan<- struct{}) {
		err := p.server.Serve(listener)
		close(done)
		if err != nil {
			// TODO: We could communicate this to all sandboxes and shut them down
			// gracefully. But I honestly doubt this will ever happen, why should it?
			panic(fmt.Sprint("Fatal: meta-data service listener failed, error: ", err))
		}
	})(p, serverDone)

	return p
}

func (p *Pool) dispatchRequest(w http.ResponseWriter, r *http.Request) {
	// Match remote address to find ipPrefix
	match := remoteAddrPattern.FindStringSubmatch(r.RemoteAddr)
	if len(match) != 2 {
		w.WriteHeader(403)
		return
	}
	ipPrefix := match[1]

	// Find network from the ipPrefix
	n := p.networks[ipPrefix]
	if n == nil {
		w.WriteHeader(403)
		return
	}

	// Read lock the network, so the handler can't be cleared while we do this
	n.m.RLock()
	handler := n.handler
	n.m.RUnlock()

	// Call handler
	if handler != nil {
		handler.ServeHTTP(w, r)
	} else {
		w.WriteHeader(404)
	}
}

// Network is provides the interface for using a TAP device, and releasing it.
type Network struct {
	m     sync.Mutex
	entry *entry
}

// SetHandler sets the http.handler for meta-data service for this tap-device.
func (n *Network) SetHandler(handler http.Handler) {
	n.m.Lock()
	defer n.m.Unlock()
	n.entry.m.Lock()
	defer n.entry.m.Unlock()
	n.entry.handler = handler
}

// TapDevice returns the name of the tap-device for this network.
func (n *Network) TapDevice() string {
	n.m.Lock()
	defer n.m.Unlock()
	return n.entry.tapDevice
}

// Release returns this network to the Pool, this cannot be called twice.
func (n *Network) Release() {
	// Lock the wrapper
	n.m.Lock()
	defer n.m.Unlock()

	// Prevent multiple Release calls
	if n.entry == nil {
		panic("Cannot release a network more than once!")
	}
	n.entry = nil

	// Lock entry and clear the handler
	n.entry.m.Lock()
	n.entry.handler = nil
	n.entry.m.Unlock()

	// Set entry as idle
	n.entry.pool.m.Lock()
	n.entry.inUse = false
	n.entry.pool.m.Unlock()
}

// Network returns an unused network, or nil if no network is available.
func (p *Pool) Network() *Network {
	p.m.Lock()
	defer p.m.Unlock()
	if p.networks == nil {
		panic("Pool.networks is nil, implying that the pool hsa been destroyed")
	}

	for _, entry := range p.networks {
		if !entry.inUse {
			entry.handler = nil
			entry.inUse = true
			if entry.tapDevice == "" {
				panic("entry.tapDevice is empty, implying the network has been destroyed")
			}
			return &Network{entry: entry}
		}
	}

	return nil
}

// Dispose deletes all the networks created, should not be called while any of
// networks are in use.
func (p *Pool) Dispose() error {
	if p.networks == nil {
		panic("networkPool.Dispose() cannot be called while a network is in use")
	}

	// Gracefully stop the server
	p.server.Stop(30 * time.Second)
	<-p.serverDone

	// Kills dnsmasq
	p.dnsmasqKill.Set(true) // indicate that error exit is expected
	p.dnsmasq.Process.Kill()
	<-p.dnsmasqDone

	// Delete all the networks
	errs := []string{}
	for _, network := range p.networks {
		err := destroyNetwork(network)
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	p.networks = nil
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}

	// Remove meta-data IP from loopback device
	err := script([][]string{
		{"ip", "addr", "del", metaDataIP, "dev", "lo"},
	})

	return err
}

// createNetwork creates a tap device and related ip-tables configuration.
// This does not start dnsmasq, use newNetworkPool() to create a set of
// networks with dnsmasq running.
func createNetwork(index int, parent *Pool) *entry {
	// Each network has a name and an ip-prefix, we use the 192.168.0.0/16
	// subnet starting from 192.168.150.0
	tapDevice := "tctap" + strconv.Itoa(index)
	ipPrefix := "192.168." + strconv.Itoa(index+150)

	err := script([][]string{
		// Create tap device
		{"ip", "tuntap", "add", "dev", tapDevice, "mode", "tap"},
		// Assign IP-address to tap device
		{"ip", "addr", "add", ipPrefix + ".1", "dev", tapDevice},
		// Activate the link
		{"ip", "link", "set", "dev", tapDevice, "up"},
		// Add route for the network subnet, routing it to the tap device
		{"ip", "route", "add", ipPrefix + ".0/24", "dev", tapDevice},
	})
	if err != nil {
		panic(fmt.Sprint("Failed to setup tap device: ", tapDevice, ", error: ", err))
	}

	// Create iptables rules and chains
	err = script(ipTableRules(tapDevice, ipPrefix, false))
	if err != nil {
		panic(fmt.Sprint("Failed to setup ip-tables for tap device: ", tapDevice, ", error: ", err))
	}

	// Construct the network object
	return &entry{
		tapDevice: tapDevice,
		ipPrefix:  ipPrefix,
		handler:   nil,
		pool:      parent,
	}
}

// destroy deletes the networks tap device and related ip-tables configuration.
func destroyNetwork(n *entry) error {
	n.m.Lock()
	defer n.m.Unlock()
	if n.tapDevice == "" {
		return errors.New("network.tapDevice is empty, implying the network has been destroyed")
	}

	// Delete iptables rules and chains
	err := script(ipTableRules(n.tapDevice, n.ipPrefix, true))
	if err != nil {
		return fmt.Errorf("Failed to remove ip-tables for tap device: %s, error: %s", n.tapDevice, err)
	}

	err = script([][]string{
		// Remove route for the network subnet
		{"ip", "route", "del", n.ipPrefix + ".0/24", "dev", n.tapDevice},
		// Deactivate the link
		{"ip", "link", "set", "dev", n.tapDevice, "down"},
		// Unassign IP-address from tap device
		{"ip", "addr", "del", n.ipPrefix + ".1", "dev", n.tapDevice},
		// Delete tap device
		{"ip", "tuntap", "del", "dev", n.tapDevice, "mode", "tap"},
	})
	if err != nil {
		return fmt.Errorf("Failed to remove tap device: %s, error: %s", n.tapDevice, err)
	}

	// Clear handler and tapDevice
	n.handler = nil
	n.tapDevice = ""

	return nil
}
