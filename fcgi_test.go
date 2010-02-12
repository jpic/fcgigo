// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fcgi_test

import (
	"io"
	"io/ioutil"
	"http"
	"testing"
	"os"
	"strings"
	"strconv"
	"net"
	"http/fcgi"
	"log"
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
var repeatCount = 60 // should be an even multiple of len(responders) inside the handler, so that we test every responder equally

func createStaticTestFile() os.Error {
	f, err := os.Open("_test/fcgi_test.html", os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	io.WriteString(f, "hello static world")
	f.Close()
	return nil
}
func removeStaticTestFile() { os.Remove("_test/fcgi_test.html") }

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
	if err := createStaticTestFile(); err != nil {
		t.Fatal(err)
		return
	}
	// then start the responders
	tcplisten, _ = fcgi.Listen("tcp", ":0")
	go http.Serve(tcplisten, fcgiMux)
	unixlisten, _ = fcgi.Listen("unix", "_test/unixsocket")
	go http.Serve(unixlisten, fcgiMux)

	// define the muxer for the http server to use
	// (all requests go to the pool of listeners)
	wd, _ := os.Getwd()
	handler, err := fcgi.Handler([]fcgi.Dialer{
		fcgi.NewDialer("tcp", tcplisten.Addr().String()),
		fcgi.NewDialer("unix", unixlisten.Addr().String()),
		fcgi.NewDialer("exec", wd+"/_test/listener_test_exec.out"),
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
	removeStaticTestFile()
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
// gotest: mkdir -p _test
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

// gotest: mkdir -p _test
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

var helloWorldBytes = strings.Bytes("Hello, world!")

func BenchmarkStaticFileOverTCPNoMultiplex(b *testing.B) {
	b.StopTimer()
	fcgiMux := http.NewServeMux()
	fcgiMux.Handle("/static/", http.FileServer("_test/", "/static"))
	if err := createStaticTestFile(); err != nil {
		log.Stderr("Failed to create test file:", err)
		return
	}
	tcplisten, err := fcgi.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("fcgi.Listen error:", err)
		return
	}
	weblisten, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("net.Listen error:", err)
		return
	}
	url := "http://" + weblisten.Addr().String() + "/static/fcgi_test.html"
	handler, err := fcgi.Handler([]fcgi.Dialer{
		fcgi.NewDialer("tcp", tcplisten.Addr().String()),
	})
	if err != nil {
		log.Stderr("fcgi.Handler error:", err)
		return
	}
	http.Handle("/static/", handler)
	go http.Serve(tcplisten, fcgiMux)
	go http.Serve(weblisten, nil)
	b.StartTimer()
	log.Stderr("Loop starting...", b.N)
	for i := 0; i < b.N; i++ {
		response, _, err := http.Get(url)
		if err != nil {
			log.Stderr("http.Get error:", err)
		}
		if response == nil {
			log.Stderr("Nil response.")
			continue
		}
		if response.StatusCode != 200 {
			log.Stderr("Bad response status:", response.StatusCode)
			continue
		}
		if response != nil {
			_, err := ioutil.ReadAll(response.Body)
			if err != nil {
				log.Stderr("ioutil.ReadAll error:", err)
				break
			}
			response.Body.Close()
			// b.SetBytes(int64(len(body)))
		}
	}
	weblisten.Close()
	tcplisten.Close()
	removeStaticTestFile()
}

func BenchmarkStaticFileOverTCPWithMultiplex(b *testing.B) {
	b.StopTimer()
	var C = 50 // number of simultaneous clients
	fcgiMux := http.NewServeMux()
	fcgiMux.Handle("/static/", http.FileServer("_test/", "/static"))
	if err := createStaticTestFile(); err != nil {
		log.Stderr("Failed to create test file:", err)
		return
	}
	tcplisten, err := fcgi.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("fcgi.Listen error:", err)
		return
	}
	weblisten, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("net.Listen error:", err)
		return
	}
	url := "http://" + weblisten.Addr().String() + "/static/fcgi_test.html"
	handler, err := fcgi.Handler([]fcgi.Dialer{
		fcgi.NewDialer("tcp", tcplisten.Addr().String()),
	})
	if err != nil {
		log.Stderr("fcgi.Handler error:", err)
		return
	}
	http.Handle("/static/", handler)
	go http.Serve(tcplisten, fcgiMux)
	go http.Serve(weblisten, nil)

	start := make(chan bool, C) // allow this many simultaneous connections to the webserver
	for i := 0; i < C; i++ {
		start <- true
	}
	done := make(chan bool, b.N) // for syncing all the multiplex goroutines
	b.StartTimer()
	log.Stderr("Loop starting...", b.N)
	for i := 0; i < b.N; i++ {
		go func() {
			<-start
			defer func() {
				done <- true
				start <- true
			}()
			response, _, err := http.Get(url)
			if err != nil {
				log.Stderr("http.Get error:", err)
			}
			if response == nil {
				log.Stderr("Nil response.")
				return
			}
			if response.StatusCode != 200 {
				log.Stderr("Bad response status:", response.StatusCode)
				return
			}
			if response != nil {
				_, err := ioutil.ReadAll(response.Body)
				if err != nil {
					log.Stderr("ioutil.ReadAll error:", err)
					return
				}
				// b.SetBytes(int64(len(body)))
				response.Body.Close()
			}
		}()
	}
	for i := 0; i < b.N; i++ {
		<-done
	}
	weblisten.Close()
	tcplisten.Close()
	removeStaticTestFile()
}

