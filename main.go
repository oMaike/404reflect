package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

const (
	defaultUserAgent = "404reflect/1.0"
	defaultBodyLimit = 2 * 1024 * 1024
)

type config struct {
	target          *url.URL
	maxDepth        int
	maxURLs         int
	rate            float64
	userAgent       string
	timeout         time.Duration
	bodyLimit       int64
	followRedirects bool
	insecureTLS     bool
	probeDirs       bool
	wordlistPath    string
	jsonOutput      bool
	verbose         bool
}

type task struct {
	u      *url.URL
	parent string
	depth  int
	reason string
}

type responseData struct {
	requestedURL string
	finalURL     string
	status       int
	contentType  string
	body         []byte
}

type finding struct {
	URL              string   `json:"url"`
	Status           int      `json:"status"`
	ReflectedVariant string   `json:"reflected_variant"`
	Source           string   `json:"source"`
	Route            []string `json:"route"`
}

type scanner struct {
	cfg       config
	client    *http.Client
	limiter   *rateLimiter
	queue     []task
	seen      map[string]bool
	parents   map[string]string
	reasons   map[string]string
	dirSource map[string]string
	dirs      map[string]bool
	findings  int
	requests  int
}

type rateLimiter struct {
	interval time.Duration
	next     time.Time
}

func newRateLimiter(rps float64) *rateLimiter {
	if rps <= 0 {
		return &rateLimiter{}
	}
	return &rateLimiter{interval: time.Duration(float64(time.Second) / rps)}
}

func (r *rateLimiter) wait(ctx context.Context) error {
	if r.interval <= 0 {
		return nil
	}

	now := time.Now()
	if r.next.IsZero() || now.After(r.next) {
		r.next = now
	}

	delay := time.Until(r.next)
	r.next = r.next.Add(r.interval)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func main() {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n\n", err)
		usage()
		os.Exit(2)
	}

	s := newScanner(cfg)
	ctx := context.Background()

	fmt.Fprintf(os.Stderr, "[*] target: %s\n", cfg.target.String())
	fmt.Fprintf(os.Stderr, "[*] depth=%d max-urls=%d rate=%.2f req/s user-agent=%q\n", cfg.maxDepth, cfg.maxURLs, cfg.rate, cfg.userAgent)

	if err := s.run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "[*] finalizado: %d requests, %d diretorios, %d achados\n", s.requests, len(s.dirs), s.findings)
}

