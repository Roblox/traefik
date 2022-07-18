package server

import (
	"crypto/tls"
	"net/http"

	"github.com/traefik/traefik/types"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

const (
	ServiceDestinationUriHeader = "x-roblox-traefik-dest"
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
			emitBreadCrumbs(req, response)
		}
	}

	return response, err
}

func (m *smartRoundTripper) GetTLSClientConfig() *tls.Config {
	return m.http2.TLSClientConfig
}

func emitBreadCrumbs(req *http.Request, response *http.Response) {
	response.Header.Add(ServiceDestinationUriHeader, req.URL.String())
}