func BenchmarkHelloWorldOverTCPNoMultiplex(b *testing.B) {
	b.StopTimer()
	fcgiMux := http.NewServeMux()
	fcgiMux.Handle("/hello/", http.HandlerFunc(func(conn *http.Conn, req *http.Request) { conn.Write(helloWorldBytes) }))
	tcplisten, err := fcgi.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("fcgi.Listen error:", err)
		return
	}
	weblisten, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("net.Listen error:", err)
		return
	}
	url := "http://" + weblisten.Addr().String() + "/hello/"
	handler, err := fcgi.Handler([]fcgi.Dialer{
		fcgi.NewDialer("tcp", tcplisten.Addr().String()),
	})
	if err != nil {
		log.Stderr("fcgi.Handler error:", err)
		return
	}
	http.Handle("/hello/", handler)
	go http.Serve(tcplisten, fcgiMux)
	go http.Serve(weblisten, nil)
	b.StartTimer()
	log.Stderr("Loop starting...", b.N)
	for i := 0; i < b.N; i++ {
		response, _, err := http.Get(url)
		if err != nil {
			log.Stderr("http.Get error:", err)
		}
		if response == nil {
			log.Stderr("Nil response.")
			continue
		}
		if response.StatusCode != 200 {
			log.Stderr("Bad response status:", response.StatusCode)
			continue
		}
		body, err := ioutil.ReadAll(response.Body)
		if err != nil {
			log.Stderr("ioutil.ReadAll error:", err)
			break
		}
		response.Body.Close()
		b.SetBytes(int64(len(body)))
	}
	weblisten.Close()
	tcplisten.Close()
	removeStaticTestFile()
}

func BenchmarkHelloWorldOverTCPWithMultiplex(b *testing.B) {
	b.StopTimer()
	var C = 50 // number of simultaneous clients
	fcgiMux := http.NewServeMux()
	fcgiMux.Handle("/hello/", http.HandlerFunc(func(conn *http.Conn, req *http.Request) { conn.Write(helloWorldBytes) }))
	tcplisten, err := fcgi.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("fcgi.Listen error:", err)
		return
	}
	weblisten, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("net.Listen error:", err)
		return
	}
	url := "http://" + weblisten.Addr().String() + "/hello/"
	handler, err := fcgi.Handler([]fcgi.Dialer{
		fcgi.NewDialer("tcp", tcplisten.Addr().String()),
	})
	if err != nil {
		log.Stderr("fcgi.Handler error:", err)
		return
	}
	http.Handle("/hello/", handler)
	go http.Serve(tcplisten, fcgiMux)
	go http.Serve(weblisten, nil)

	start := make(chan bool, C) // allow this many simultaneous connections to the webserver
	for i := 0; i < C; i++ {
		start <- true
	}
	done := make(chan bool, b.N) // for syncing all the multiplex goroutines
	b.StartTimer()
	log.Stderr("Loop starting...", b.N)
	for i := 0; i < b.N; i++ {
		go func() {
			<-start
			defer func() {
				done <- true
				start <- true
			}()
			response, _, err := http.Get(url)
			if err != nil {
				log.Stderr("http.Get error:", err)
			}
			if response == nil {
				log.Stderr("Nil response.")
				return
			}
			if response.StatusCode != 200 {
				log.Stderr("Bad response status:", response.StatusCode)
				return
			}
			if response != nil {
				body, err := ioutil.ReadAll(response.Body)
				if err != nil {
					log.Stderr("ioutil.ReadAll error:", err)
					return
				}
				b.SetBytes(int64(len(body)))
				response.Body.Close()
			}
		}()
	}
	for i := 0; i < b.N; i++ {
		<-done
	}
	weblisten.Close()
	tcplisten.Close()
	removeStaticTestFile()
}
/*
func BenchmarkStaticFileOverUnix(b *testing.B) {
	b.StopTimer()
	fcgiMux := http.NewServeMux()
	fcgiMux.Handle("/static/", http.FileServer("_test/", "/static"))
	if err := createStaticTestFile(); err != nil {
		log.Stderr("Failed to create test file:",err)
		return
	}
	tcplisten, err := fcgi.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("fcgi.Listen error:",err)
		return
	}
	weblisten, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Stderr("net.Listen error:",err)
		return
	}
	url := "http://" + weblisten.Addr().String() + "/static/fcgi_test.html"
	handler, err := fcgi.Handler([]fcgi.Dialer{
		fcgi.NewDialer("tcp", tcplisten.Addr().String()),
	})
	if err != nil {
		log.Stderr("fcgi.Handler error:",err)
		return
	}
	http.Handle("/static/", handler)
	go http.Serve(tcplisten, fcgiMux)
	go http.Serve(weblisten, nil)
	b.StartTimer()
	log.Stderr("Loop starting...",b.N)
	for i := 0; i < b.N; i++ {
		response, _, err := http.Get(url)
		if err != nil {
			log.Stderr("http.Get error:",err)
		}
		if response == nil {
			log.Stderr("Nil response.")
			continue
		}
		if response != nil {
			body, err := ioutil.ReadAll(response.Body)
			if err != nil {
				log.Stderr("ioutil.ReadAll error:",err)
				break
			}
			b.SetBytes(int64(len(body)))
		}
	}
	weblisten.Close()
	tcplisten.Close()
	removeStaticTestFile()
}
*/