func parseFlags() (config, error) {
	var rawTarget string
	var timeoutRaw string
	var bodyLimitMB int

	cfg := config{}
	flag.StringVar(&rawTarget, "target", "", "URL inicial do alvo, ex: https://example.com")
	flag.IntVar(&cfg.maxDepth, "depth", 2, "profundidade maxima do crawler")
	flag.IntVar(&cfg.maxURLs, "max-urls", 1000, "maximo de URLs para visitar no crawler")
	flag.Float64Var(&cfg.rate, "rate", 2, "limite global de requests por segundo; use 0 para desativar")
	flag.StringVar(&cfg.userAgent, "user-agent", defaultUserAgent, "User-Agent usado nas requisicoes")
	flag.StringVar(&timeoutRaw, "timeout", "10s", "timeout por request, ex: 5s, 30s")
	flag.IntVar(&bodyLimitMB, "body-limit-mb", 2, "maximo de MB lidos por resposta")
	flag.BoolVar(&cfg.followRedirects, "follow-redirects", true, "seguir redirects dentro da mesma origem")
	flag.BoolVar(&cfg.insecureTLS, "insecure", false, "ignorar erros TLS")
	flag.BoolVar(&cfg.probeDirs, "probe-dirs", true, "sondar um caminho 404 aleatorio em cada diretorio descoberto")
	flag.StringVar(&cfg.wordlistPath, "wordlist", "", "wordlist opcional para testar nomes em cada diretorio descoberto")
	flag.BoolVar(&cfg.jsonOutput, "json", false, "emitir achados em JSONL")
	flag.BoolVar(&cfg.verbose, "v", false, "mostrar progresso no stderr")
	flag.Usage = usage
	flag.Parse()

	if strings.TrimSpace(rawTarget) == "" {
		return cfg, errors.New("informe -target")
	}

	target, err := parseTarget(rawTarget)
	if err != nil {
		return cfg, err
	}

	timeout, err := time.ParseDuration(timeoutRaw)
	if err != nil || timeout <= 0 {
		return cfg, errors.New("-timeout precisa ser uma duracao valida maior que zero")
	}

	if cfg.maxDepth < 0 {
		return cfg, errors.New("-depth nao pode ser negativo")
	}
	if cfg.maxURLs <= 0 {
		return cfg, errors.New("-max-urls precisa ser maior que zero")
	}
	if cfg.rate < 0 {
		return cfg, errors.New("-rate nao pode ser negativo")
	}
	if bodyLimitMB <= 0 {
		return cfg, errors.New("-body-limit-mb precisa ser maior que zero")
	}
	if strings.TrimSpace(cfg.userAgent) == "" {
		cfg.userAgent = defaultUserAgent
	}

	cfg.target = target
	cfg.timeout = timeout
	cfg.bodyLimit = int64(bodyLimitMB) * 1024 * 1024

	return cfg, nil
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), `404reflect - encontra paginas 404 que refletem a propria URL no HTML.

Uso:
  404reflect -target https://example.com -depth 3 -rate 1 -user-agent "Mozilla/5.0"

Flags:
`)
	flag.PrintDefaults()
}

func parseTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("target precisa usar http ou https")
	}
	if u.Host == "" {
		return nil, errors.New("target sem host")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func newScanner(cfg config) *scanner {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.insecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	client := &http.Client{
		Timeout:   cfg.timeout,
		Transport: transport,
	}

	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !cfg.followRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) >= 5 {
			return errors.New("redirect limit reached")
		}
		if !sameOrigin(cfg.target, req.URL) {
			return http.ErrUseLastResponse
		}
		req.Header.Set("User-Agent", cfg.userAgent)
		return nil
	}

	return &scanner{
		cfg:       cfg,
		client:    client,
		limiter:   newRateLimiter(cfg.rate),
		seen:      make(map[string]bool),
		parents:   make(map[string]string),
		reasons:   make(map[string]string),
		dirSource: make(map[string]string),
		dirs:      make(map[string]bool),
	}
}

