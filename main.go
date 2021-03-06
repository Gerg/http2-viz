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
	Start(*sync.WaitGroup)
}

type Http2Server struct{ Port string }
type ErrorHandler struct{ Prefix string }
type TransportFactory struct{}
type ConfigurationParser struct{}
type HttpVersion string

type Ui struct {
	ClientPort string
	ConfigurationParser
	ErrorHandler
	Http2Server
}
type Client struct {
	ConfigurationParser
	ErrorHandler
	Http2Server
	ProxyPort string
	TransportFactory
}
type Proxy struct {
	ConfigurationParser
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

type Configuration struct {
	ClientUseHttp2 bool
	ProxyUseHttp2  bool
}

type ViewData struct {
	ClientResponse
	ClientUseHTTP2 bool
	ProxyUseHTTP2  bool
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
		ClientPort:          clientPort,
		ConfigurationParser: ConfigurationParser{},
		ErrorHandler:        ErrorHandler{Prefix: "UI"},
		Http2Server:         Http2Server{Port: uiPort},
	}
	client := Client{
		ConfigurationParser: ConfigurationParser{},
		ErrorHandler:        ErrorHandler{Prefix: "Client"},
		Http2Server:         Http2Server{Port: clientPort},
		ProxyPort:           proxyPort,
		TransportFactory:    TransportFactory{},
	}
	proxy := Proxy{
		ConfigurationParser: ConfigurationParser{},
		ErrorHandler:        ErrorHandler{Prefix: "Proxy"},
		Http2Server:         Http2Server{Port: proxyPort},
		ServerPort:          serverPort,
		TransportFactory:    TransportFactory{},
	}
	server := Server{
		ErrorHandler: ErrorHandler{Prefix: "Server"},
		Http2Server:  Http2Server{Port: serverPort},
	}

	startables := []startable{client, proxy, server, ui}

	waitGroup.Add(len(startables))
	for _, aStartable := range startables {
		go aStartable.Start(&waitGroup)
	}

	waitGroup.Wait()
}

func (this Ui) Start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.ServeHttp("ui", this.handle, false)
	this.ErrorHandler.HandleErr(err, "http2server crashed")
}

func (this Ui) handle(w http.ResponseWriter, r *http.Request) {
	clientHost := fmt.Sprintf("localhost%s", this.ClientPort)
	clientUrl := url.URL{
		Scheme:   "http",
		Host:     clientHost,
		RawQuery: r.URL.RawQuery,
	}

	response := this.makeRequest(clientUrl.String())

	var clientResponse ClientResponse
	err := json.Unmarshal(response, &clientResponse)
	this.ErrorHandler.HandleErr(err, "error unmarshalling client response")

	configuration := this.ConfigurationParser.Parse(r)
	viewData := ViewData{
		ClientResponse: clientResponse,
		ClientUseHTTP2: configuration.ClientUseHttp2,
		ProxyUseHTTP2:  configuration.ProxyUseHttp2,
	}

	this.renderTemplate(w, viewData)
}

func (this Ui) renderTemplate(w http.ResponseWriter, viewData ViewData) {
	templateName := path.Base(uiTemplateFile)
	parsedTemplate := template.Must(template.New(templateName).ParseFiles(uiTemplateFile))

	err := parsedTemplate.Execute(w, viewData)
	this.ErrorHandler.HandleErr(err, "error rendering html")
}

func (this Ui) makeRequest(url string) []byte {
	client := &http.Client{}

	resp, err := client.Get(url)
	this.ErrorHandler.HandleErr(err, "failed GET to client")

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	this.ErrorHandler.HandleErr(err, "Failed reading client response body")

	return body
}

func (this Client) Start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.ServeHttp("client", this.handle, false)
	this.ErrorHandler.HandleErr(err, "http2server crashed")
}

func (this Client) handle(w http.ResponseWriter, r *http.Request) {
	proxyHost := fmt.Sprintf("localhost%s", this.ProxyPort)
	proxyUrl := url.URL{
		Scheme:   "https",
		Host:     proxyHost,
		RawQuery: r.URL.RawQuery,
	}

	configuration := this.ConfigurationParser.Parse(r)
	proxyResponse := this.makeRequest(proxyUrl.String(), configuration.ClientUseHttp2)

	parsedProxyResponse, parsedServerResponse := this.parseResponse(proxyResponse)

	clientResponse := ClientResponse{
		ResponseProtocol: proxyResponse.Proto,
		ResponseCode:     strconv.Itoa(proxyResponse.StatusCode),
		ProxyResponse:    parsedProxyResponse,
		ServerResponse:   parsedServerResponse,
	}

	jsonResponse, err := json.Marshal(clientResponse)
	this.ErrorHandler.HandleErr(err, "failed jsonifying client response")

	fmt.Fprint(w, string(jsonResponse))
}

