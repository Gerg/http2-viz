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

type Client struct{}
type Proxy struct{}
type Server struct{}

func main() {
	waitGroup := sync.WaitGroup{}

	client := Client{}
	proxy := Proxy{}
	server := Server{}

	waitGroup.Add(3)
	go client.start(&waitGroup)
	go proxy.start(&waitGroup)
	go server.start(&waitGroup)

	waitGroup.Wait()
}

func (c Client) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	srv := &http.Server{Addr: clientPort, Handler: http.HandlerFunc(c.handle)}

	log.Printf("Starting client on https://0.0.0.0%s", clientPort)
	log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
}

func (c Client) handle(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("https://localhost%s", proxyPort)
	response := c.makeRequest(url)
	fmt.Fprint(w, string(response))
}

func (c Client) makeRequest(url string) string {
	client := &http.Client{}

	c.configureTLS(client)

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

func (c Client) configureTLS(client *http.Client) {
	caCert, err := ioutil.ReadFile("server.crt")
	if err != nil {
		log.Fatalf("Reading server certificate: %s", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	client.Transport = &http2.Transport{
		TLSClientConfig: tlsConfig,
	}
}

func (p Proxy) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	srv := &http.Server{Addr: proxyPort, Handler: http.HandlerFunc(p.handle)}

	log.Printf("Starting proxy on https://0.0.0.0%s", proxyPort)
	log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
}

func (p Proxy) handle(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Proxy request protocol: %s\n", r.Proto)

	origin, _ := url.Parse(fmt.Sprintf("http://localhost%s", serverPort))

	director := func(req *http.Request) {
		req.Header.Add("X-Forwarded-Host", req.Host)
		req.Header.Add("X-Origin-Host", origin.Host)
		req.URL.Scheme = "https"
		req.URL.Host = origin.Host
	}

	proxy := &httputil.ReverseProxy{Director: director}
	p.configureTLS(proxy)

	proxy.ServeHTTP(w, r)
}

func (p Proxy) configureTLS(proxy *httputil.ReverseProxy) {
	caCert, err := ioutil.ReadFile("server.crt")
	if err != nil {
		log.Fatalf("Reading server certificate: %s", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	proxy.Transport = &http2.Transport{
		TLSClientConfig: tlsConfig,
	}
}

func (s Server) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	srv := &http.Server{Addr: serverPort, Handler: http.HandlerFunc(s.handle)}

	log.Printf("Starting server on https://0.0.0.0%s", serverPort)
	log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
}

func (s Server) handle(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Server request protocol: %s\n", r.Proto)
}