func (s *scanner) run(ctx context.Context) error {
	s.enqueue(s.cfg.target, "", 0, "seed")
	s.addDirectoriesFromURL(s.cfg.target, canonicalURL(s.cfg.target))

	for len(s.queue) > 0 && len(s.seen) <= s.cfg.maxURLs {
		current := s.queue[0]
		s.queue = s.queue[1:]

		if s.cfg.verbose {
			fmt.Fprintf(os.Stderr, "[crawl] %s\n", current.u.String())
		}

		res, err := s.fetch(ctx, current.u)
		if err != nil {
			if s.cfg.verbose {
				fmt.Fprintf(os.Stderr, "[erro] %s: %v\n", current.u.String(), err)
			}
			continue
		}

		s.inspectResponse(res, "crawl")

		if current.depth >= s.cfg.maxDepth {
			continue
		}
		if res.status >= 400 || !isHTML(res.contentType, res.body) {
			continue
		}

		finalBase, err := url.Parse(res.finalURL)
		if err != nil {
			continue
		}

		links := extractLinks(res.finalURL, res.body)
		for _, link := range links {
			normalized, ok := normalizeURL(finalBase, link)
			if !ok || !sameOrigin(s.cfg.target, normalized) {
				continue
			}
			s.addDirectoriesFromURL(normalized, canonicalURL(current.u))
			s.enqueue(normalized, canonicalURL(current.u), current.depth+1, "link")
		}
	}

	if s.cfg.probeDirs {
		if err := s.probeDirectories(ctx); err != nil {
			return err
		}
	}

	if s.cfg.wordlistPath != "" {
		if err := s.runWordlist(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (s *scanner) enqueue(u *url.URL, parent string, depth int, reason string) {
	key := canonicalURL(u)
	if s.seen[key] {
		return
	}
	if len(s.seen) >= s.cfg.maxURLs {
		return
	}
	s.seen[key] = true
	s.parents[key] = parent
	s.reasons[key] = reason
	s.queue = append(s.queue, task{u: cloneURL(u), parent: parent, depth: depth, reason: reason})
}

func (s *scanner) fetch(ctx context.Context, u *url.URL) (responseData, error) {
	if err := s.limiter.wait(ctx); err != nil {
		return responseData{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return responseData{}, err
	}
	req.Header.Set("User-Agent", s.cfg.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.7")

	resp, err := s.client.Do(req)
	if err != nil {
		return responseData{}, err
	}
	defer resp.Body.Close()
	s.requests++

	body, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.bodyLimit))
	if err != nil {
		return responseData{}, err
	}

	finalURL := u.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	return responseData{
		requestedURL: u.String(),
		finalURL:     finalURL,
		status:       resp.StatusCode,
		contentType:  resp.Header.Get("Content-Type"),
		body:         body,
	}, nil
}

func (s *scanner) inspectResponse(res responseData, source string) {
	if res.status != http.StatusNotFound {
		return
	}

	variants := reflectionVariants(res.requestedURL, res.finalURL)
	body := string(res.body)
	for _, variant := range variants {
		if variant.value != "" && strings.Contains(body, variant.value) {
			key := canonicalURLString(res.requestedURL)
			f := finding{
				URL:              res.requestedURL,
				Status:           res.status,
				ReflectedVariant: variant.name,
				Source:           source,
				Route:            s.routeTo(key),
			}
			s.printFinding(f)
			return
		}
	}
}

func (s *scanner) probeDirectories(ctx context.Context) error {
	dirs := make([]string, 0, len(s.dirs))
	for dir := range s.dirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		token, err := randomToken(8)
		if err != nil {
			return err
		}

		probeURL, err := appendPath(dir, "__404reflect_"+token)
		if err != nil {
			continue
		}
		key := canonicalURLString(probeURL)
		s.parents[key] = s.dirSource[dir]
		s.reasons[key] = "probe-dir"

		if s.cfg.verbose {
			fmt.Fprintf(os.Stderr, "[probe] %s\n", probeURL)
		}

		u, err := url.Parse(probeURL)
		if err != nil {
			continue
		}

		res, err := s.fetch(ctx, u)
		if err != nil {
			if s.cfg.verbose {
				fmt.Fprintf(os.Stderr, "[erro] %s: %v\n", probeURL, err)
			}
			continue
		}
		s.inspectResponse(res, "probe-dir")
	}
	return nil
}

func (s *scanner) runWordlist(ctx context.Context) error {
	words, err := loadWordlist(s.cfg.wordlistPath)
	if err != nil {
		return err
	}
	if len(words) == 0 {
		return nil
	}

	dirs := make([]string, 0, len(s.dirs))
	for dir := range s.dirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		for _, word := range words {
			candidate, err := appendPath(dir, word)
			if err != nil {
				continue
			}

			key := canonicalURLString(candidate)
			if s.parents[key] == "" {
				s.parents[key] = s.dirSource[dir]
				s.reasons[key] = "wordlist"
			}

			if s.cfg.verbose {
				fmt.Fprintf(os.Stderr, "[wordlist] %s\n", candidate)
			}

			u, err := url.Parse(candidate)
			if err != nil || !sameOrigin(s.cfg.target, u) {
				continue
			}

			res, err := s.fetch(ctx, u)
			if err != nil {
				if s.cfg.verbose {
					fmt.Fprintf(os.Stderr, "[erro] %s: %v\n", candidate, err)
				}
				continue
			}
			s.inspectResponse(res, "wordlist")
		}
	}
	return nil
}

func (s *scanner) printFinding(f finding) {
	s.findings++
	if s.cfg.jsonOutput {
		data, err := json.Marshal(f)
		if err == nil {
			fmt.Println(string(data))
		}
		return
	}

	fmt.Printf("\n[REFLECTED-404] %s\n", f.URL)
	fmt.Printf("  status: %d\n", f.Status)
	fmt.Printf("  refletiu: %s\n", f.ReflectedVariant)
	fmt.Printf("  origem: %s\n", f.Source)
	if len(f.Route) > 0 {
		fmt.Println("  caminho:")
		for i, item := range f.Route {
			fmt.Printf("    %d. %s\n", i+1, item)
		}
	}
}

func (s *scanner) routeTo(key string) []string {
	var route []string
	seen := make(map[string]bool)
	current := key

	for current != "" && !seen[current] {
		seen[current] = true
		route = append(route, current)
		current = s.parents[current]
	}

	for i, j := 0, len(route)-1; i < j; i, j = i+1, j-1 {
		route[i], route[j] = route[j], route[i]
	}
	return route
}

func (s *scanner) addDirectoriesFromURL(u *url.URL, source string) {
	clone := cloneURL(u)
	clone.RawQuery = ""
	clone.Fragment = ""
	if clone.Path == "" {
		clone.Path = "/"
	}

	parts := strings.Split(strings.Trim(clone.Path, "/"), "/")
	dirPaths := []string{"/"}
	current := ""
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == len(parts)-1 && looksLikeFile(part) {
			break
		}
		current += "/" + part
		dirPaths = append(dirPaths, current+"/")
	}

	for _, p := range dirPaths {
		clone.Path = p
		dir := canonicalURL(clone)
		if !s.dirs[dir] {
			s.dirs[dir] = true
			s.dirSource[dir] = source
		}
	}
}

