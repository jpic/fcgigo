// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/* This package implements FastCGI for use with the builtin http module.

It provides two main pieces: fcgi.Handler and fcgi.Listener

fcgi.Handler returns a http.Handler that will do round-robin dispatch to remote FastCGI Responders to handle each request.

 - You create one directly: http.Handle("/", fcgi.Handler(...)).  See example for details.

fcgi.Listener is a net.Listener that you can pass to http.Serve to produce a FastCGI Responder.

 - You get one by calling fcgi.Listen(), or fcgi.ListenFD().  See example and tests for details.

Example: You want a create a FastCGI responder that a webserver can connect to via a TCP socket.

	package main

	import (
		"http"
		"http/fcgi"
	)

	func HelloServer(con *http.Conn, req *http.Request) {
		io.WriteString(con, "hello, world!\n")
	}

	func main() {
		http.Handle("/hello", http.HandlerFunc(HelloServer))
		if listen, err := fcgi.Listen("0.0.0.0:7134"); err == nil {
			http.Serve(listen, nil)
		}
	}

Now, if you configured lighttpd to connect to ( "host" => "127.0.0.1", "port" => 7134 ),
It will serve your hello, world application.

Example: You want to create an application that can be spawned to service FastCGI requests.

	package main

	import (
		"http"
		"http/fcgi"
	)

	func main() {
		if listen, err := fcgi.ListenFD(0); err == nil {
			http.Serve(listen, nil)
		}
	}

Example: You have an existing FastCGI application running on a TCP host and want Go's http.Serve to send requests to it for servicing.

	http.Handle("/", fcgi.Handler([]string{
		"127.0.0.1:7134",
		"127.0.0.1:7135",
		// ... repeat for each responder ...
	}))
	http.ListenAndServe(":80", nil)

Example: You want to serve static files, or errors, or anything, immediately, while sending only some other requests to a FastCGI Responder.

	http.Handle("/", fcgi.Handler([]string{
		// ... as above ...
	}))
	http.Handle("/static", http.FileServer(...))
	http.ListenAndServe(":80", nil)

In this example, the http server will use the FileServer to serve files from /static immediately, but all other requests will be sent along to the Responder.

See fcgi_test.go for examples of how to use both the Handler and Listener in the same process.
(hint: they cannot share the same default muxer).

Future Work:

There should be a variant of fcgi.Handler that can use ForkAndExec to dynamically spawn another process,
and connect to it over an FD.  fcgi.ListenFD is in place to provide support for accepting connections this way, would be nice to be able to produce them.
Also, this would allow Go to run PHP applications as FastCGI responders.

There should be variants of Handler and Listener that support net.UnixConn
*/
package fcgi

// fcgi.go defines the raw protocol and some utils
// listener.go defines fcgi.Listener, et al
// handler.go defines fcgi.Handler, et al

import (
	"os"
	"io"
	"net"
	"encoding/binary"
	"bytes"
	"fmt"
	"log"
)

// Log is the logging function used throughout this package.
// By default, it will log nothing.  But, if you set this to log.Stderr, or some other logger that you create, it will use that as well.
var Log = log.Stderr

func dontLog(k string, v ...) {}

const (
	// The fd to use when we are execed by the web-server
	FCGI_LISTENSOCK_FILENO = iota

	// Packet Types (fcgiHeader.Kind)
	FCGI_BEGIN_REQUEST
	FCGI_ABORT_REQUEST
	FCGI_END_REQUEST
	FCGI_PARAMS
	FCGI_STDIN
	FCGI_STDOUT
	FCGI_STDERR
	FCGI_DATA
	FCGI_GET_VALUES
	FCGI_GET_VALUES_RESULT
	FCGI_UNKNOWN_TYPE
	FCGI_MAXTYPE = FCGI_UNKNOWN_TYPE
)

// Keep the connection between web-server and responder open after request
const FCGI_KEEP_CONN = 1

// Max amount of data in a FastCGI record body
const FCGI_MAX_WRITE = 65534

// Roles (fcgiBeginRequest.Roles)
const (
	FCGI_RESPONDER = iota + 1 // only Responders are implemented.
	FCGI_AUTHORIZER
	FCGI_FILTER
)

// ProtocolStatus (in fcgiEndRequest)
const (
	FCGI_REQUEST_COMPLETE = iota
	FCGI_CANT_MPX_CONN
	FCGI_OVERLOADED
	FCGI_UNKNOWN_ROLE
)

type fcgiHeader struct {
	Version       uint8
	Kind          uint8
	ReqId         uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}
type fcgiEndRequest struct {
	AppStatus      uint32
	ProtocolStatus uint8
	Reserved       [3]uint8
}
type fcgiBeginRequest struct {
	Role     uint16
	Flags    uint8
	Reserved [5]uint8
}

func newFcgiHeader(kind uint8, id uint16, content_length int) *fcgiHeader {
	return &fcgiHeader{
		Version: 1,
		Kind: kind,
		ReqId: id,
		ContentLength: uint16(content_length),
		PaddingLength: uint8(-content_length & 7),
	}
}