func (this Client) makeRequest(url string, useHttp2 bool) *http.Response {
	client := &http.Client{}

	var transport http.RoundTripper
	var err error

	if useHttp2 {
		transport, err = this.TransportFactory.BuildHttp2Transport()
		this.ErrorHandler.HandleErr(err, "failed building HTTP2 transport")
	} else {
		transport, err = this.TransportFactory.BuildHttp1Transport()
		this.ErrorHandler.HandleErr(err, "failed building HTTP1 transport")
	}

	client.Transport = transport

	response, err := client.Get(url)
	this.ErrorHandler.HandleErr(err, "failed GET to proxy")

	return response
}

func (this Client) parseResponse(proxyResponse *http.Response) (ProxyResponse, ServerResponse) {
	defer proxyResponse.Body.Close()

	body, err := ioutil.ReadAll(proxyResponse.Body)
	this.ErrorHandler.HandleErr(err, "failed reading proxy response body")

	splitResponses := bytes.Split(body, []byte(responseBoundary))
	proxyResponseBody, serverResponseBody := splitResponses[0], splitResponses[1]

	var parsedProxyResponse ProxyResponse
	var parsedServerResponse ServerResponse

	err = json.Unmarshal(proxyResponseBody, &parsedProxyResponse)
	this.ErrorHandler.HandleErr(err, "failed unmarshalling proxy response")

	err = json.Unmarshal(serverResponseBody, &parsedServerResponse)
	this.ErrorHandler.HandleErr(err, "failed unmarshalling server response")

	return parsedProxyResponse, parsedServerResponse
}

func (this Proxy) Start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.ServeHttp("proxy", this.handle, true)
	this.ErrorHandler.HandleErr(err, "http2server crashed")
}

func (this Proxy) handle(w http.ResponseWriter, r *http.Request) {
	response := ProxyResponse{
		RequestProtocol: r.Proto,
	}
	jsonResponse, err := json.Marshal(response)
	this.ErrorHandler.HandleErr(err, "failed jsonifying proxy response")

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

	configuration := this.ConfigurationParser.Parse(r)

	var transport http.RoundTripper

	if configuration.ProxyUseHttp2 {
		transport, err = this.TransportFactory.BuildHttp2Transport()
		this.ErrorHandler.HandleErr(err, "failed building HTTP2 transport")
	} else {
		transport, err = this.TransportFactory.BuildHttp1Transport()
		this.ErrorHandler.HandleErr(err, "failed building HTTP1 transport")
	}

	proxy.Transport = transport

	proxy.ServeHTTP(w, r)
}

func (this Server) Start(waitGroup *sync.WaitGroup) {
	defer waitGroup.Done()

	err := this.Http2Server.ServeHttp("server", this.handle, true)
	this.ErrorHandler.HandleErr(err, "http2server crashed")
}

func (this Server) handle(w http.ResponseWriter, r *http.Request) {
	response := ServerResponse{
		RequestProtocol: r.Proto,
	}
	jsonResponse, err := json.Marshal(response)
	this.ErrorHandler.HandleErr(err, "failed jsonifying server response")

	fmt.Fprint(w, string(jsonResponse))
}

func (this Http2Server) ServeHttp(name string, handler http.HandlerFunc, tls bool) (err error) {
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

func (this TransportFactory) BuildHttp2Transport() (http.RoundTripper, error) {
	return this.buildTransport(Http2)
}

func (this TransportFactory) BuildHttp1Transport() (http.RoundTripper, error) {
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

func (this ErrorHandler) HandleErr(err error, errorMessage string) {
	if err != nil {
		log.Fatalf("%s: %s: %s", this.Prefix, errorMessage, err)
	}
}

func (this ConfigurationParser) Parse(r *http.Request) Configuration {
	clientHttp2Param, ok := r.URL.Query()["client-http2"]
	clientUseHttp2 := ok && (clientHttp2Param[0] == "true")

	proxyHttp2Param, ok := r.URL.Query()["proxy-http2"]
	proxyUseHttp2 := ok && (proxyHttp2Param[0] == "true")

	return Configuration{
		ClientUseHttp2: clientUseHttp2,
		ProxyUseHttp2:  proxyUseHttp2,
	}
}
