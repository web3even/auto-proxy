package main

import (
	"crypto/tls"
	"flag"
	"github.com/Sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"
)

var listenHttp = flag.String("listen-http", ":80", "The address to listen for HTTP requests")
var listenHttps = flag.String("listen-https", ":443", "The address to listen for HTTPS requests")
var accountKey = flag.String("account-key", "/etc/auto-proxy/account.key", "Where to store the account key")
var certsDirectory = flag.String("certs-dir", "/etc/auto-proxy/certs.d", "Where to store the generated certificates")
var requestBefore = flag.Duration("request-before", time.Hour*24*31, "When to start certificate renewal")
var retryInterval = flag.Duration("retry-interval", time.Hour, "Re-read the certificates")
var defaultCert = flag.String("default-crt", "/etc/auto-proxy/default.crt", "The path to default certificate")
var defaultKey = flag.String("default-key", "/etc/auto-proxy/default.key", "The path to default certificate key")
var useDefaultKey = flag.Bool("use-default-key", true, "All certificates will be generated with the default certificate key")
var ports = flag.String("ports", "80,8080,3000,5000", "Auto-create mapping for these ports")
var insecureSkipVerify = flag.Bool("insecure-skip-verify", false, "Disable SSL/TLS checking for proxied requests")
var http2proto = flag.Bool("http2", true, "Enable HTTP2 support")
var verbose = flag.Bool("debug", false, "Be more verbose")

type theApp struct {
	routes       Routes
	certificates Certificates
	wellKnown    map[string]string
	lock         sync.RWMutex
}

func (a *theApp) update(routes Routes) {
	logrus.Infoln("Updating routes...")
	a.routes = routes
}

func (a *theApp) ServeTLS(ch *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if ch.ServerName == "" {
		return nil, nil
	}

	serverName := ch.ServerName

	// Try to find that certificate
	tls := a.certificates.Find(serverName)
	if tls == nil {
		// Check if we should request that certificate
		route := a.routes.Find(serverName)
		if route != nil {
			tls, _ = a.certificates.Load(serverName, a)
		}
	}

	return tls, nil
}

func (a *theApp) serveWellKnown(w http.ResponseWriter, r *http.Request) bool {
	a.lock.RLock()
	defer a.lock.RUnlock()

	if wellKnown, ok := a.wellKnown[r.RequestURI]; ok {
		written, _ := io.WriteString(w, wellKnown)
		httpLog(200, int64(written), r, time.Now())
		return true
	}
	return false
}

func (a *theApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.serveWellKnown(w, r) {
		return
	}

	route := a.routes.Find(r.Host)
	if route == nil {
		httpServerError(w, r, "no route for", r.Host)
		return
	}

	// Add auto redirect
	if r.TLS == nil && !route.EnableHTTP {
		u, err := url.ParseRequestURI(r.RequestURI)
		if err != nil {
			httpServerError(w, r, "Failed to parse:", r.RequestURI, "with:", err)
			return
		}
		u.Scheme = "https"

		http.Redirect(w, r, u.String(), 307)
		httpLog(307, 0, r, time.Now())
		return
	}

	if len(route.Servers) == 0 {
		httpServerError(w, r, "no upstreams for", r.Host)
		return
	}

	// Add HSTS header
	if r.TLS != nil && !route.EnableHTTP && route.HSTS != "" {
		w.Header().Set("Strict-Transport-Security", route.HSTS)
	}

	// Proxy request
	httpProxyRequest(route.Servers[0], w, r)
}

func (a *theApp) AddCertificate(name string, certificate *tls.Certificate) {
	a.certificates.Add(&Certificate{
		Name: name,
		TLS:  certificate,
	})
}

func (a *theApp) RemoveCertificate(name string) {
	a.certificates.Remove(name)
}

func (a *theApp) AddHttpUri(uriPath, resource string) {
	a.lock.Lock()
	defer a.lock.Unlock()
	if a.wellKnown == nil {
		a.wellKnown = make(map[string]string)
	}
	a.wellKnown[uriPath] = resource
}

func (a *theApp) RemoveHttpUri(uriPath string) {
	a.lock.Lock()
	defer a.lock.Unlock()
	if a.wellKnown != nil {
		delete(a.wellKnown, uriPath)
	}
}

func main() {
	var wg sync.WaitGroup
	var app theApp

	flag.Parse()

	if *verbose {
		logrus.SetLevel(logrus.DebugLevel)
	}

	// Create directories
	os.MkdirAll(*certsDirectory, 0700)
	os.MkdirAll(path.Dir(*accountKey), 0700)
	os.MkdirAll(path.Dir(*defaultCert), 0700)
	os.MkdirAll(path.Dir(*defaultKey), 0700)

	// Create default transport
	defaultTransport = http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: *insecureSkipVerify,
		},
	}

	// Load or create default certificate
	defaultCertificate = &Certificate{
		Name:            "default",
		CertificateFile: *defaultCert,
		KeyFile:         *defaultKey,
	}
	err := defaultCertificate.Load()
	if os.IsNotExist(err) {
		err = defaultCertificate.CreateSelfSigned()
		if err != nil {
			logrus.Fatalln(err)
		}
	} else if err != nil {
		logrus.Fatalln(err)
	}

	// Listen for HTTP
	if *listenHttp != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ListenAndServe(*listenHttp, &app)
			if err != nil {
				logrus.Fatalln(err)
			}
		}()
	}

	// Listen for HTTPS
	if *listenHttps != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ListenAndServeTLS(*listenHttps, defaultCertificate, &app)
			if err != nil {
				logrus.Fatalln(err)
			}
		}()
	}

	// Watch for docker events to generate routes
	go func() {
		watchEvents(app.update)
	}()

	// Renew certificates
	go func() {
		for {
			time.Sleep(time.Hour)
			app.certificates.Tick(&app)
		}
	}()

	wg.Wait()
}