package checker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const dnsTimeout = 5 * time.Second

var icmpSeq atomic.Uint32

// Result holds the ping result for a single IP.
type Result struct {
	IP        string
	Latency   time.Duration
	Jitter    time.Duration
	LossRate  float64
	Attempts  int
	Successes int
	Score     float64
	LastErr   error
	Err       error
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
	filtered := 0
	var failures []string
	for range dnsServers {
		r := <-ch
		if r.err != nil {
			failures = append(failures, r.err.Error())
			continue
		}
		for _, ip := range r.ips {
			if normalized, ok := NormalizeCandidateIP(ip); ok {
				ipSet[normalized] = true
			} else {
				filtered++
			}
		}
	}

	if len(ipSet) == 0 {
		ips, err := resolveFromDoH(domain)
		if err != nil {
			failures = append(failures, "doh: "+err.Error())
		}
		for _, ip := range ips {
			ipSet[ip] = true
		}
	}

	if len(ipSet) == 0 {
		detail := strings.Join(failures, "; ")
		if detail == "" && filtered > 0 {
			detail = fmt.Sprintf("filtered %d non-public/reserved candidate(s)", filtered)
		}
		if detail == "" {
			detail = "no usable public IPv4 candidates"
		}
		return nil, fmt.Errorf("all DNS servers failed to resolve usable public IPv4 for %s: %s", domain, detail)
	}

	unique := make([]string, 0, len(ipSet))
	for ip := range ipSet {
		unique = append(unique, ip)
	}
	return unique, nil
}

func resolveFromDNS(domain, dnsServer string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()

	resolver := &net.Resolver{
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: dnsTimeout}
			return d.DialContext(ctx, "udp", dnsServer+":53")
		},
	}
	ips, err := resolver.LookupHost(ctx, domain)
	if err != nil {
		return nil, fmt.Errorf("dns %s: %w", dnsServer, err)
	}
	return ips, nil
}

