package main

import (
	"http"
	"http/fcgi"
	"io"
	"flag"
)

var net *string = flag.String("net", "exec", "What type of network to listen on.")
var bind *string = flag.String("b", "", "The address (or path) of the socket to bind to.")

func HelloServer(con *http.Conn, req *http.Request) {
	io.WriteString(con, "hello, world\r\n")
}

func main() {
	flag.Parse()
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
	if listen, err := fcgi.Listen(*net, *bind); err == nil {
		http.Serve(listen, nil)
	}
}