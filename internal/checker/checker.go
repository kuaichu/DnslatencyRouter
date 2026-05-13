package checker

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

const dnsTimeout = 5 * time.Second

// Result holds the ping result for a single IP.
type Result struct {
	IP      string
	Latency time.Duration
	Err     error
}

// ResolveFromAllDNS resolves a domain from multiple DNS servers and returns unique IPs.
// This helps discover IPs that different ISPs might route to (geo-DNS / CDN).
func ResolveFromAllDNS(domain string, dnsServers []string) ([]string, error) {
	type dnsResult struct {
		ips []string
		err error
	}

	ch := make(chan dnsResult, len(dnsServers))
	for _, dns := range dnsServers {
		go func(srv string) {
			ips, err := resolveFromDNS(domain, srv)
			ch <- dnsResult{ips, err}
		}(dns)
	}

	ipSet := make(map[string]bool)
	for range dnsServers {
		r := <-ch
		if r.err != nil {
			continue
		}
		for _, ip := range r.ips {
			ipSet[ip] = true
		}
	}

	if len(ipSet) == 0 {
		return nil, fmt.Errorf("all DNS servers failed to resolve %s", domain)
	}

	unique := make([]string, 0, len(ipSet))
	for ip := range ipSet {
		unique = append(unique, ip)
	}
	return unique, nil
}

func resolveFromDNS(domain, dnsServer string) ([]string, error) {
	resolver := &net.Resolver{
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: dnsTimeout}
			return d.DialContext(ctx, "udp", dnsServer+":53")
		},
	}
	ips, err := resolver.LookupHost(context.Background(), domain)
	if err != nil {
		return nil, fmt.Errorf("dns %s: %w", dnsServer, err)
	}
	return ips, nil
}

// PingTCP measures TCP connect latency to ip:port within the given timeout.
func PingTCP(ip string, port int, timeout time.Duration) Result {
	start := time.Now()
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return Result{IP: ip, Err: fmt.Errorf("dial %s: %w", addr, err)}
	}
	conn.Close()
	return Result{
		IP:      ip,
		Latency: time.Since(start),
	}
}

// PingAll pings all given IPs concurrently and returns results sorted by latency (fastest first).
// IPs that fail to respond within timeout are placed at the end.
func PingAll(ips []string, port int, timeout time.Duration) []Result {
	if len(ips) == 0 {
		return nil
	}

	results := make([]Result, len(ips))
	var wg sync.WaitGroup
	wg.Add(len(ips))

	for i, ip := range ips {
		go func(idx int, addr string) {
			defer wg.Done()
			results[idx] = PingTCP(addr, port, timeout)
		}(i, ip)
	}

	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		ri, rj := results[i], results[j]
		if ri.Err != nil && rj.Err != nil {
			return ri.IP < rj.IP
		}
		if ri.Err != nil {
			return false
		}
		if rj.Err != nil {
			return true
		}
		return ri.Latency < rj.Latency
	})

	return results
}
