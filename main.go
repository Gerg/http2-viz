package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sync"

	"golang.org/x/net/http2"
)

const serverPort = ":8000"
const clientPort = ":8001"

type Client struct{}
type Server struct{}

func main() {
	waitGroup := sync.WaitGroup{}

	client := Client{}
	server := Server{}

	waitGroup.Add(2)
	go client.start(&waitGroup)
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
	url := fmt.Sprintf("https://localhost%s", serverPort)
	response := c.makeRequest(url)
	fmt.Fprint(w, response)
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
	return fmt.Sprintf(
		`Response from server:
  Code: %d
  Protocol: %s
  Body:
	  %s
			`,
		resp.StatusCode, resp.Proto, string(body))
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

func (s Server) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	srv := &http.Server{Addr: serverPort, Handler: http.HandlerFunc(s.handle)}

	log.Printf("Starting server on https://0.0.0.0%s", serverPort)
	log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
}

func (s Server) handle(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Request protocol: %s\n", r.Proto)
}
