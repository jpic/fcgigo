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
	"once"
	"net"
	"http/fcgi"
)

// the same hello, world page from the http tutorial
func HelloServer(c *http.Conn, req *http.Request) {
	io.WriteString(c, "hello, world!\n")
}

var (
	// the listeners
	tcplisten, unixlisten, weblisten net.Listener

	// the mux that is shared by all the FastCGI Listeners
	fcgiMux = http.NewServeMux()

	// the mux used by the HTTP Listener
	webMux = http.NewServeMux()
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
		URL: "http://localhost:8181/hello/",
		StatusCode: 200,
		BodyPrefix: "hello, world",
	},
	testRecord{
		URL: "http://localhost:8181/static/fcgi_test.html",
		StatusCode: 200,
		BodyPrefix: "hello static world",
	},
	testRecord{
		URL: "http://localhost:8181/notfound/",
		StatusCode: 404,
		BodyPrefix: "",
	},
	testRecord{
		URL: "http://localhost:8181/connection/",
		StatusCode: 200,
		BodyPrefix: "connection test",
		Headers: []headerRecord{
			headerRecord{Key: "Connection", Val: "keep-alive"},
		},
	},
}
// each test is repeated N times
var repeatCount = 6 // should be an even multiple of len(responders) inside the handler, so that we test every responder equally

func registerFcgiMux() {
	// for hello world test
	fcgiMux.Handle("/hello/", http.HandlerFunc(HelloServer))
	// for testing response status codes
	fcgiMux.Handle("/notfound/", http.HandlerFunc(http.NotFound))
	// for testing does the header make it all the way back (does not test that the connection actually stays open, which is a known limitation of http)
	fcgiMux.Handle("/connection/", http.HandlerFunc(func(conn *http.Conn, req *http.Request) {
		conn.SetHeader("Connection", "keep-alive")
		io.WriteString(conn, "connection test")
	}))
	// for testing the serving of static files
	fcgiMux.Handle("/static/", http.FileServer("/tmp", "/static"))
	f, _ := os.Open("/tmp/fcgi_test.html", os.O_WRONLY|os.O_CREATE, 0666)
	io.WriteString(f, "hello static world")
	f.Close()
}

func registerWebMux() {
	// define the muxer for the http server to use
	// (all requests go to the pool of listeners)
	if wd, err := os.Getwd(); err == nil {
		webMux.Handle("/", fcgi.Handler([]string{
			"tcp://127.0.0.1:7134",
			"unix:///tmp/fcgi_test.sock",
			"exec://" + wd + "/listener_test_exec.out", // will be ForkExec'd by the Handler (right now)
		}))
	} else {
		webMux.Handle("/", fcgi.Handler([]string{
			"tcp://127.0.0.1:7134",
			"unix:///tmp/fcgi_test.sock",
		}))
	}
}

// Build the test executable for this part.
// gotest: make listener_test_exec.out
func TestStartTcpListener(t *testing.T) {
	once.Do(registerFcgiMux)
	var err os.Error
	if tcplisten, err = fcgi.Listen("tcp", "0.0.0.0:7134"); err == nil {
		go http.Serve(tcplisten, fcgiMux)
	} else {
		t.Fatal(err)
	}
}

func TestStartUnixListener(t *testing.T) {
	once.Do(registerFcgiMux)
	var err os.Error
	if unixlisten, err = fcgi.Listen("unix", "/tmp/fcgi_test.sock"); err == nil {
		go http.Serve(unixlisten, fcgiMux)
	} else {
		t.Fatal(err)
	}
}
func TestStopUnixListener(t *testing.T) {
	t.Log("Stopping unix", unixlisten)
	if err := unixlisten.Close(); err != nil {
		t.Error(err)
	}
}
func TestStartUnixListenerAgain(t *testing.T) {
	once.Do(registerFcgiMux)
	if unixlisten, err := fcgi.Listen("unix", "/tmp/fcgi_test.sock"); err == nil {
		go http.Serve(unixlisten, fcgiMux)
	} else {
		t.Fatal(err)
	}
}

func TestStartWebServer(t *testing.T) {
	once.Do(registerWebMux)
	var err os.Error
	if weblisten, err = net.Listen("tcp", ":8181"); err == nil {
		go http.Serve(weblisten, webMux)
	} else {
		t.Fatal(err)
	}
}

func TestRunTests(t *testing.T) {
	for _, test := range tests {
		for j := 0; j < repeatCount; j++ {
			if response, _, err := http.Get(test.URL); err == nil {
				if response.StatusCode != test.StatusCode {
					t.Error(test.URL, j, "Response had wrong status code:", response.StatusCode)
				}
				if len(test.BodyPrefix) > 0 {
					prefix := make([]byte, len(test.BodyPrefix))
					if n, err := response.Body.Read(prefix); err == nil {
						p := string(prefix[0:n])
						if p != test.BodyPrefix {
							t.Error(test.URL, j, "Bad body, expected prefix:", test.BodyPrefix, "got:", p)
						}
					} else {
						t.Error(test.URL, j, "Error reading response.Body:", err)
					}
				}
				if test.Headers != nil {
					for _, hdr := range test.Headers {
						if v := response.GetHeader(hdr.Key); v != hdr.Val {
							t.Error(test.URL, j, "Header value in response:", strconv.Quote(v), "did not match", strconv.Quote(hdr.Val))
						}
					}
				}
			} else {
				t.Error(err)
			}
		}
	}
}

func TestRemoveTmpFile(t *testing.T) {
	os.Remove("/tmp/fcgi_test.html")
	os.Remove("listener_test_exec.out")
}

func TestStopUnixListenerAgain(t *testing.T) {
	t.Log("Stopping unix", unixlisten)
	if err := unixlisten.Close(); err != nil {
		t.Error(err)
	}
}

func TestStopWebServer(t *testing.T) {
	t.Log("Stopping web", weblisten)
	if err := weblisten.Close(); err != nil {
		t.Error(err)
	}
}

func TestStopTcpListener(t *testing.T) {
	t.Log("Stopping TCP", tcplisten)
	if err := tcplisten.Close(); err != nil {
		t.Error(err)
	}
}