func extractLinks(baseRaw string, body []byte) []string {
	doc, err := xhtml.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}

	var rawLinks []string
	var baseHref string

	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch strings.ToLower(n.Data) {
			case "base":
				if baseHref == "" {
					baseHref = attr(n, "href")
				}
			case "a", "area", "link":
				rawLinks = appendIfNotEmpty(rawLinks, attr(n, "href"))
			case "img", "script", "iframe", "frame", "embed", "source", "track", "audio", "video":
				rawLinks = appendIfNotEmpty(rawLinks, attr(n, "src"))
				rawLinks = append(rawLinks, parseSrcset(attr(n, "srcset"))...)
			case "form":
				rawLinks = appendIfNotEmpty(rawLinks, attr(n, "action"))
			case "blockquote", "q", "del", "ins":
				rawLinks = appendIfNotEmpty(rawLinks, attr(n, "cite"))
			case "meta":
				if strings.EqualFold(attr(n, "http-equiv"), "refresh") {
					rawLinks = appendIfNotEmpty(rawLinks, parseMetaRefresh(attr(n, "content")))
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	if baseHref == "" {
		return rawLinks
	}

	baseURL, err := url.Parse(baseRaw)
	if err != nil {
		return rawLinks
	}
	normalizedBase, ok := normalizeURL(baseURL, baseHref)
	if !ok {
		return rawLinks
	}

	rebased := make([]string, 0, len(rawLinks))
	for _, link := range rawLinks {
		u, ok := normalizeURL(normalizedBase, link)
		if !ok {
			continue
		}
		rebased = append(rebased, u.String())
	}
	return rebased
}

func attr(n *xhtml.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

func appendIfNotEmpty(items []string, item string) []string {
	item = strings.TrimSpace(item)
	if item == "" {
		return items
	}
	return append(items, item)
}

func parseSrcset(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) > 0 {
			out = append(out, fields[0])
		}
	}
	return out
}

