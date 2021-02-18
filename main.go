package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"sync"

	"golang.org/x/net/http2"
)

const serverPort = ":8000"
const proxyPort = ":8001"
const clientPort = ":8002"
const uiPort = ":8003"

const uiTemplateFile = "ui.tmpl"
const responseBoundary = "~~boundary~~"

type startable interface {
	start(*sync.WaitGroup)
}

type Http2Server struct{ Port string }
type ErrorHandler struct{ Prefix string }
type Ui struct {
	Http2Server
	ErrorHandler
	ClientPort string
}
type Client struct {
	Http2Server
	ErrorHandler
	ProxyPort string
}
type Proxy struct {
	Http2Server
	ErrorHandler
	ServerPort string
}
type Server struct {
	Http2Server
	ErrorHandler
}

type ClientResponse struct {
	ResponseCode     string         `json:"code"`
	ResponseProtocol string         `json:"protocol"`
	ProxyResponse    ProxyResponse  `json:"proxy_response"`
	ServerResponse   ServerResponse `json:"server_response"`
}

type ProxyResponse struct {
	RequestProtocol string `json:"protocol"`
}

type ServerResponse struct {
	RequestProtocol string `json:"protocol"`
}

func main() {
	waitGroup := sync.WaitGroup{}

	ui := Ui{
		Http2Server:  Http2Server{Port: uiPort},
		ErrorHandler: ErrorHandler{Prefix: "UI"},
		ClientPort:   clientPort,
	}
	client := Client{
		Http2Server:  Http2Server{Port: clientPort},
		ErrorHandler: ErrorHandler{Prefix: "Client"},
		ProxyPort:    proxyPort,
	}
	proxy := Proxy{
		Http2Server:  Http2Server{Port: proxyPort},
		ErrorHandler: ErrorHandler{Prefix: "Proxy"},
		ServerPort:   serverPort,
	}
	server := Server{
		Http2Server:  Http2Server{Port: serverPort},
		ErrorHandler: ErrorHandler{Prefix: "Server"},
	}

	startables := []startable{client, proxy, server, ui}

	waitGroup.Add(len(startables))
	for _, aStartable := range startables {
		go aStartable.start(&waitGroup)
	}

	waitGroup.Wait()
}

func (this Ui) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.serveHttp("ui", this.handle, false)
	this.ErrorHandler.handleErr(err, "http2server crashed")
}

func (this Ui) handle(w http.ResponseWriter, r *http.Request) {
	var err error
	templateName := path.Base(uiTemplateFile)
	parsedTemplate := template.Must(template.New(templateName).ParseFiles(uiTemplateFile))

	url := fmt.Sprintf("http://localhost%s", this.ClientPort)
	response := this.makeRequest(url)

	var vizResponse ClientResponse
	err = json.Unmarshal(response, &vizResponse)

	this.ErrorHandler.handleErr(err, "error unmarshalling client response")

	err = parsedTemplate.Execute(w, vizResponse)

	this.ErrorHandler.handleErr(err, "error rendering html")
}

func (this Ui) makeRequest(url string) []byte {
	client := &http.Client{}

	resp, err := client.Get(url)
	this.ErrorHandler.handleErr(err, "failed GET to client")

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	this.ErrorHandler.handleErr(err, "Failed reading client response body")

	return body
}

func (this Client) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.serveHttp("client", this.handle, false)
	this.ErrorHandler.handleErr(err, "http2server crashed")
}

func (this Client) handle(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("https://localhost%s", this.ProxyPort)
	proxyResponse := this.makeRequest(url)

	defer proxyResponse.Body.Close()

	body, err := ioutil.ReadAll(proxyResponse.Body)
	this.ErrorHandler.handleErr(err, "failed reading proxy response body")

	splitResponses := bytes.Split(body, []byte(responseBoundary))
	proxyResponseBody, serverResponseBody := splitResponses[0], splitResponses[1]

	var parsedProxyResponse ProxyResponse
	var parsedServerResponse ServerResponse

	err = json.Unmarshal(proxyResponseBody, &parsedProxyResponse)
	this.ErrorHandler.handleErr(err, "failed unmarshalling proxy response")

	err = json.Unmarshal(serverResponseBody, &parsedServerResponse)
	this.ErrorHandler.handleErr(err, "failed unmarshalling server response")

	response := ClientResponse{
		ResponseProtocol: proxyResponse.Proto,
		ResponseCode:     strconv.Itoa(proxyResponse.StatusCode),
		ProxyResponse:    parsedProxyResponse,
		ServerResponse:   parsedServerResponse,
	}

	jsonResponse, err := json.Marshal(response)
	this.ErrorHandler.handleErr(err, "failed jsonifying client response")

	fmt.Fprint(w, string(jsonResponse))
}

func (this Client) makeRequest(url string) *http.Response {
	client := &http.Client{}
	transport, err := buildTlsTransport()
	this.ErrorHandler.handleErr(err, "failed building TLS transport")

	client.Transport = transport

	response, err := client.Get(url)
	this.ErrorHandler.handleErr(err, "failed GET to proxy")

	return response
}

func (this Proxy) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.serveHttp("proxy", this.handle, true)
	this.ErrorHandler.handleErr(err, "http2server crashed")
}

func (this Proxy) handle(w http.ResponseWriter, r *http.Request) {
	response := ProxyResponse{
		RequestProtocol: r.Proto,
	}
	jsonResponse, err := json.Marshal(response)
	this.ErrorHandler.handleErr(err, "failed jsonifying proxy response")

	fmt.Fprint(w, string(jsonResponse))
	fmt.Fprint(w, responseBoundary)

	origin, _ := url.Parse(fmt.Sprintf("http://localhost%s", this.ServerPort))

	director := func(req *http.Request) {
		req.Header.Add("X-Forwarded-Host", req.Host)
		req.Header.Add("X-Origin-Host", origin.Host)
		req.URL.Scheme = "https"
		req.URL.Host = origin.Host
	}

	proxy := &httputil.ReverseProxy{Director: director}
	transport, err := buildTlsTransport()
	this.ErrorHandler.handleErr(err, "failed building TLS transport")

	proxy.Transport = transport

	proxy.ServeHTTP(w, r)
}

func (this Server) start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.serveHttp("server", this.handle, true)
	this.ErrorHandler.handleErr(err, "http2server crashed")
}

func (this Server) handle(w http.ResponseWriter, r *http.Request) {
	response := ServerResponse{
		RequestProtocol: r.Proto,
	}
	jsonResponse, err := json.Marshal(response)
	this.ErrorHandler.handleErr(err, "failed jsonifying server response")

	fmt.Fprint(w, string(jsonResponse))
}

func (this Http2Server) serveHttp(name string, handler http.HandlerFunc, tls bool) (err error) {
	srv := &http.Server{Addr: this.Port, Handler: http.HandlerFunc(handler)}

	var scheme string
	if tls {
		scheme = "https"
	} else {
		scheme = "http"
	}

	log.Printf("Starting %s on %s://0.0.0.0%s", name, scheme, this.Port)

	if tls {
		err = srv.ListenAndServeTLS("server.crt", "server.key")
	} else {
		err = srv.ListenAndServe()
	}
	return
}

func buildTlsTransport() (*http2.Transport, error) {
	caCert, err := ioutil.ReadFile("server.crt")
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}
	transport := &http2.Transport{
		TLSClientConfig: tlsConfig,
	}
	return transport, nil
}

func (this ErrorHandler) handleErr(err error, errorMessage string) {
	if err != nil {
		log.Fatalf("%s: %s: %s", this.Prefix, errorMessage, err)
	}
}
