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

type startable interface {
	start(*sync.WaitGroup)
}

type Http2Server struct{ Port string }
type ErrorHandler struct{ Prefix string }
type TransportFactory struct{}
type HttpVersion string

type Ui struct {
	ClientPort string
	ErrorHandler
	Http2Server
}
type Client struct {
	ErrorHandler
	Http2Server
	ProxyPort string
	TransportFactory
}
type Proxy struct {
	ErrorHandler
	Http2Server
	ServerPort string
	TransportFactory
}
type Server struct {
	ErrorHandler
	Http2Server
}

type ClientResponse struct {
	ProxyResponse    ProxyResponse  `json:"proxy_response"`
	ResponseCode     string         `json:"code"`
	ResponseProtocol string         `json:"protocol"`
	ServerResponse   ServerResponse `json:"server_response"`
}

type ProxyResponse struct {
	RequestProtocol string `json:"protocol"`
}

type ServerResponse struct {
	RequestProtocol string `json:"protocol"`
}

type ViewData struct {
	ClientResponse
	ClientUseHTTP2 bool
}

const (
	Http1 HttpVersion = "http1"
	Http2 HttpVersion = "http2"
)

const serverPort = ":8000"
const proxyPort = ":8001"
const clientPort = ":8002"
const uiPort = ":8003"

const uiTemplateFile = "ui.tmpl"
const responseBoundary = "~~boundary~~"

func main() {
	waitGroup := sync.WaitGroup{}

	ui := Ui{
		ClientPort:   clientPort,
		ErrorHandler: ErrorHandler{Prefix: "UI"},
		Http2Server:  Http2Server{Port: uiPort},
	}
	client := Client{
		ErrorHandler:     ErrorHandler{Prefix: "Client"},
		Http2Server:      Http2Server{Port: clientPort},
		ProxyPort:        proxyPort,
		TransportFactory: TransportFactory{},
	}
	proxy := Proxy{
		ErrorHandler:     ErrorHandler{Prefix: "Proxy"},
		Http2Server:      Http2Server{Port: proxyPort},
		ServerPort:       serverPort,
		TransportFactory: TransportFactory{},
	}
	server := Server{
		ErrorHandler: ErrorHandler{Prefix: "Server"},
		Http2Server:  Http2Server{Port: serverPort},
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

type Configuration struct {
	ClientUseHttp2 bool
}

func (this Ui) handle(w http.ResponseWriter, r *http.Request) {
	clientHost := fmt.Sprintf("localhost%s", this.ClientPort)
	clientUrl := url.URL{
		Scheme:   "http",
		Host:     clientHost,
		RawQuery: r.URL.RawQuery,
	}

	configuration := this.parseQuery(r)
	response := this.makeRequest(clientUrl.String())

	var clientResponse ClientResponse
	err := json.Unmarshal(response, &clientResponse)
	this.ErrorHandler.handleErr(err, "error unmarshalling client response")

	viewData := ViewData{
		ClientResponse: clientResponse,
		ClientUseHTTP2: configuration.ClientUseHttp2,
	}

	this.renderTemplate(w, viewData)
}

func (this Ui) parseQuery(r *http.Request) Configuration {
	clientHttp2Param, ok := r.URL.Query()["client-http2"]
	useHttp2 := ok && (clientHttp2Param[0] == "true")

	return Configuration{
		ClientUseHttp2: useHttp2,
	}
}

func (this Ui) renderTemplate(w http.ResponseWriter, viewData ViewData) {
	templateName := path.Base(uiTemplateFile)
	parsedTemplate := template.Must(template.New(templateName).ParseFiles(uiTemplateFile))

	err := parsedTemplate.Execute(w, viewData)
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
	configuration := this.parseQuery(r)
	proxyResponse := this.makeRequest(url, configuration.ClientUseHttp2)

	parsedProxyResponse, parsedServerResponse := this.parseResponse(proxyResponse)

	clientResponse := ClientResponse{
		ResponseProtocol: proxyResponse.Proto,
		ResponseCode:     strconv.Itoa(proxyResponse.StatusCode),
		ProxyResponse:    parsedProxyResponse,
		ServerResponse:   parsedServerResponse,
	}

	jsonResponse, err := json.Marshal(clientResponse)
	this.ErrorHandler.handleErr(err, "failed jsonifying client response")

	fmt.Fprint(w, string(jsonResponse))
}

func (this Client) parseQuery(r *http.Request) Configuration {
	clientHttp2Param, ok := r.URL.Query()["client-http2"]
	useHttp2 := ok && (clientHttp2Param[0] == "true")

	return Configuration{
		ClientUseHttp2: useHttp2,
	}
}

func (this Client) makeRequest(url string, useHttp2 bool) *http.Response {
	client := &http.Client{}

	var transport http.RoundTripper
	var err error

	if useHttp2 {
		transport, err = this.TransportFactory.buildHttp2Transport()
		this.ErrorHandler.handleErr(err, "failed building HTTP2 transport")
	} else {
		transport, err = this.TransportFactory.buildHttp1Transport()
		this.ErrorHandler.handleErr(err, "failed building HTTP1 transport")
	}

	client.Transport = transport

	response, err := client.Get(url)
	this.ErrorHandler.handleErr(err, "failed GET to proxy")

	return response
}

func (this Client) parseResponse(proxyResponse *http.Response) (ProxyResponse, ServerResponse) {
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

	return parsedProxyResponse, parsedServerResponse
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
	transport, err := this.TransportFactory.buildHttp2Transport()
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

func (this TransportFactory) buildHttp2Transport() (http.RoundTripper, error) {
	return this.buildTransport(Http2)
}

func (this TransportFactory) buildHttp1Transport() (http.RoundTripper, error) {
	return this.buildTransport(Http1)
}

func (this TransportFactory) buildTransport(httpVersion HttpVersion) (http.RoundTripper, error) {
	caCert, err := ioutil.ReadFile("server.crt")
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}

	var transport http.RoundTripper

	switch httpVersion {
	case Http1:
		transport = &http.Transport{TLSClientConfig: tlsConfig}
	case Http2:
		transport = &http2.Transport{TLSClientConfig: tlsConfig}
	}

	return transport, nil
}

func (this ErrorHandler) handleErr(err error, errorMessage string) {
	if err != nil {
		log.Fatalf("%s: %s: %s", this.Prefix, errorMessage, err)
	}
}