func parseMetaRefresh(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	idx := strings.Index(lower, "url=")
	if idx == -1 {
		return ""
	}
	return strings.Trim(raw[idx+4:], " '\"")
}

func normalizeURL(base *url.URL, raw string) (*url.URL, bool) {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" {
		return nil, false
	}

	lower := strings.ToLower(raw)
	blockedSchemes := []string{"javascript:", "mailto:", "tel:", "data:", "blob:", "about:", "file:"}
	for _, blocked := range blockedSchemes {
		if strings.HasPrefix(lower, blocked) {
			return nil, false
		}
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}

	abs := base.ResolveReference(parsed)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return nil, false
	}
	if abs.Host == "" {
		return nil, false
	}
	if abs.Path == "" {
		abs.Path = "/"
	}
	abs.Fragment = ""
	abs.Scheme = strings.ToLower(abs.Scheme)
	abs.Host = strings.ToLower(abs.Host)
	return abs, true
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func isHTML(contentType string, body []byte) bool {
	lower := strings.ToLower(contentType)
	if strings.Contains(lower, "text/html") || strings.Contains(lower, "application/xhtml+xml") {
		return true
	}
	prefix := strings.ToLower(string(body[:min(len(body), 512)]))
	return strings.Contains(prefix, "<html") || strings.Contains(prefix, "<!doctype html")
}

func canonicalURL(u *url.URL) string {
	clone := cloneURL(u)
	clone.Fragment = ""
	if clone.Path == "" {
		clone.Path = "/"
	}
	clone.Scheme = strings.ToLower(clone.Scheme)
	clone.Host = strings.ToLower(clone.Host)
	return clone.String()
}

func canonicalURLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return canonicalURL(u)
}

func cloneURL(u *url.URL) *url.URL {
	clone := *u
	return &clone
}

func appendPath(baseRaw, segment string) (string, error) {
	base, err := url.Parse(baseRaw)
	if err != nil {
		return "", err
	}

	segment = strings.TrimSpace(segment)
	segment = strings.TrimLeft(segment, "/")
	if segment == "" {
		return "", errors.New("empty path segment")
	}

	if base.Path == "" {
		base.Path = "/"
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}

	ref, err := url.Parse(segment)
	if err != nil {
		return "", err
	}
	base.RawQuery = ""
	base.Fragment = ""
	return base.ResolveReference(ref).String(), nil
}

func looksLikeFile(segment string) bool {
	if strings.HasPrefix(segment, ".") {
		return false
	}
	return regexp.MustCompile(`\.[a-zA-Z0-9]{1,8}$`).MatchString(segment)
}

type variant struct {
	name  string
	value string
}

func reflectionVariants(requestedRaw, finalRaw string) []variant {
	seen := make(map[string]bool)
	var out []variant

	add := func(name, value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[name+"\x00"+value] {
			return
		}
		seen[name+"\x00"+value] = true
		out = append(out, variant{name: name, value: value})
	}

	for label, raw := range map[string]string{"requested": requestedRaw, "final": finalRaw} {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}

		full := u.String()
		requestURI := u.RequestURI()
		path := u.EscapedPath()
		if path == "" {
			path = "/"
		}

		add(label+":full-url", full)
		add(label+":request-uri", requestURI)
		add(label+":path", path)
		add(label+":html-full-url", html.EscapeString(full))
		add(label+":html-request-uri", html.EscapeString(requestURI))

		if decoded, err := url.QueryUnescape(requestURI); err == nil {
			add(label+":decoded-request-uri", decoded)
			add(label+":html-decoded-request-uri", html.EscapeString(decoded))
		}
		if decoded, err := url.QueryUnescape(path); err == nil {
			add(label+":decoded-path", decoded)
		}
		add(label+":urlencoded-request-uri", url.QueryEscape(requestURI))
	}

	return out
}

func randomToken(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func loadWordlist(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var words []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words = append(words, line)
	}
	return words, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
