package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-etcd/etcd"
	"github.com/flynn/go-discoverd"
)

type HTTPFrontend struct {
	Addr      string
	TLSAddr   string
	TLSConfig *tls.Config

	mtx      sync.RWMutex
	domains  map[string]*httpServer
	services map[string]*httpServer

	etcdPrefix string

	etcd      EtcdClient
	discoverd DiscoverdClient
}

type EtcdClient interface {
	Create(key string, value string, ttl uint64) (*etcd.Response, error)
	Get(key string, sort, recursive bool) (*etcd.Response, error)
	Watch(prefix string, waitIndex uint64, recursive bool, receiver chan *etcd.Response, stop chan bool) (*etcd.Response, error)
}

type DiscoverdClient interface {
	NewServiceSet(string) (discoverd.ServiceSet, error)
}

func NewHTTPFrontend(addr string, etcdc EtcdClient, discoverdc DiscoverdClient) *HTTPFrontend {
	return &HTTPFrontend{
		Addr:       addr,
		etcd:       etcdc,
		etcdPrefix: "/strowger/http/",
		discoverd:  discoverdc,
		domains:    make(map[string]*httpServer),
		services:   make(map[string]*httpServer),
	}
}

func (s *HTTPFrontend) AddHTTPDomain(domain string, service string, certs [][]byte, key []byte) error {
	return s.addDomain(domain, service, true)
}

var ErrDomainExists = errors.New("strowger: domain exists with different service")

func (s *HTTPFrontend) addDomain(domain string, service string, persist bool) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if server, ok := s.domains[domain]; ok {
		if server.name != service {
			return ErrDomainExists
		}
		return nil
	}

	server := s.services[service]
	if server == nil {
		services, err := s.discoverd.NewServiceSet(service)
		if err != nil {
			return err
		}
		server = &httpServer{name: service, services: services}
	}
	if persist {
		if _, err := s.etcd.Create(s.etcdPrefix+domain+"/service", service, 0); err != nil {
			return err
		}
	}
	// TODO: set cert/key data if provided

	server.refs++
	s.domains[domain] = server
	s.services[service] = server
	// TODO: TLS config

	log.Println("Add service", service, "to domain", domain)

	return nil
}

func (s *HTTPFrontend) RemoveHTTPDomain(domain string) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	server := s.domains[domain]
	if server == nil {
		return
	}
	delete(s.domains, domain)
	server.refs++
	if server.refs <= 0 {
		// TODO: close service set stream
		delete(s.services, server.name)
	}
	// TODO: persist
}

func (s *HTTPFrontend) syncDatabase() {
	var since uint64
	data, err := s.etcd.Get(s.etcdPrefix, false, true)
	if e, ok := err.(*etcd.EtcdError); ok && e.ErrorCode == 100 {
		// key not found, ignore
		goto watch
	}
	if err != nil {
		log.Fatal(err)
		return
	}
	since = data.EtcdIndex
	for _, node := range data.Node.Nodes {
		if !node.Dir {
			continue
		}
		domain := path.Base(node.Key)
		serviceRes, err := s.etcd.Get(node.Key+"/service", false, false)
		if err != nil {
			log.Fatal(err)
		}
		if err := s.addDomain(domain, serviceRes.Node.Value, false); err != nil {
			log.Fatal(err)
		}
	}

watch:
	stream := make(chan *etcd.Response)
	stop := make(chan bool)
	// TODO: store stop
	go s.etcd.Watch(s.etcdPrefix, since, false, stream, stop)
	for res := range stream {
		if res.Node.Dir || path.Base(res.Node.Key) == "service" {
			continue
		}
		domain := path.Base(path.Dir(res.Node.Key))
		s.mtx.Lock()
		_, exists := s.domains[domain]
		s.mtx.Unlock()
		if !exists {
			if err := s.addDomain(domain, res.Node.Value, false); err != nil {
				// TODO: log error
			}
		}
	}
	// TODO: handle delete
	log.Println("done watching etcd")
}

func (s *HTTPFrontend) serve() {
	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		// TODO: log error
		return
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			// TODO: log error
			break
		}
		go s.handle(conn)
	}
}

func (s *HTTPFrontend) serveTLS() {
	l, err := net.Listen("tcp", s.TLSAddr)
	if err != nil {
		// TODO: log error
		return
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			// TODO: log error
			break
		}
		go s.handle(conn)
	}
}