// this will read ContentLength+PaddingLength bytes, and return the first ContentLength bytes
func (self *fcgiHeader) readContent(r io.Reader) (b []byte, err os.Error) {
	t := int(self.ContentLength) + int(self.PaddingLength)
	b = make([]byte, t)
	if t == 0 {
		return b, nil
	}
	n, err := r.Read(b)
	if n < int(self.ContentLength) {
		return b[0:n], os.NewError(fmt.Sprint("Short read got ", n, "of", t))
	} else if n < t {
		// so we read the content but not the padding, which we _must_ read
		pad := make([]byte, self.PaddingLength)
		if m, err := r.Read(pad); err == nil {
			if m < int(self.PaddingLength) {
				return b[0:n], os.NewError(fmt.Sprint("Short read got ", n, "of", t, " and only", m, "of", self.PaddingLength, "padding"))
			}
		} else {
			return b[0:n], os.NewError(fmt.Sprint("Failed to read padding:", err))
		}
	}
	// Log("readContent{",self.ContentLength,self.PaddingLength,"}: ",b[0:self.ContentLength],b[self.ContentLength:n])
	// discard the padding bytes from the final selection
	return b[0:self.ContentLength], err
}

// so we dont have to allocate new ones all the time, these are always zero, and we write slices of it for padding
var paddingSource = make([]byte, 7) // packets are padded to 8-bytes, so if we are padding more than 7 its wasted
func (self *fcgiHeader) writePadding(w io.Writer) os.Error {
	p := self.PaddingLength
	if p > 0 {
		if p > 7 {
			return os.NewError(fmt.Sprint("fcgiWrite: invalid padding requested:", p, "should be less than 8."))
		}
		pad := paddingSource[0:p]
		// Log("fcgiWrite: Padding", pad)
		_, err := w.Write(pad)
		return err
	}
	return nil
}

// fcgiWrite() writes a single FastCGI record to a Writer
func fcgiWrite(conn io.Writer, kind uint8, reqId uint16, b []byte) (n int, err os.Error) {
	h := newFcgiHeader(kind, reqId, len(b))
	Log("fcgiWrite: Header", conn, h)
	writeStruct(conn, h)
	if len(b) > 0 {
		n, err = conn.Write(b)
		// Log("fcgiWrite: Body", b)
		h.writePadding(conn)
	}
	return n, err
}

// FastCGI has its own pair encoding: <name-len><val-len><name><val>, with a couple kinks.
// but these two functions read a chunk at a time, see parseFcgiParams for usage
func getOneSize(slice []byte) (uint32, []byte) {
	size := slice[0]
	r := 1
	if size>>7 == 1 {
		er := binary.Read(bytes.NewBuffer(slice[0:4]), binary.BigEndian, &size)
		if er != nil {
			Log("binary.Read err:", er)
			return 0, slice[len(slice):len(slice)]
		}
		r = 4
	}
	return uint32(size), slice[r:len(slice)]
}

func getOneValue(slice []byte, size uint32) (string, []byte) {
	if int(size) > len(slice) {
		Log("invalid pair encoding", slice, size)
		return "", slice[0:0]
	}
	return string(slice[0:size]), slice[size:len(slice)]
}

func encodeSize(size int) []byte {
	if size > 127 {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, uint32(size))
		return buf
	}
	buf := make([]byte, 1)
	buf[0] = uint8(size)
	return buf
}

// when the webserver sends us "ACCEPT_ENCODING" as a header,
// (in the FCGI_PARAMS) standardize it like: Accept-Encoding
func standardCase(str []byte) string {
	ret := make([]byte, len(str))
	first := true
	for i := 0; i < len(str); i++ {
		if str[i] == '_' {
			ret[i] = '-'
			first = true
		} else if first {
			ret[i] = str[i]
			first = false
		} else {
			ret[i] = bytes.ToLower(str[i : i+1])[0]
		}
	}
	return string(ret)
}

// write a struct in binary encoding to a writer
func writeStruct(w io.Writer, data interface{}) os.Error {
	err := binary.Write(w, binary.BigEndian, data)
	if err != nil && err != os.EOF {
		Log("WriteTo Error:", err)
	}
	return err
}

// read a struct in binary encoding from a reader
func readStruct(r io.Reader, data interface{}) (err os.Error) {
	err = binary.Read(r, binary.BigEndian, data)
	if err != nil && err != os.EOF {
		Log("ReadFrom Error:", err)
	}
	return err
}

// a reader that supports a dummy Close()
type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() os.Error { return nil }

// a quick wrapper for DialTCP that handles name resolution
func dialTcpAddr(addr string) (conn *net.TCPConn, err os.Error) {
	laddr, _ := net.ResolveTCPAddr("127.0.0.1:0") // our source addr is any available local one
	if raddr, err := net.ResolveTCPAddr(addr); err == nil {
		if sock, err := net.DialTCP("tcp", laddr, raddr); err == nil {
			conn = sock
		}
	}
	return conn, err
}

// a quick wrapper for DialUnix that handles name resolution
func dialUnixAddr(addr string) (conn *net.UnixConn, err os.Error) {
	if raddr, err := net.ResolveUnixAddr("unix", addr); err == nil {
		if sock, err := net.DialUnix("unix", nil, raddr); err == nil {
			conn = sock
		}
	}
	return conn, err
}

// copied from http.request
// this does the atoi conversion at different offsets i in s
// used in parsing the HTTP protocol version
func atoi(s string, i int) (n, i1 int, ok bool) {
	const Big = 1000000
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		return 0, 0, false
	}
	n = 0
	for ; i < len(s) && '0' <= s[i] && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
		if n > Big {
			return 0, 0, false
		}
	}
	return n, i, true
}
