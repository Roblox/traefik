package server

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"

	"github.com/traefik/traefik/types"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

const (
	proxyIPHeader               = "x-roblox-traefik-src"
	serviceDestinationURIHeader = "x-roblox-traefik-dest"
	headerDelimiter             = ";"
)

var (
	localIP     = ""
	localIPOnce = sync.Once{}
)

func newSmartRoundTripper(transport *http.Transport, config *types.BreadCrumbsConfig) (http.RoundTripper, error) {
	transportHTTP1 := transport.Clone()

	err := http2.ConfigureTransport(transport)
	if err != nil {
		return nil, err
	}

	return &smartRoundTripper{
		http2:  transport,
		http:   transportHTTP1,
		config: config,
	}, nil
}

// smartRoundTripper implements RoundTrip while making sure that HTTP/2 is not used
// with protocols that start with a Connection Upgrade, such as SPDY or Websocket.
type smartRoundTripper struct {
	http2  *http.Transport
	http   *http.Transport
	config *types.BreadCrumbsConfig
}

func (m *smartRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var response *http.Response
	var err error

	// If we have a connection upgrade, we don't use HTTP2
	if httpguts.HeaderValuesContainsToken(req.Header["Connection"], "Upgrade") {
		response, err = m.http.RoundTrip(req)
	} else {
		response, err = m.http2.RoundTrip(req)
	}
	if err != nil {
		return response, err
	}

	if m.config != nil {
		enabledFlags := [...]bool{
			false,
			m.config.Enabled1xx,
			m.config.Enabled2xx,
			m.config.Enabled3xx,
			m.config.Enabled4xx,
			m.config.Enabled5xx,
		} // pls golang compiler optimize this :pray:
		if response.StatusCode >= 100 && response.StatusCode < 600 && enabledFlags[response.StatusCode/100] {
			emitBreadCrumbs(req, response, m.config)
		}
	}

	return response, err
}

func (m *smartRoundTripper) GetTLSClientConfig() *tls.Config {
	return m.http2.TLSClientConfig
}

func emitBreadCrumbs(req *http.Request, response *http.Response, config *types.BreadCrumbsConfig) {
	var proxyIP string
	if config.ProxyIP == "" {
		localIPOnce.Do(func() {
			localIP = getLocalIP()
		})
		proxyIP = localIP
	} else {
		proxyIP = config.ProxyIP
	}
	appendToHeader(response, proxyIPHeader, proxyIP)
	appendToHeader(response, serviceDestinationURIHeader, req.URL.String())
}

func appendToHeader(response *http.Response, headerKey, headerVal string) {
	if oldValue := response.Header.Get(headerKey); oldValue != "" {
		response.Header.Add(headerKey, oldValue+headerDelimiter+headerVal)
	} else {
		response.Header.Add(headerKey, headerVal)
	}
}

func getLocalIP() string {
	interfaceAddresses, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, interfaceAddress := range interfaceAddresses {
		if ipnet, ok := interfaceAddress.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}
