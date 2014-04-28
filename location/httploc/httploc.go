// HTTP location with load balancing and pluggable middlewares
package httploc

import (
	"fmt"
	log "github.com/mailgun/gotools-log"
	timetools "github.com/mailgun/gotools-time"
	. "github.com/mailgun/vulcan/endpoint"
	"github.com/mailgun/vulcan/failover"
	"github.com/mailgun/vulcan/headers"
	. "github.com/mailgun/vulcan/loadbalance"
	. "github.com/mailgun/vulcan/middleware"
	"github.com/mailgun/vulcan/netutils"
	. "github.com/mailgun/vulcan/request"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Location with built in failover and load balancing support
type HttpLocation struct {
	// Unique identifier of this location
	id string
	// Transport with customized timeouts
	transport *http.Transport
	// Load balancer controls endpoints for this location
	loadBalancer LoadBalancer
	// Timeouts, failover and other optional settings
	options Options
	// Chain with pluggable middlewares that can intercept the request
	middlewareChain *MiddlewareChain
	// Chain of observers that watch the request
	observerChain *ObserverChain
}

// Additional options to control this location, such as timeouts
type Options struct {
	Timeouts struct {
		// Socket read timeout (before we receive the first reply header)
		Read time.Duration
		// Socket connect timeout
		Dial time.Duration
	}
	// Predicate that defines when requests are allowed to failover
	ShouldFailover failover.Predicate
	// Used in forwarding headers
	Hostname string
	// In this case appends new forward info to the existing header
	TrustForwardHeader bool
	// Time provider (useful for testing purposes)
	TimeProvider timetools.TimeProvider
}

func NewLocation(id string, loadBalancer LoadBalancer) (*HttpLocation, error) {
	return NewLocationWithOptions(id, loadBalancer, Options{})
}

func NewLocationWithOptions(id string, loadBalancer LoadBalancer, o Options) (*HttpLocation, error) {
	if loadBalancer == nil {
		return nil, fmt.Errorf("Provide load balancer")
	}
	o, err := parseOptions(o)
	if err != nil {
		return nil, err
	}

	observerChain := NewObserverChain()
	observerChain.Append(BalancerId, loadBalancer)

	middlewareChain := NewMiddlewareChain()
	middlewareChain.Append(BalancerId, loadBalancer)

	return &HttpLocation{
		id:           id,
		loadBalancer: loadBalancer,
		transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return net.DialTimeout(network, addr, o.Timeouts.Dial)
			},
			ResponseHeaderTimeout: o.Timeouts.Read,
		},
		options:         o,
		middlewareChain: middlewareChain,
		observerChain:   observerChain,
	}, nil
}

func (l *HttpLocation) GetMiddlewareChain() *MiddlewareChain {
	return l.middlewareChain
}

func (l *HttpLocation) GetObserverChain() *ObserverChain {
	return l.observerChain
}

// Round trips the request to one of the endpoints and returns the response
func (l *HttpLocation) RoundTrip(req Request) (*http.Response, error) {
	for {
		_, err := req.GetBody().Seek(0, 0)
		if err != nil {
			return nil, err
		}

		endpoint, err := l.loadBalancer.NextEndpoint(req)
		if err != nil {
			log.Errorf("Load Balancer failure: %s", err)
			return nil, err
		}

		// Adds headers, changes urls
		newRequest := l.rewriteRequest(req.GetHttpRequest(), endpoint)

		// In case if error is not nil, we allow load balancer to choose the next endpoint
		// e.g. to do request failover. Nil error means that we got proxied the request successfully.
		response, err := l.proxyToEndpoint(endpoint, req, newRequest)
		if l.options.ShouldFailover(req) {
			log.Errorf("Request(%s) failover", req)
			continue
		} else {
			return response, err
		}
	}
	log.Errorf("All endpoints failed!")
	return nil, fmt.Errorf("All endpoints failed")
}

func (l *HttpLocation) GetLoadBalancer() LoadBalancer {
	return l.loadBalancer
}

