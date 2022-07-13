package service

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

const (
	BreadCrumbsEnabled          = true // TODO: modify configurations to support changing this value
	ServiceDestinationUriHeader = "X-Traefik-Dest-Uri"
)

func newSmartRoundTripper(transport *http.Transport, forwardingTimeouts *dynamic.ForwardingTimeouts) (http.RoundTripper, error) {
	transportHTTP1 := transport.Clone()

	transportHTTP2, err := http2.ConfigureTransports(transport)
	if err != nil {
		return nil, err
	}

	if forwardingTimeouts != nil {
		transportHTTP2.ReadIdleTimeout = time.Duration(forwardingTimeouts.ReadIdleTimeout)
		transportHTTP2.PingTimeout = time.Duration(forwardingTimeouts.PingTimeout)
	}

	transportH2C := &h2cTransportWrapper{
		Transport: &http2.Transport{
			DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
			AllowHTTP: true,
		},
	}

	if forwardingTimeouts != nil {
		transportH2C.ReadIdleTimeout = time.Duration(forwardingTimeouts.ReadIdleTimeout)
		transportH2C.PingTimeout = time.Duration(forwardingTimeouts.PingTimeout)
	}

	transport.RegisterProtocol("h2c", transportH2C)

	return &smartRoundTripper{
		http2:           transport,
		http:            transportHTTP1,
		emitBreadCrumbs: BreadCrumbsEnabled,
	}, nil
}

// smartRoundTripper implements RoundTrip while making sure that HTTP/2 is not used
// with protocols that start with a Connection Upgrade, such as SPDY or Websocket.
// Also emits breadcrumbs
type smartRoundTripper struct {
	http2           *http.Transport
	http            *http.Transport
	emitBreadCrumbs bool // TODO: support enabling via status code
}

func (m *smartRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var response *http.Response
	var err error

	// If we have a connection upgrade, we don't use HTTP/2
	if httpguts.HeaderValuesContainsToken(req.Header["Connection"], "Upgrade") {
		response, err = m.http.RoundTrip(req)
	} else {
		response, err = m.http2.RoundTrip(req)
	}
	if err != nil {
		return response, err
	}

	if m.emitBreadCrumbs {
		emitBreadCrumbs(req, response)
	}
	return response, err
}

func emitBreadCrumbs(req *http.Request, response *http.Response) {
	response.Header.Add(ServiceDestinationUriHeader, req.URL.String())
}
