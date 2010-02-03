package fcgi_test

import (
	"io"
	"io/ioutil"
	"http"
	"testing"
	"os"
	"strings"
	"strconv"
	"once"
	"net"
	"fmt"
	"fcgi"
)

// helper to fetch a url
func Get(url string) (string, os.Error) {
	r, _, err := http.Get(url)
	if err != nil || r == nil {
		return "", err
	}
	if r.Status != "200 OK" {
		return "", os.NewError(fmt.Sprint("Bad Status", r.Status))
	}
	body, _ := ioutil.ReadAll(r.Body)
	r.Body.Close()
	return string(body), nil
}

// the same hello, world page from the http tutorial
func HelloServer(c *http.Conn, req *http.Request) {
	io.WriteString(c, "hello, world!\n")
}

var (
	flisten, wlisten net.Listener
)

func startAll() {
	if flisten, err := fcgi.Listen("127.0.0.1:7134"); err == nil {
		fcgiMux := http.NewServeMux()
		// for hello world test
		fcgiMux.Handle("/hello", http.HandlerFunc(HelloServer))
		// for testing response status codes
		fcgiMux.Handle("/404", http.HandlerFunc(http.NotFound))
		// for testing does the header make it all the way back
		fcgiMux.Handle("/Connection", http.HandlerFunc(func(conn *http.Conn, req *http.Request) {
			conn.SetHeader("Connection", "keep-alive")
			io.WriteString(conn, "hello, world!\n")
		}))
		go http.Serve(flisten, fcgiMux)
		if wlisten, err = net.Listen("tcp", ":8181"); err == nil {
			// on the webserver side, all requests get routed over
			httpMux := http.NewServeMux()
			httpMux.Handle("/", fcgi.Handler([]string{"127.0.0.1:7134"}))
			go http.Serve(wlisten, httpMux)
		}
	}
}

func stopAll() {
	if flisten != nil {
		flisten.Close()
		flisten = nil
	}
	if wlisten != nil {
		wlisten.Close()
		wlisten = nil
	}
}

func TestFcgiHello(t *testing.T) {
	once.Do(startAll)

	// test a basic hello world request
	if body, err := Get("http://localhost:8181/hello"); err == nil {
		if !strings.HasPrefix(body, "hello") {
			t.Error("Bad Body", body)
		}
	} else {
		t.Error(err)
	}
}

func TestFcgiNotFound(t *testing.T) {
	once.Do(startAll)
	// test a 404 Not Found handler
	if response, _, err := http.Get("http://localhost:8181/404"); err == nil {
		if response.StatusCode != 404 {
			t.Error("/404 page response had wrong status code:", response.StatusCode)
		}
	} else {
		t.Error(err)
	}
}

// this test will always fail right now, because of http
func _testFcgiContentLength(t *testing.T) {
	once.Do(startAll)
	// test that if we specify a fixed content-length that it does not automatically add Transfer-Encoding
	if response, _, err := http.Get("http://localhost:8181/ContentLength"); err == nil {
		if n := response.GetHeader("Content-Length"); n == "" {
			t.Error("Content-Length did not arrive in response.")
		} else {
			if i, _ := strconv.Atoi(n); i != 14 {
				t.Error("Content-Length had wrong value, expected 14, got ", n)
			}
		}
		if body, err := ioutil.ReadAll(response.Body); err == nil {
			if len(body) != 14 {
				t.Error("Body had wrong length,", len(body))
			}
			if !strings.HasPrefix(string(body), "hello, world") {
				t.Error("Body did not match 'hello, world': ", body)
			}
		} else {
			t.Error(err)
		}
		if enc := response.GetHeader("Transfer-Encoding"); enc == "chunked" {
			t.Error("Response still had chunked Transfer-Encoding")
		}
	} else {
		t.Error(err)
	}
}

func TestFcgiConnection(t *testing.T) {
	once.Do(startAll)
	// test what happens if the responder-side handler sets a Connection: keep-alive header
	if response, _, err := http.Get("http://localhost:8181/Connection"); err == nil {
		if n := response.GetHeader("Connection"); n == "" {
			t.Error("Connection did not arrive in response")
		} else if n != "keep-alive" {
			t.Error("Connection had wrong value", n, " != keep-alive")
		}
	} else {
		t.Error(err)
	}
}

func TestStopAll(t *testing.T) { stopAll() }

/*
func TestFcgiHelloWorld(t *testing.T) {
	once.Do(registerHttp)
	// this launches a responder on 7134
	if flisten, err := fcgi.Listen("127.0.0.1:7134"); err == nil {
		go http.Serve(flisten, nil) // so the fcgi responder is the one using the default mux
		// then launch a web server
		if wlisten, err := net.Listen("tcp", ":8181"); err == nil {
			// create a new ServeMux, we cant use the http.DefaultServeMux
			// since in this test both servers are in the same space, and they share the same pattern (/hello)
			mux := http.NewServeMux()
			// this handler sends /hello requests over the wire
			mux.Handle("/hello", fcgi.Handler([]string{"127.0.0.1:7134"}))
			// other handlers can still respond locally, you can freely mix different protocols among different patterns
			// mux.Handler("/", http.HandlerFunc(HelloServer))
			go http.Serve(wlisten, mux)
			log.Stderr("Requesting test page /hello...")
			log.Stderr("Stopping http server...")
			wlisten.Close()
			log.Stderr("Stopping fcgi responder...")
			flisten.Close()
		} else {
			t.Error(err)
		}
	} else {
		t.Error(err)
	}
}
// this is how you launch and test a vanilla hello, world web server
func TestHttpServer(t *testing.T) {
	once.Do(registerHttp)
	log.Stderr("Starting http server...")
	if listen, err := net.Listen("tcp", ":8181"); err == nil {
		go http.Serve(listen, nil)
		log.Stderr("Requesting test page...")
		if body, err := Get("http://localhost:8181/hello"); err == nil {
			if !strings.HasPrefix(body, "hello") {
				t.Error("Bad Body", body)
			}
		} else {
			t.Error(err)
		}
		log.Stderr("Stopping http server...")
		listen.Close()
	} else {
		t.Error(err)
	}
}
*/