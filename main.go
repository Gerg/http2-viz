package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"

	"golang.org/x/net/http2"
)

const serverPort = ":8000"
const proxyPort = ":8001"
const clientPort = ":8002"

type Http2Server struct{ Port string }
type Client struct {
	Http2Server
	ProxyPort string
}
type Proxy struct {
	Http2Server
	ServerPort string
}
type Server struct{ Http2Server }

func main() {
	waitGroup := sync.WaitGroup{}

	client := Client{
		Http2Server: Http2Server{Port: clientPort},
		ProxyPort:   proxyPort,
	}
	proxy := Proxy{
		Http2Server: Http2Server{Port: proxyPort},
		ServerPort:  serverPort,
	}
	server := Server{Http2Server: Http2Server{Port: serverPort}}

	waitGroup.Add(3)
	go client.start(&waitGroup)
	go proxy.start(&waitGroup)
	go server.start(&waitGroup)

	waitGroup.Wait()
}

func (c Client) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	serveHttp(c.Http2Server.Port, "client", c.handle)
}

func (c Client) handle(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("https://localhost%s", c.ProxyPort)
	response := c.makeRequest(url)
	fmt.Fprint(w, string(response))
}

func (c Client) makeRequest(url string) string {
	client := &http.Client{}
	client.Transport = buildTlsTransport()

	resp, err := client.Get(url)
	if err != nil {
		log.Fatalf("Failed get: %s", err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed reading response body: %s", err)
	}

	response := map[string]string{
		"code":     strconv.Itoa(resp.StatusCode),
		"protocol": resp.Proto,
		"body":     string(body),
	}

	stringifiedResponse, _ := json.Marshal(response)
	return string(stringifiedResponse)
}

func (p Proxy) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	serveHttp(p.Http2Server.Port, "proxy", p.handle)
}

func (p Proxy) handle(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Proxy request protocol: %s\n", r.Proto)

	origin, _ := url.Parse(fmt.Sprintf("http://localhost%s", p.ServerPort))

	director := func(req *http.Request) {
		req.Header.Add("X-Forwarded-Host", req.Host)
		req.Header.Add("X-Origin-Host", origin.Host)
		req.URL.Scheme = "https"
		req.URL.Host = origin.Host
	}

	proxy := &httputil.ReverseProxy{Director: director}
	proxy.Transport = buildTlsTransport()

	proxy.ServeHTTP(w, r)
}

func (s Server) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	serveHttp(s.Http2Server.Port, "server", s.handle)
}

func (s Server) handle(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Server request protocol: %s\n", r.Proto)
}

func serveHttp(port string, name string, handler http.HandlerFunc) {
	srv := &http.Server{Addr: port, Handler: http.HandlerFunc(handler)}

	log.Printf("Starting %s on https://0.0.0.0%s", name, port)
	log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
}

func buildTlsTransport() *http2.Transport {
	caCert, err := ioutil.ReadFile("server.crt")
	if err != nil {
		log.Fatalf("Reading server certificate: %s", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	return &http2.Transport{
		TLSClientConfig: tlsConfig,
	}
}
