package fcgi

import (
	"io/ioutil"
	"http"
	"testing"
	"os"
	"strings"
)

func Get(url string) (string, os.Error) {
	r, _, err := http.Get(url)
	Log("Test: http.Get returned.")
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", err
	}
	if r.Status != "200 OK" {
		return "", os.NewError("Bad Status")
	}
	Log("Test: reading Body")
	body, _ := ioutil.ReadAll(r.Body)
	Log("Test: closing Body")
	r.Body.Close()
	return string(body), nil
}

func TestServer(t *testing.T) {
	ready := make(chan bool)
	done := make(chan bool, 3) // for this test, let 3 things end without blocking
	exit := make(chan bool)
	// start 2 fcgi responders
	go NewTCPResponder("127.0.0.1:7134", func(req *Request) { req.Write("Hello from 7134") },
		10).Run(ready, exit, done)
	go NewTCPResponder("127.0.0.1:7135", func(req *Request) { req.Write("Hello from 7135") },
		10).Run(ready, exit, done)
	<-ready
	<-ready
	Log("Test: Responders are started.")
	// start a web server that will use them
	go NewWebServer("0.0.0.0:8181", []string{
		"127.0.0.1:7134",
		"127.0.0.1:7135",
	}).Run(ready, exit, done)
	<-ready
	Log("Test: Server is started.")
	if body, err := Get("http://127.0.0.1:8181/"); err == nil {
		if !strings.HasPrefix(body, "Hello") {
			t.Error("Bad Body", body)
		} else {
			Log("Test: got response:", body)
		}
	} else {
		t.Error(err)
	}
	Log("Test: killing servers...")
	exit <- true
	exit <- true
	exit <- true
	Log("Test: waiting for servers to stop...")
	<-done
	<-done
	<-done
}
