package main

import (
	"http"
	"http/fcgi"
	"io"
)

func HelloServer(con *http.Conn, req *http.Request) {
	io.WriteString(con, "hello, world\n")
}

func main() {
	http.Handle("/", http.HandlerFunc(HelloServer))
	if fdlisten, err := fcgi.Listen("exec", ""); err == nil {
		http.Serve(fdlisten, nil)
	}
}