func (l *HttpLocation) GetId() string {
	return l.id
}

// Unwind middlewares iterator in reverse order
func (l *HttpLocation) unwindIter(it *MiddlewareIter, req Request, a Attempt) {
	for v := it.Prev(); v != nil; v = it.Prev() {
		v.ProcessResponse(req, a)
	}
}

// Proxy the request to the given endpoint, execute observers and middlewares chains
func (l *HttpLocation) proxyToEndpoint(endpoint Endpoint, req Request, httpReq *http.Request) (*http.Response, error) {

	a := &BaseAttempt{Endpoint: endpoint}

	l.observerChain.ObserveRequest(req)
	defer l.observerChain.ObserveResponse(req, a)
	defer req.AddAttempt(a)

	it := l.middlewareChain.GetIter()
	defer l.unwindIter(it, req, a)

	for v := it.Next(); v != nil; v = it.Next() {
		a.Response, a.Error = v.ProcessRequest(req)
		if a.Response != nil || a.Error != nil {
			log.Errorf("Midleware intercepted request with response=%s, error=%s", a.Response.Status, a.Error)
			return a.Response, a.Error
		}
	}

	// Forward the request and mirror the response
	start := l.options.TimeProvider.UtcNow()
	a.Response, a.Error = l.transport.RoundTrip(httpReq)
	a.Duration = l.options.TimeProvider.UtcNow().Sub(start)
	return a.Response, a.Error
}

// This function alters the original request - adds/removes headers, removes hop headers, changes the request path.
func (l *HttpLocation) rewriteRequest(req *http.Request, endpoint Endpoint) *http.Request {
	outReq := new(http.Request)
	*outReq = *req // includes shallow copies of maps, but we handle this below

	outReq.URL.Scheme = endpoint.GetUrl().Scheme
	outReq.URL.Host = endpoint.GetUrl().Host
	outReq.URL.RawQuery = req.URL.RawQuery

	outReq.Proto = "HTTP/1.1"
	outReq.ProtoMajor = 1
	outReq.ProtoMinor = 1
	outReq.Close = false

	outReq.Header = make(http.Header)
	netutils.CopyHeaders(outReq.Header, req.Header)

	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		if l.options.TrustForwardHeader {
			if prior, ok := outReq.Header[headers.XForwardedFor]; ok {
				clientIP = strings.Join(prior, ", ") + ", " + clientIP
			}
		}
		outReq.Header.Set(headers.XForwardedFor, clientIP)
	}
	if req.TLS != nil {
		outReq.Header.Set(headers.XForwardedProto, "https")
	} else {
		outReq.Header.Set(headers.XForwardedProto, "http")
	}
	if req.Host != "" {
		outReq.Header.Set(headers.XForwardedHost, req.Host)
	}
	outReq.Header.Set(headers.XForwardedServer, l.options.Hostname)

	// Remove hop-by-hop headers to the backend.  Especially important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us.
	netutils.RemoveHeaders(headers.HopHeaders, outReq.Header)
	return outReq
}

// Standard dial and read timeouts, can be overriden when supplying location
const (
	DefaultHttpReadTimeout = time.Duration(10) * time.Second
	DefaultHttpDialTimeout = time.Duration(10) * time.Second
)

func parseOptions(o Options) (Options, error) {
	if o.Timeouts.Read <= time.Duration(0) {
		o.Timeouts.Read = DefaultHttpReadTimeout
	}
	if o.Timeouts.Dial <= time.Duration(0) {
		o.Timeouts.Dial = DefaultHttpDialTimeout
	}

	if o.Hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			o.Hostname = h
		}
	}
	if o.TimeProvider == nil {
		o.TimeProvider = &timetools.RealTime{}
	}
	if o.ShouldFailover == nil {
		// Failover on errors for 2 times maximum on GET requests only.
		o.ShouldFailover = failover.And(failover.MaxAttempts(2), failover.OnErrors, failover.OnGets)
	}
	return o, nil
}

const (
	BalancerId = "__loadBalancer"
)