func fail(sc *httputil.ServerConn, req *http.Request, code int, msg string) {
	resp := &http.Response{
		StatusCode:    code,
		ProtoMajor:    1,
		ProtoMinor:    0,
		Request:       req,
		Body:          ioutil.NopCloser(bytes.NewBufferString(msg)),
		ContentLength: int64(len(msg)),
	}
	sc.Write(req, resp)
}

func (s *HTTPFrontend) handle(conn net.Conn) {
	defer conn.Close()
	sc := httputil.NewServerConn(conn, nil)
	req, err := sc.Read()
	if err != nil {
		if err != httputil.ErrPersistEOF {
			// TODO: log error
		}
		return
	}

	s.mtx.RLock()
	// TODO: handle wildcard domains
	backend := s.domains[req.Host]
	s.mtx.RUnlock()
	log.Println(req, backend)
	if backend == nil {
		fail(sc, req, 404, "Not Found")
		return
	}
	_, tls := conn.(*tls.Conn)
	backend.handle(req, sc, tls)
}

type httpServer struct {
	name     string
	services discoverd.ServiceSet
	refs     int
}

func (s *httpServer) getBackend() *httputil.ClientConn {
	for _, addr := range shuffle(s.services.Addrs()) {
		// TODO: set connection timeout
		backend, err := net.Dial("tcp", addr)
		if err != nil {
			// TODO: log error
			// TODO: limit number of backends tried
			// TODO: temporarily quarantine failing backends
			log.Println("backend error", err)
			continue
		}
		return httputil.NewClientConn(backend, nil)
	}
	// TODO: log no backends found error
	return nil
}

func (s *httpServer) handle(req *http.Request, sc *httputil.ServerConn, tls bool) {
	req.Header.Set("X-Request-Start", strconv.FormatInt(time.Now().UnixNano()/int64(time.Millisecond), 10))
	backend := s.getBackend()
	if backend == nil {
		log.Println("no backend found")
		fail(sc, req, 503, "Service Unavailable")
		return
	}
	defer backend.Close()

	for {
		if req.Method != "GET" && req.Method != "POST" && req.Method != "HEAD" &&
			req.Method != "OPTIONS" && req.Method != "PUT" && req.Method != "DELETE" && req.Method != "TRACE" {
			fail(sc, req, 405, "Method not allowed")
			return
		}

		req.Proto = "HTTP/1.1"
		req.ProtoMajor = 1
		req.ProtoMinor = 1
		delete(req.Header, "Te")
		delete(req.Header, "Transfer-Encoding")

		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			// If we aren't the first proxy retain prior
			// X-Forwarded-For information as a comma+space
			// separated list and fold multiple headers into one.
			if prior, ok := req.Header["X-Forwarded-For"]; ok {
				clientIP = strings.Join(prior, ", ") + ", " + clientIP
			}
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		if tls {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
		// TODO: Set X-Forwarded-Port

		if err := backend.Write(req); err != nil {
			log.Println("server write err:", err)
			return
		}
		res, err := backend.Read(req)
		if res != nil {
			if err := sc.Write(req, res); err != nil {
				if err != io.EOF && err != httputil.ErrPersistEOF {
					log.Println("client write err:", err)
					// TODO: log error
				}
				return
			}
		}
		if err != nil {
			if err != io.EOF && err != httputil.ErrPersistEOF {
				log.Println("server read err:", err)
				// TODO: log error
				fail(sc, req, 502, "Bad Gateway")
			}
			return
		}

		// TODO: Proxy HTTP CONNECT? (example: Go RPC over HTTP)
		if res.StatusCode == http.StatusSwitchingProtocols {
			serverW, serverR := backend.Hijack()
			clientW, clientR := sc.Hijack()
			defer serverW.Close()
			done := make(chan struct{})
			go func() {
				serverR.WriteTo(clientW)
				close(done)
			}()
			clientR.WriteTo(serverW)
			<-done
			return
		}

		// TODO: http pipelining
		req, err = sc.Read()
		if err != nil {
			if err != io.EOF && err != httputil.ErrPersistEOF {
				log.Println("client read err:", err)
			}
			return
		}
		req.Header.Set("X-Request-Start", strconv.FormatInt(time.Now().UnixNano()/int64(time.Millisecond), 10))
	}
}

func shuffle(s []string) []string {
	for i := len(s) - 1; i > 0; i-- {
		j := rand.Intn(i + 1)
		s[i], s[j] = s[j], s[i]
	}
	return s
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