func resolveFromDoH(domain string) ([]string, error) {
	endpoints := []string{
		"https://1.1.1.1/dns-query",
		"https://1.0.0.1/dns-query",
	}
	client := &http.Client{Timeout: dnsTimeout}
	var failures []string
	for _, endpoint := range endpoints {
		u, err := url.Parse(endpoint)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		q := u.Query()
		q.Set("name", domain)
		q.Set("type", "A")
		u.RawQuery = q.Encode()
		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		req.Header.Set("Accept", "application/dns-json")
		resp, err := client.Do(req)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		var payload struct {
			Status int `json:"Status"`
			Answer []struct {
				Type int    `json:"type"`
				Data string `json:"data"`
			} `json:"Answer"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			failures = append(failures, fmt.Sprintf("%s returned HTTP %d", endpoint, resp.StatusCode))
			continue
		}
		if decodeErr != nil {
			failures = append(failures, decodeErr.Error())
			continue
		}
		if payload.Status != 0 {
			failures = append(failures, fmt.Sprintf("%s returned DNS status %d", endpoint, payload.Status))
			continue
		}
		ips := make([]string, 0, len(payload.Answer))
		for _, answer := range payload.Answer {
			if answer.Type != 1 {
				continue
			}
			if ip, ok := NormalizeCandidateIP(answer.Data); ok {
				ips = append(ips, ip)
			}
		}
		if len(ips) > 0 {
			return dedupeSorted(ips), nil
		}
		failures = append(failures, endpoint+" returned no usable A records")
	}
	return nil, fmt.Errorf("%s", strings.Join(failures, "; "))
}

func dedupeSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := set[value]; ok {
			continue
		}
		set[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// pingICMP sends an ICMP echo request and measures round-trip time.
func pingICMP(ip string, timeout time.Duration) Result {
	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return Result{IP: ip, Err: fmt.Errorf("icmp listen: %w", err)}
	}
	defer conn.Close()

	dst, err := net.ResolveIPAddr("ip4", ip)
	if err != nil {
		return Result{IP: ip, Err: fmt.Errorf("resolve: %w", err)}
	}

	id := uint16(rand.Intn(65536))
	seq := uint16(icmpSeq.Add(1))
	pkt := buildICMPEchoRequest(id, seq)

	conn.SetDeadline(time.Now().Add(timeout))
	start := time.Now()

	if _, err := conn.WriteTo(pkt, dst); err != nil {
		return Result{IP: ip, Err: fmt.Errorf("send: %w", err)}
	}

	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return Result{IP: ip, Err: fmt.Errorf("recv: %w", err)}
		}
		// Only accept replies from our target
		if peerAddr, ok := peer.(*net.IPAddr); !ok || !peerAddr.IP.Equal(dst.IP) {
			continue
		}
		// ICMP header: type(1) code(1) checksum(2) id(2) seq(2)
		// Type 0 = Echo Reply
		if n >= 8 && buf[0] == 0 {
			replyID := binary.BigEndian.Uint16(buf[4:6])
			replySeq := binary.BigEndian.Uint16(buf[6:8])
			if replyID == id && replySeq == seq {
				return Result{IP: ip, Latency: time.Since(start)}
			}
		}
	}
}

// buildICMPEchoRequest builds an ICMP Echo Request packet.
func buildICMPEchoRequest(id, seq uint16) []byte {
	pkt := make([]byte, 8)
	pkt[0] = 8 // Echo Request
	pkt[1] = 0 // Code 0
	// Checksum at [2:4], computed below
	binary.BigEndian.PutUint16(pkt[4:6], id)
	binary.BigEndian.PutUint16(pkt[6:8], seq)

	// Compute ICMP checksum
	sum := uint32(0)
	for i := 0; i < len(pkt); i += 2 {
		sum += uint32(pkt[i])<<8 | uint32(pkt[i+1])
	}
	sum = (sum >> 16) + (sum & 0xffff)
	sum += sum >> 16
	binary.BigEndian.PutUint16(pkt[2:4], uint16(^sum))

	return pkt
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

func scoreResult(latencyMs, jitterMs, lossRate, latencyWeight, jitterWeight, lossWeight float64) float64 {
	return latencyMs*latencyWeight + jitterMs*jitterWeight + lossRate*lossWeight
}

func aggregateResults(ip string, attempts []Result, latencyWeight, jitterWeight, lossWeight float64) Result {
	out := Result{
		IP:       ip,
		Attempts: len(attempts),
	}

	successLatencies := make([]float64, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.Err != nil {
			out.LastErr = attempt.Err
			continue
		}
		out.Successes++
		successLatencies = append(successLatencies, float64(attempt.Latency.Microseconds())/1000.0)
	}

	if out.Attempts > 0 {
		out.LossRate = float64(out.Attempts-out.Successes) / float64(out.Attempts) * 100
	}

	if out.Successes == 0 {
		if out.LastErr == nil {
			out.LastErr = fmt.Errorf("all probes failed")
		}
		out.Err = out.LastErr
		return out
	}

	avgLatency := 0.0
	for _, ms := range successLatencies {
		avgLatency += ms
	}
	avgLatency /= float64(len(successLatencies))
	out.Latency = time.Duration(avgLatency * float64(time.Millisecond))

	if len(successLatencies) > 1 {
		jitterSum := 0.0
		for i := 1; i < len(successLatencies); i++ {
			jitterSum += math.Abs(successLatencies[i] - successLatencies[i-1])
		}
		jitterMs := jitterSum / float64(len(successLatencies)-1)
		out.Jitter = time.Duration(jitterMs * float64(time.Millisecond))
	}

	out.Score = scoreResult(avgLatency, float64(out.Jitter.Microseconds())/1000.0, out.LossRate, latencyWeight, jitterWeight, lossWeight)
	return out
}

// PingAll probes all given IPs concurrently and returns aggregated results sorted by score (best first).
// mode: "icmp" or "tcp". For TCP, port is used; for ICMP, port is ignored.
func PingAll(ips []string, mode string, port int, timeout time.Duration, attempts int, latencyWeight, jitterWeight, lossWeight float64) []Result {
	if len(ips) == 0 {
		return nil
	}
	if attempts < 1 {
		attempts = 1
	}

	results := make([]Result, len(ips))
	var wg sync.WaitGroup
	wg.Add(len(ips))

	for i, ip := range ips {
		go func(idx int, addr string) {
			defer wg.Done()
			probes := make([]Result, 0, attempts)
			for attempt := 0; attempt < attempts; attempt++ {
				switch mode {
				case "icmp":
					probes = append(probes, pingICMP(addr, timeout))
				default:
					probes = append(probes, PingTCP(addr, port, timeout))
				}
			}
			results[idx] = aggregateResults(addr, probes, latencyWeight, jitterWeight, lossWeight)
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
		if ri.Score != rj.Score {
			return ri.Score < rj.Score
		}
		if ri.LossRate != rj.LossRate {
			return ri.LossRate < rj.LossRate
		}
		return ri.Latency < rj.Latency
	})

	return results
}
