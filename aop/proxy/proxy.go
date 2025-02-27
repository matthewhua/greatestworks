package proxy

import (
	"errors"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"sync"

	"greatestworks/aop/logtype"
)

// Proxy is an HTTP proxy that forwards traffic to a set of backends.
type Proxy struct {
	logger   logtype.Logger        // logger
	reverse  httputil.ReverseProxy // underlying proxy
	mu       sync.Mutex            // guards backends
	backends []string              // backend addresses
}

// NewProxy returns a new proxy.
func NewProxy(logger logtype.Logger) *Proxy {
	p := &Proxy{logger: logger}
	p.reverse = httputil.ReverseProxy{Director: p.director}
	return p
}

// ServeHTTP implements the http.Handler interface.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.reverse.ServeHTTP(w, r)
}

// AddBackend adds a backend to the proxy. Note that backends cannot be
// removed.
func (p *Proxy) AddBackend(backend string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = append(p.backends, backend)
}

// director implements a ReverseProxy.Director function [1].
//
// [1]: https://pkg.go.dev/net/http/httputil#ReverseProxy
func (p *Proxy) director(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.backends) == 0 {
		p.logger.Error("director", errors.New("no backends"), "url", r.URL)
		return
	}
	r.URL.Scheme = "http" // TODO(mwhittaker): Support HTTPS.
	r.URL.Host = p.backends[rand.Intn(len(p.backends))]
}
