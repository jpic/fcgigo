package main

import (
	"http"
	"http/fcgi"
	"io"
)

func HelloServer(con *http.Conn, req *http.Request) {
	io.WriteString(con, "hello, world\r\n")
}

func main() {
	// the hello world test from the http docs
	http.Handle("/hello/", http.HandlerFunc(HelloServer))
	// for testing response status codes
	http.Handle("/notfound/", http.HandlerFunc(http.NotFound))
	// for testing does the header make it all the way back (does not test that the connection actually stays open, which is a known limitation of http)
	http.Handle("/connection/", http.HandlerFunc(func(conn *http.Conn, req *http.Request) {
		conn.SetHeader("Connection", "keep-alive")
		io.WriteString(conn, "connection test")
	}))
	// for testing the serving of static files
	http.Handle("/static/", http.FileServer("/tmp", "/static"))
	// this process will be a FastCGI responder, accepting connections on stdin
	// so we have to give it the same handlers as the fcgiMux from the test
	if fdlisten, err := fcgi.Listen("exec", ""); err == nil {
		http.Serve(fdlisten, nil)
	}
}
