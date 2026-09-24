package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	cfProxyDomainsURL   = "https://raw.githubusercontent.com/Flowseal/tg-ws-proxy/main/.github/cfproxy-domains.txt"
	cfProxyRefreshEvery = 1 * time.Hour
	cfProxyMinDomains   = 3
	cfProxyMaxAge       = 120 * time.Second
	cfProxyPoolSize     = 4
	cfProxyCacheFile    = "/opt/zapret2/.cfproxy-domains.cache"
)

// Захардкоженные fallback домены (закодированные как в Flowseal)
var cfProxyEncodedDomains = []string{
	"virkgj.com", "vmmzovy.com", "mkuosckvso.com", "zaewayzmplad.com",
	"twdmbzcm.com", "awzwsldi.com", "clngqrflngqin.com", "tjacxbqtj.com",
	"bxaxtxmrw.com", "dmohrsgmohcrwb.com", "vwbmtmoi.com", "khgrre.com",
	"ulihssf.com", "tmhqsdqmfpmk.com", "xwuwoqbm.com", "orgcnunpj.com",
	"zhkuldz.com", "zypoljnslxa.com", "efabnxaowuzs.com", "zaftuzsftqdq.com",
}

type cfProxyManager struct {
	mu          sync.RWMutex
	domains     []string
	currentIdx  int
	lastRefresh time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	pinnedHTTP  *http.Client
}

func decodeCfProxyDomain(encoded string) string {
	if !strings.HasSuffix(encoded, ".com") {
		return encoded
	}

	prefix := encoded[:len(encoded)-4]
	shift := 0
	for _, c := range prefix {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			shift++
		}
	}

	decoded := make([]byte, len(prefix))
	for i, c := range prefix {
		if c >= 'a' && c <= 'z' {
			decoded[i] = byte((int(c-'a')-shift+26)%26) + 'a'
		} else if c >= 'A' && c <= 'Z' {
			decoded[i] = byte((int(c-'A')-shift+26)%26) + 'A'
		} else {
			decoded[i] = byte(c)
		}
	}

	return string(decoded) + ".co.uk"
}

func newCfProxyManager() *cfProxyManager {
	ctx, cancel := context.WithCancel(context.Background())

	defaultDomains := make([]string, len(cfProxyEncodedDomains))
	for i, enc := range cfProxyEncodedDomains {
		defaultDomains[i] = decodeCfProxyDomain(enc)
	}

	mgr := &cfProxyManager{
		domains:     defaultDomains,
		ctx:         ctx,
		cancel:      cancel,
		lastRefresh: time.Now(),
		pinnedHTTP:  createPinnedHTTPClient(),
	}

	// Пробуем загрузить из кэша
	mgr.loadCache()

	log.Printf("[cfproxy] initialized with %d domains", mgr.count())
	return mgr
}

func createPinnedHTTPClient() *http.Client {
	githubIP := "185.199.109.133"

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.Contains(addr, "githubusercontent.com") || strings.Contains(addr, "github.com") {
				host, port, _ := net.SplitHostPort(addr)
				if port == "" {
					port = "443"
				}
				_ = host // suppress unused warning
				return dialer.DialContext(ctx, "tcp", net.JoinHostPort(githubIP, port))
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}
}

func (mgr *cfProxyManager) refresh() {
	req, err := http.NewRequest("GET", cfProxyDomainsURL, nil)
	if err != nil {
		log.Printf("[cfproxy] failed to create request: %v", err)
		return
	}

	req.URL.RawQuery = fmt.Sprintf("v=%d", time.Now().Unix())
	req.Header.Set("User-Agent", "tg-ws-proxy")

	resp, err := mgr.pinnedHTTP.Do(req)
	if err != nil {
		log.Printf("[cfproxy] failed to fetch domains: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[cfproxy] GitHub returned %d", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[cfproxy] failed to read response: %v", err)
		return
	}

	var fetched []string
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			decoded := decodeCfProxyDomain(line)
			if isValidDomain(decoded) {
				fetched = append(fetched, decoded)
			}
		}
	}

	if len(fetched) < cfProxyMinDomains {
		log.Printf("[cfproxy] fetched only %d domains (need >= %d), keeping current pool",
			len(fetched), cfProxyMinDomains)
		return
	}

	mgr.mu.Lock()
	mgr.domains = fetched
	mgr.currentIdx = 0
	mgr.lastRefresh = time.Now()
	mgr.mu.Unlock()

	mgr.saveCache()
	log.Printf("[cfproxy] updated domain pool: %d domains", len(fetched))
}

func (mgr *cfProxyManager) refreshLoop() {
	ticker := time.NewTicker(cfProxyRefreshEvery)
	defer ticker.Stop()

	mgr.refresh()

	for {
		select {
		case <-mgr.ctx.Done():
			return
		case <-ticker.C:
			mgr.refresh()
		}
	}
}

func (mgr *cfProxyManager) nextDomain() string {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	if len(mgr.domains) == 0 {
		return ""
	}

	domain := mgr.domains[mgr.currentIdx%len(mgr.domains)]
	mgr.currentIdx++
	return domain
}

func (mgr *cfProxyManager) getRandomDomain() string {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	if len(mgr.domains) == 0 {
		return ""
	}

	return mgr.domains[rand.Intn(len(mgr.domains))]
}

func (mgr *cfProxyManager) count() int {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	return len(mgr.domains)
}

func (mgr *cfProxyManager) saveCache() {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()

	f, err := os.Create(cfProxyCacheFile)
	if err != nil {
		return
	}
	defer f.Close()

	for _, d := range mgr.domains {
		fmt.Fprintln(f, d)
	}
}

func (mgr *cfProxyManager) loadCache() {
	f, err := os.Open(cfProxyCacheFile)
	if err != nil {
		return
	}
	defer f.Close()

	var domains []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && isValidDomain(line) {
			domains = append(domains, line)
		}
	}

	if len(domains) >= cfProxyMinDomains {
		mgr.mu.Lock()
		mgr.domains = domains
		mgr.mu.Unlock()
		log.Printf("[cfproxy] loaded %d domains from cache", len(domains))
	}
}

func isValidDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}

	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}

	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	hasLetter := false
	for _, c := range tld {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			hasLetter = true
			break
		}
	}
	return hasLetter
}
