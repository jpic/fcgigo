// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fcgi_test

import (
	"io"
	"http"
	"testing"
	"os"
	"strconv"
	"net"
	"http/fcgi"
)

// the same hello, world page from the http tutorial
func HelloServer(c *http.Conn, req *http.Request) {
	io.WriteString(c, "hello, world!\n")
}

var (
	done = make(chan int, 1) // for syncing the multiplex tests
)

type testRecord struct {
	URL        string
	StatusCode int
	BodyPrefix string
	Headers    []headerRecord
}
type headerRecord struct {
	Key string
	Val string
}

// all the different tests we are set up to do, their URLs and expected results
var tests = []testRecord{
	testRecord{
		URL: "/hello/",
		StatusCode: 200,
		BodyPrefix: "hello, world",
	},
	testRecord{
		URL: "/static/fcgi_test.html",
		StatusCode: 200,
		BodyPrefix: "hello static world",
	},
	testRecord{
		URL: "/notfound/",
		StatusCode: 404,
		BodyPrefix: "",
	},
	testRecord{
		URL: "/connection/",
		StatusCode: 200,
		BodyPrefix: "connection test",
		Headers: []headerRecord{
			headerRecord{Key: "Connection", Val: "keep-alive"},
		},
	},
}

// each test is repeated N times
var repeatCount = 6 // should be an even multiple of len(responders) inside the handler, so that we test every responder equally

func startAllServers(t *testing.T) (tcplisten, unixlisten, weblisten net.Listener) {
	var fcgiMux = http.NewServeMux()
	var webMux = http.NewServeMux()
	// define the muxer for the FCGI responders to use
	fcgiMux.Handle("/hello/", http.HandlerFunc(HelloServer))
	fcgiMux.Handle("/notfound/", http.HandlerFunc(http.NotFound))
	fcgiMux.Handle("/connection/", http.HandlerFunc(func(conn *http.Conn, req *http.Request) {
		conn.SetHeader("Connection", "keep-alive")
		io.WriteString(conn, "connection test")
	}))
	fcgiMux.Handle("/static/", http.FileServer("_test/", "/static"))
	f, err := os.Open("_test/fcgi_test.html", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		t.Fatal(err)
		return nil, nil, nil
	}
	io.WriteString(f, "hello static world")
	f.Close()
	// then start the responders
	tcplisten, _ = fcgi.Listen("tcp", ":0")
	go http.Serve(tcplisten, fcgiMux)
	unixlisten, _ = fcgi.Listen("unix", "_test/unixsocket")
	go http.Serve(unixlisten, fcgiMux)

	// define the muxer for the http server to use
	// (all requests go to the pool of listeners)
	wd, _ := os.Getwd()
	handler, err := fcgi.Handler([]string{
		"tcp://" + tcplisten.Addr().String(),
		"unix://" + wd + "/_test/unixsocket",
		"exec://" + wd + "/_test/listener_test_exec.out", // will be ForkExec'd by the Handler (right now)
	})
	if err != nil {
		t.Fatal(err)
		return
	}
	webMux.Handle("/", handler)

	// start the web server
	weblisten, _ = net.Listen("tcp", ":0")
	go http.Serve(weblisten, webMux)

	// return all the data
	return tcplisten, unixlisten, weblisten
}
func stopAllServers(v ...net.Listener) {
	for _, a := range v {
		a.Close()
	}
	os.Remove("_test/fcgi_test.html")
	os.Remove("_test/listener_test_exec.out")
	os.Remove("_test/listener_test_exec.out.8")
}

func runTest(test testRecord, j int, webaddr string, t *testing.T) {
	var response *http.Response
	var err os.Error
	defer func() { done <- 1 }()
	url := "http://" + webaddr + test.URL
	if response, _, err = http.Get(url); err != nil {
		t.Error(err)
	}
	if response.StatusCode != test.StatusCode {
		t.Error(j, webaddr, test.URL, "Response had wrong status code:", response.StatusCode)
	}
	if len(test.BodyPrefix) > 0 {
		prefix := make([]byte, len(test.BodyPrefix))
		if n, err := response.Body.Read(prefix); err == nil {
			p := string(prefix[0:n])
			if p != test.BodyPrefix {
				t.Error(j, webaddr, test.URL, "Bad body, expected prefix:", test.BodyPrefix, "got:", p)
			}
		} else {
			t.Error(j, webaddr, test.URL, "Error reading response.Body:", err)
		}
	}
	if test.Headers != nil {
		for _, hdr := range test.Headers {
			if v := response.GetHeader(hdr.Key); v != hdr.Val {
				t.Error(j, webaddr, test.URL, "Header value in response:", strconv.Quote(v), "did not match", strconv.Quote(hdr.Val))
			}
		}
	}
}

// Build the test executable needed for the exec handler
// gotest: make _test/listener_test_exec.out
func TestRunTests(t *testing.T) {
	tcplisten, unixlisten, weblisten := startAllServers(t)
	webaddr := weblisten.Addr().String()
	for _, test := range tests {
		for j := 0; j < repeatCount; j++ {
			runTest(test, j, webaddr, t)
			<-done
		}
	}
	stopAllServers(tcplisten, unixlisten, weblisten)
}

// gotest: make _test/listener_test_exec.out
func TestRunMultiplexTests(t *testing.T) {
	tcplisten, unixlisten, weblisten := startAllServers(t)
	webaddr := weblisten.Addr().String()
	for _, test := range tests {
		for j := 0; j < repeatCount; j++ {
			go runTest(test, j, webaddr, t)
		}
	}
	for i := 0; i < len(tests)*repeatCount; i++ {
		<-done
	}
	stopAllServers(tcplisten, unixlisten, weblisten)
}
